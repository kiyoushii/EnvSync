package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/kiyoushii/EnvSync/internal/core"
	"github.com/kiyoushii/EnvSync/internal/transport"
)

const guardUsage = `envsync guard — предохранитель массового удаления

  status                  папки, поставленные на паузу из-за массового удаления, и действующий порог
  accept <папка>          принять удаление: папка возобновляется, удалённое остаётся в «корзине» (.stversions)
  reject <папка>          отклонить удаление: файлы остаются (удалённые возвращаются из «корзины»),
                          после чего другая машина получит их обратно

Порог задаётся в манифесте: safety.mass_delete (max_files, max_percent, min_files).
`

// massDeleteFromManifest берёт порог из манифеста; нет манифеста — значения по умолчанию.
func (e *env) massDeleteFromManifest() core.MassDeleteDef {
	m, err := core.LoadManifest(e.s.ManifestFile())
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			e.logger.Printf("манифест не прочитан, предохранитель работает с порогом по умолчанию: %v", err)
		}
		return (&core.Manifest{}).MassDelete()
	}
	return m.MassDelete()
}

func (e *env) guard(ctx context.Context) (*transport.Guard, error) {
	key, err := transport.ReadAPIKey(e.s.ConfigXMLPath())
	if err != nil {
		return nil, errors.New("конфига Syncthing нет: выполните envsync init и envsync run")
	}
	cenv, err := core.CurrentEnv()
	if err != nil {
		return nil, err
	}
	return &transport.Guard{
		C:         transport.NewClient(e.s.GUIPort, key),
		Cfg:       e.massDeleteFromManifest(),
		StatePath: e.s.GuardFile(),
		Journal:   &core.Journal{Path: e.s.JournalFile()},
		Machine:   cenv.Machine,
	}, nil
}

func (e *env) cmdGuard(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Print(guardUsage)
		return errors.New("не указано действие")
	}
	g, err := e.guard(ctx)
	if err != nil {
		return err
	}
	switch args[0] {
	case "status":
		fmt.Printf("Порог: больше %d файлов или %.4g%% папки, не меньше %d файлов.\n", g.Cfg.MaxFiles, g.Cfg.MaxPercent, g.Cfg.MinFiles)
		tr, err := g.Tripped()
		if err != nil {
			return err
		}
		if len(tr) == 0 {
			fmt.Println("Папок, ждущих решения, нет.")
		}
		for _, t := range tr {
			fmt.Printf("ПАУЗА: %s с %s: другая машина удаляет %d из %d файлов\n", t.FolderID, t.At.Local().Format("2006-01-02 15:04:05"), t.NeedDeletes, t.GlobalFiles)
			fmt.Printf("  принять: envsync guard accept %s    отклонить: envsync guard reject %s\n", t.FolderID, t.FolderID)
		}
		return nil
	case "accept", "reject":
		if len(args) != 2 {
			return fmt.Errorf("использование: envsync guard %s <папка>", args[0])
		}
		id := args[1]
		if args[0] == "accept" {
			if !confirm(fmt.Sprintf("Принять массовое удаление в папке %q? Файлы останутся в .stversions. [y/N] ", id)) {
				fmt.Println("Отменено, папка остаётся на паузе.")
				return nil
			}
			if err := g.Accept(ctx, id); err != nil {
				return err
			}
			fmt.Println("Принято: папка возобновлена, удаления применяются.")
			return nil
		}
		if !confirm(fmt.Sprintf("Отклонить удаление в папке %q и вернуть файлы? [y/N] ", id)) {
			fmt.Println("Отменено, папка остаётся на паузе.")
			return nil
		}
		res, err := g.Reject(ctx, id)
		if err != nil {
			return err
		}
		fmt.Printf("Отклонено: оставлено %d, возвращено из корзины %d, не найдено %d. Папка возобновлена.\n", res.Kept, res.Restored, res.Missing)
		return nil
	default:
		fmt.Print(guardUsage)
		return fmt.Errorf("неизвестное действие %q", args[0])
	}
}

// cmdPause — «пауза всего»: связь со всеми устройствами приостанавливается.
func (e *env) cmdPause(ctx context.Context, resume bool) error {
	key, err := transport.ReadAPIKey(e.s.ConfigXMLPath())
	if err != nil {
		return errors.New("конфига Syncthing нет")
	}
	c := transport.NewClient(e.s.GUIPort, key)
	cenv, _ := core.CurrentEnv()
	j := &core.Journal{Path: e.s.JournalFile()}
	if resume {
		if err := c.ResumeAll(ctx); err != nil {
			return err
		}
		_ = j.Append(core.Entry{Time: time.Now().UTC(), Machine: cenv.Machine, Module: "guard", Action: "resume-all", Detail: "синхронизация возобновлена человеком"})
		fmt.Println("Синхронизация возобновлена.")
		return nil
	}
	if err := c.PauseAll(ctx); err != nil {
		return err
	}
	_ = j.Append(core.Entry{Time: time.Now().UTC(), Machine: cenv.Machine, Module: "guard", Action: "pause-all", Detail: "синхронизация приостановлена человеком"})
	fmt.Println("Синхронизация приостановлена. Возобновить: envsync resume")
	return nil
}

// cmdRollback — единая команда отката: сама определяет, чей это запуск (настройки или программы).
func (e *env) cmdRollback(ctx context.Context, args []string) error {
	snaps := &core.SnapshotStore{Dir: e.s.SnapshotsDir()}
	id := ""
	if len(args) > 0 {
		id = args[0]
	}
	run, err := snaps.OpenRun(id)
	if err != nil {
		return err
	}
	if run.Rolled {
		return fmt.Errorf("запуск %s уже откатан", run.ID)
	}
	if len(run.Modules) == 1 && run.Modules[0] == "software" {
		mgr, _, err := e.softwareManager()
		if err != nil {
			return err
		}
		res, err := mgr.Rollback(ctx, run.ID, func(p string) bool { return confirm(p + " [y/N] ") })
		if res != nil {
			fmt.Printf("Запуск %s (программы): возвращено %d, удалено %d, не выполнено %d\n", res.RunID, len(res.Installed), len(res.Removed), len(res.Failed))
		}
		return err
	}
	eng, err := e.engine()
	if err != nil {
		return err
	}
	r, err := eng.Rollback(ctx, run.ID)
	if err != nil {
		return err
	}
	fmt.Printf("Запуск %s откатан: %s\n", r.ID, strings.Join(r.Modules, ", "))
	return nil
}

// ackLiveData проверяет подтверждение резервной копии перед первым синком реальных данных.
// Без отметки отказывает и выводит чек-лист; с флагом backupConfirmed ставит отметку.
func (e *env) ackLiveData(liveIDs []string, backupConfirmed bool) error {
	if _, err := os.Stat(e.s.LiveDataAckFile()); err == nil {
		return nil
	}
	if !backupConfirmed {
		return fmt.Errorf("перед первым синком реальных данных (папки: %s) нужна резервная копия.\n"+
			"  1. Сделайте копию важных папок (внешний диск или облако).\n"+
			"  2. Создайте точку восстановления Windows (от администратора: Checkpoint-Computer -Description EnvSync -RestorePointType MODIFY_SETTINGS).\n"+
			"  3. Убедитесь, что envsync guard status показывает порог, а envsync settings runs пуст или понятен.\n"+
			"Когда готово, повторите команду с флагом --backup-confirmed (отметка сохранится)", strings.Join(liveIDs, ", "))
	}
	data, _ := json.MarshalIndent(map[string]any{"confirmedAt": time.Now().UTC(), "folders": liveIDs}, "", "  ")
	return os.WriteFile(e.s.LiveDataAckFile(), data, 0o600)
}
