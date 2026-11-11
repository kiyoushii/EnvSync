package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"sort"
	"strings"

	"github.com/kiyoushii/EnvSync/internal/core"
	"github.com/kiyoushii/EnvSync/internal/modules/software"
)

const softwareUsage = `envsync software — программы через winget

  scan                    записать список установленных пакетов этой машины в папку состояний
  plan [--file F]         dry-run: что поставить по манифесту, что предложить добавить в манифест (ничего не меняет)
  apply [--yes] [--uninstall]   поставить недостающее (после подтверждения); --uninstall разрешает удалить
                          пакеты из software.remove, каждое с отдельным подтверждением
  rollback [id]           отменить применение: удалить поставленное им (с подтверждением) и вернуть удалённое
`

func (e *env) softwareManager() (*software.Manager, core.Env, error) {
	cenv, err := core.CurrentEnv()
	if err != nil {
		return nil, cenv, err
	}
	return &software.Manager{
		W:       software.RealWinget{},
		Machine: cenv.Machine,
		State:   core.StateDir{Root: e.s.StateDir()},
		Snaps:   &core.SnapshotStore{Dir: e.s.SnapshotsDir()},
		Journal: &core.Journal{Path: e.s.JournalFile()},
	}, cenv, nil
}

func (e *env) cmdSoftware(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Print(softwareUsage)
		return errors.New("не указано действие")
	}
	mgr, cenv, err := e.softwareManager()
	if err != nil {
		return err
	}
	rest := args[1:]
	switch args[0] {
	case "scan":
		installed, err := mgr.Scan(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("Установлено пакетов winget: %d. Список записан в %s\n", len(installed), e.s.StateDir())
		return nil

	case "plan", "apply":
		fs := flag.NewFlagSet("software "+args[0], flag.ContinueOnError)
		file := fs.String("file", "", "путь к манифесту")
		yes := fs.Bool("yes", false, "не спрашивать подтверждение")
		uninstall := fs.Bool("uninstall", false, "разрешить удаление пакетов из software.remove (с подтверждением каждого)")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		path := *file
		if path == "" {
			path = e.s.ManifestFile()
		}
		m, err := core.LoadManifest(path)
		if err != nil {
			return err
		}
		plan, err := mgr.Plan(ctx, m.ResolveSoftware(cenv))
		if err != nil {
			return err
		}
		printSoftwarePlan(plan)
		if args[0] == "plan" || !plan.HasWork() {
			return nil
		}

		fmt.Println("\nУстановка принимает лицензионные соглашения устанавливаемых пакетов (--accept-package-agreements).")
		fmt.Println("Часть установщиков запросит подтверждение Windows (UAC).")
		if !*yes && !confirm("Поставить перечисленное? [y/N] ") {
			fmt.Println("Отменено, ничего не изменено.")
			return nil
		}
		res, err := mgr.Apply(ctx, plan, software.ApplyOptions{
			Uninstall: *uninstall,
			Confirm:   func(p string) bool { return confirm(p + " [y/N] ") },
		})
		if res != nil && res.RunID != "" {
			fmt.Printf("\nЗапуск %s. Установлено: %d, удалено: %d, ошибок: %d. Откат: envsync software rollback %s\n",
				res.RunID, len(res.Installed), len(res.Removed), len(res.Failed), res.RunID)
			for id, ferr := range res.Failed {
				fmt.Printf("  не удалось %s: %v\n", id, ferr)
			}
		}
		return err

	case "rollback":
		id := ""
		if len(rest) > 0 {
			id = rest[0]
		}
		res, err := mgr.Rollback(ctx, id, func(p string) bool { return confirm(p + " [y/N] ") })
		if res != nil {
			fmt.Printf("Запуск %s: возвращено %d, удалено %d, не выполнено %d\n", res.RunID, len(res.Installed), len(res.Removed), len(res.Failed))
		}
		return err

	default:
		fmt.Print(softwareUsage)
		return fmt.Errorf("неизвестное действие %q", args[0])
	}
}

func printSoftwarePlan(p *software.Plan) {
	fmt.Printf("Уже установлено из манифеста: %d\n", p.AlreadyOK)
	if len(p.Install) == 0 {
		fmt.Println("Ставить нечего.")
	} else {
		fmt.Println("Будет установлено:")
		for _, w := range p.Install {
			extra := ""
			if w.Version != "" {
				extra += " версия " + w.Version
			}
			if w.Scope != "" {
				extra += " (" + w.Scope + ")"
			}
			fmt.Printf("  + %s%s\n", w.ID, extra)
		}
	}
	if len(p.Remove) > 0 {
		fmt.Println("Помечено на удаление в манифесте (удалится только с --uninstall и вашим подтверждением по каждому):")
		for _, id := range p.Remove {
			fmt.Printf("  - %s\n", id)
		}
	}
	for _, mm := range p.Mismatch {
		fmt.Println("  версия отличается:", mm)
	}
	if len(p.Manual) > 0 {
		fmt.Println("Ставятся вручную (нет в winget):")
		for _, it := range p.Manual {
			fmt.Printf("  • %s %s\n", it.Name, it.Note)
		}
	}
	if len(p.Suggest) > 0 {
		fmt.Printf("\nЕсть на машинах, но нет в манифесте (%d). Чтобы синкать, добавьте в software.winget:\n", len(p.Suggest))
		sort.SliceStable(p.Suggest, func(i, j int) bool { return len(p.Suggest[i].Where) > len(p.Suggest[j].Where) })
		const limit = 40
		for i, s := range p.Suggest {
			if i == limit {
				fmt.Printf("    … и ещё %d\n", len(p.Suggest)-limit)
				break
			}
			fmt.Printf("    - id: %-45s # на: %s\n", s.ID, strings.Join(s.Where, ", "))
		}
	}
}
