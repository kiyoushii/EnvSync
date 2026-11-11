package transport

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/kiyoushii/EnvSync/internal/core"
)

// Agent запускает Syncthing как дочерний процесс и следит, чтобы он работал
// только пока Tailscale подключён и его адрес не менялся.
type Agent struct {
	S   Settings
	TS  *Tailscale
	Log *log.Logger

	// Предохранитель массового удаления работает, пока работает Syncthing.
	MassDelete core.MassDeleteDef // порог из манифеста; нули = значения по умолчанию
	Journal    *core.Journal
	Machine    string
	// Guard — общий экземпляр предохранителя (его же использует интерфейс, чтобы запись состояния не гонялась).
	// nil = агент создаст свой.
	Guard *Guard

	// Интервалы вынесены в поля, чтобы их можно было сократить в тестах.
	TailnetPollEvery time.Duration // как часто проверять Tailscale во время работы
	StopTimeout      time.Duration // сколько ждать корректного выхода до принудительного
}

func (a *Agent) pollEvery() time.Duration {
	if a.TailnetPollEvery > 0 {
		return a.TailnetPollEvery
	}
	return 15 * time.Second
}

func (a *Agent) stopTimeout() time.Duration {
	if a.StopTimeout > 0 {
		return a.StopTimeout
	}
	return 10 * time.Second
}

// Init генерирует ключи и config.xml Syncthing, если их ещё нет. Возвращает Device ID.
func (a *Agent) Init(ctx context.Context, bin string) (string, error) {
	home := a.S.SyncthingHome()
	if _, err := os.Stat(a.S.ConfigXMLPath()); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(home, 0o700); err != nil {
			return "", err
		}
		a.Log.Printf("создаю ключи и конфиг Syncthing в %s", home)
		out, err := exec.CommandContext(ctx, bin, "generate", "--home", home, "--no-port-probing").CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("syncthing generate: %w: %s", err, out)
		}
	}
	return DeviceID(ctx, bin, home)
}

// DeviceID спрашивает ID у `syncthing device-id`. Запущенный Syncthing для этого не нужен.
func DeviceID(ctx context.Context, bin, home string) (string, error) {
	out, err := exec.CommandContext(ctx, bin, "device-id", "--home", home).Output()
	if err != nil {
		return "", fmt.Errorf("syncthing device-id: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Run работает, пока не отменён ctx. Цикл такой:
// дождаться Tailscale → записать жёсткий конфиг с текущим адресом → запустить Syncthing →
// следить за Tailscale → при потере сети остановить Syncthing и начать заново.
func (a *Agent) Run(ctx context.Context) error {
	bin, err := FindSyncthing(a.S)
	if err != nil {
		return err
	}
	if _, err := a.Init(ctx, bin); err != nil {
		return err
	}

	backoff := time.Second
	for ctx.Err() == nil {
		ip, err := a.waitForTailscale(ctx)
		if err != nil {
			return nil // отмена
		}
		started := time.Now()
		err = a.runOnce(ctx, bin, ip)
		if ctx.Err() != nil {
			return nil
		}
		a.Log.Printf("Syncthing остановлен: %v", err)

		// Если процесс жил долго, считаем перезапуск плановым; если падает сразу, растим паузу.
		if time.Since(started) > time.Minute {
			backoff = time.Second
		} else if backoff < time.Minute {
			backoff *= 2
		}
		a.Log.Printf("перезапуск через %v", backoff)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
	}
	return nil
}

// waitForTailscale блокируется, пока Tailscale не будет готов, и возвращает наш адрес.
// Пока сети нет, Syncthing не запускается вообще: это и есть защита от «слушать везде».
func (a *Agent) waitForTailscale(ctx context.Context) (netip.Addr, error) {
	logged := false
	for {
		st, err := a.TS.Status(ctx)
		if err == nil && st.Running() {
			return st.Self.IPv4, nil
		}
		if !logged {
			a.Log.Printf("жду Tailscale (%s)", describeTSProblem(st, err))
			logged = true
		}
		select {
		case <-ctx.Done():
			return netip.Addr{}, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func describeTSProblem(st *TSStatus, err error) string {
	if err != nil {
		return err.Error()
	}
	return "состояние " + st.BackendState
}

var errTailnetLost = errors.New("Tailscale недоступен или сменился адрес")

// runOnce запускает Syncthing на конкретном адресе и ждёт его завершения.
func (a *Agent) runOnce(ctx context.Context, bin string, ip netip.Addr) error {
	// Syncthing сейчас не запущен, поэтому можно безопасно переписать config.xml.
	if err := HardenConfigFile(a.S.ConfigXMLPath(), HardenParams{ListenIP: ip, SyncPort: a.S.SyncPort, GUIPort: a.S.GUIPort}); err != nil {
		return fmt.Errorf("настройка конфига: %w", err)
	}
	if bad, err := AuditConfigFile(a.S.ConfigXMLPath()); err != nil || len(bad) > 0 {
		return fmt.Errorf("конфиг не прошёл проверку, Syncthing не запущен: %v %v", bad, err)
	}
	key, err := ReadAPIKey(a.S.ConfigXMLPath())
	if err != nil {
		return err
	}
	client := NewClient(a.S.GUIPort, key)

	cmd := exec.Command(bin, "serve",
		"--home", a.S.SyncthingHome(),
		"--no-browser",
		"--no-upgrade",      // обновления только через winget
		"--no-restart",      // перезапуском занимается агент, а не сам Syncthing
		"--no-port-probing", // порты только те, что мы задали
	)
	// Вывод Syncthing пишет в свой лог-файл в папке данных (так по умолчанию на Windows).
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("запуск Syncthing: %w", err)
	}
	a.Log.Printf("Syncthing запущен (pid %d), слушает %v:%d, GUI 127.0.0.1:%d", cmd.Process.Pid, ip, a.S.SyncPort, a.S.GUIPort)

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	// Предохранитель массового удаления: следит за папками, пока этот Syncthing жив.
	guardCtx, stopGuard := context.WithCancel(ctx)
	defer stopGuard()
	go func() {
		if err := client.WaitReady(guardCtx, 60*time.Second); err != nil {
			return
		}
		g := a.Guard
		if g == nil {
			cfg := a.MassDelete
			if cfg == (core.MassDeleteDef{}) {
				cfg = (&core.Manifest{}).MassDelete()
			}
			g = &Guard{C: client, Cfg: cfg, StatePath: a.S.GuardFile(), Journal: a.Journal, Machine: a.Machine, Log: a.Log}
		}
		a.Log.Printf("предохранитель массового удаления включён: больше %d файлов или %.4g%% папки (не меньше %d файлов)", g.Cfg.MaxFiles, g.Cfg.MaxPercent, g.Cfg.MinFiles)
		g.Run(guardCtx)
	}()

	tick := time.NewTicker(a.pollEvery())
	defer tick.Stop()
	failures := 0
	for {
		select {
		case err := <-done:
			return err
		case <-ctx.Done():
			a.stop(cmd, done, client)
			return ctx.Err()
		case <-tick.C:
			st, err := a.TS.Status(ctx)
			switch {
			case err == nil && st.Running() && st.Self.IPv4 == ip:
				failures = 0
			case err != nil:
				// Один сбой CLI не повод глушить синк; три подряд — повод.
				if failures++; failures < 3 {
					continue
				}
				fallthrough
			default:
				a.Log.Printf("останавливаю Syncthing: %s", describeTSProblem(st, err))
				a.stop(cmd, done, client)
				return errTailnetLost
			}
		}
	}
}

// stop сначала просит Syncthing выйти сам (чтобы он закрыл базу), потом убивает по таймауту.
func (a *Agent) stop(cmd *exec.Cmd, done <-chan error, c *Client) {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = c.Shutdown(shutdownCtx)
	select {
	case <-done:
	case <-time.After(a.stopTimeout()):
		a.Log.Printf("Syncthing не вышел за %v, завершаю принудительно", a.stopTimeout())
		_ = cmd.Process.Kill()
		<-done
	}
}
