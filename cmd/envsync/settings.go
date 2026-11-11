package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/kiyoushii/EnvSync/internal/core"
	"github.com/kiyoushii/EnvSync/internal/modules"
)

const settingsUsage = `envsync settings — синхронизация настроек Windows, оформления и конфигов приложений

  export                  снять состояние этой машины в папку состояний (её синкает Syncthing)
  plan [--only a,b]       dry-run: что изменится на этой машине по состоянию другой (ничего не меняет)
  apply [--only a,b] [--yes]   применить (сначала dry-run и подтверждение, перед изменением делается снимок)
  rollback [id]           откатить последнее применение (или указанное)
  runs                    список применений, которые можно откатить
  journal [-n 30]         журнал аудита: что, когда и откуда менялось

Модули: appearance (тема и акцент), wallpaper (обои), explorer (проводник и панель задач),
regional (форматы даты и времени, повтор клавиш), appconfig (git, VS Code, Windows Terminal, Claude).
`

func (e *env) engine() (*core.Engine, error) {
	cenv, err := core.CurrentEnv()
	if err != nil {
		return nil, err
	}
	return &core.Engine{
		Machine: cenv.Machine,
		State:   core.StateDir{Root: e.s.StateDir()},
		Snaps:   &core.SnapshotStore{Dir: e.s.SnapshotsDir()},
		Journal: &core.Journal{Path: e.s.JournalFile()},
		Modules: modules.Default(cenv, modules.Options{DataDir: e.s.BaseDir(), WallpaperStyle: e.s.WallpaperStyle}),
	}, nil
}

func (e *env) cmdSettings(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Print(settingsUsage)
		return errors.New("не указано действие")
	}
	eng, err := e.engine()
	if err != nil {
		return err
	}
	rest := args[1:]
	switch args[0] {
	case "export":
		changed, err := eng.Export(ctx)
		if len(changed) == 0 {
			fmt.Println("Состояние не изменилось с прошлого раза.")
		} else {
			fmt.Println("Состояние обновлено:", strings.Join(changed, ", "))
		}
		fmt.Println("Папка состояний:", e.s.StateDir())
		return err
	case "plan":
		only, _, err := parseOnly(rest)
		if err != nil {
			return err
		}
		items, err := eng.Plan(ctx, only)
		if err != nil {
			return err
		}
		printPlan(items)
		return nil
	case "apply":
		return e.settingsApply(ctx, eng, rest)
	case "rollback":
		id := ""
		if len(rest) > 0 {
			id = rest[0]
		}
		run, err := eng.Rollback(ctx, id)
		if err != nil {
			return err
		}
		fmt.Printf("Запуск %s откатан: %s\n", run.ID, strings.Join(run.Modules, ", "))
		return nil
	case "runs":
		runs, err := eng.Snaps.ListRuns()
		if err != nil {
			return err
		}
		if len(runs) == 0 {
			fmt.Println("Применений ещё не было.")
		}
		for _, r := range runs {
			status := "применён"
			switch {
			case r.Rolled:
				status = "откатан"
			case !r.Done:
				status = "не завершён"
			}
			fmt.Printf("%s  %-11s %s (источник %s)\n", r.ID, status, strings.Join(r.Modules, ", "), r.Source)
		}
		return nil
	case "journal":
		fs := flag.NewFlagSet("journal", flag.ContinueOnError)
		n := fs.Int("n", 30, "сколько последних записей показать")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		entries, err := eng.Journal.Recent(*n)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			fmt.Println("Журнал пуст.")
		}
		for _, en := range entries {
			fmt.Println(en)
		}
		return nil
	default:
		fmt.Print(settingsUsage)
		return fmt.Errorf("неизвестное действие %q", args[0])
	}
}

func parseOnly(args []string) (only []string, yes bool, err error) {
	fs := flag.NewFlagSet("settings", flag.ContinueOnError)
	list := fs.String("only", "", "только эти модули через запятую")
	y := fs.Bool("yes", false, "не спрашивать подтверждение")
	if err = fs.Parse(args); err != nil {
		return
	}
	if *list != "" {
		only = strings.Split(*list, ",")
	}
	return only, *y, nil
}

func printPlan(items []core.PlanItem) {
	if len(items) == 0 {
		fmt.Println("Менять нечего: настройки этой машины не старше состояния других.")
		return
	}
	restarts := map[string]bool{}
	for _, it := range items {
		fmt.Printf("[%s] источник: %s\n", it.Module, it.Source)
		if it.Err != nil {
			fmt.Printf("  не удалось построить план: %v\n", it.Err)
			continue
		}
		for _, c := range it.Changes {
			fmt.Printf("  • %s\n", c)
			if c.Restart != "" && !c.Skip {
				restarts[c.Restart] = true
			}
		}
	}
	if restarts["explorer"] {
		fmt.Println("Часть изменений вступит в силу после перезапуска проводника (агент его сам не перезапускает).")
	}
	if restarts["logoff"] {
		fmt.Println("Часть изменений вступит в силу после выхода из системы и входа заново.")
	}
}

func (e *env) settingsApply(ctx context.Context, eng *core.Engine, args []string) error {
	only, yes, err := parseOnly(args)
	if err != nil {
		return err
	}
	items, err := eng.Plan(ctx, only)
	if err != nil {
		return err
	}
	printPlan(items)
	work := false
	for _, it := range items {
		work = work || it.HasWork()
	}
	if !work {
		return nil
	}
	fmt.Println("Перед применением сохраняется снимок «до»; откат: envsync settings rollback")
	if !yes && !confirm("Применить? [y/N] ") {
		fmt.Println("Отменено, ничего не изменено.")
		return nil
	}
	run, _, err := eng.Apply(ctx, only)
	if run != nil {
		fmt.Printf("Запуск %s. Откат: envsync settings rollback %s\n", run.ID, run.ID)
	}
	return err
}
