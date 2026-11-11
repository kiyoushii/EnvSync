package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kiyoushii/EnvSync/internal/core"
	"github.com/kiyoushii/EnvSync/internal/transport"
)

// envFlags — флаги, позволяющие «примерить» манифест на другую машину без неё самой.
type envFlags struct {
	file, machine, user, home *string
}

func addEnvFlags(fs *flag.FlagSet) envFlags {
	return envFlags{
		file:    fs.String("file", "", "путь к манифесту (по умолчанию из settings.json или envsync.yaml в папке агента)"),
		machine: fs.String("machine", "", "раскрыть манифест как на машине с этим именем (проверка, без применения)"),
		user:    fs.String("user", "", "имя пользователя для проверки на другой машине"),
		home:    fs.String("home", "", "профиль пользователя для проверки на другой машине, например D:\\Users\\kiyoushii"),
	}
}

// resolve загружает манифест и раскрывает его для текущей машины с учётом переопределений.
func (f envFlags) resolve(s transport.Settings) (core.Env, []core.ResolvedFolder, error) {
	path := *f.file
	if path == "" {
		path = s.ManifestFile()
	}
	m, err := core.LoadManifest(path)
	if err != nil {
		return core.Env{}, nil, err
	}
	env, err := core.CurrentEnv()
	if err != nil {
		return core.Env{}, nil, err
	}
	if *f.machine != "" {
		env.Machine = strings.ToUpper(*f.machine)
	}
	if *f.user != "" {
		env.User = *f.user
	}
	if *f.home != "" {
		env.Home = *f.home
		env.AppData = filepath.Join(env.Home, "AppData", "Roaming")
		env.LocalAppData = filepath.Join(env.Home, "AppData", "Local")
	}
	folders, err := m.Resolve(env)
	return env, folders, err
}

// cmdManifest: `envsync manifest check` — проверить манифест и показать, как он раскрывается.
// Ничего не меняет и не требует запущенного Syncthing.
func cmdManifest(args []string) error {
	if len(args) == 0 || args[0] != "check" {
		return errors.New("использование: envsync manifest check [--file F] [--machine ИМЯ --user U --home ПУТЬ]")
	}
	fs := flag.NewFlagSet("manifest check", flag.ContinueOnError)
	ef := addEnvFlags(fs)
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	base, err := transport.DefaultBaseDir()
	if err != nil {
		return err
	}
	s, err := transport.LoadSettings(base)
	if err != nil {
		return err
	}
	env, folders, err := ef.resolve(s)
	if err != nil {
		return err
	}
	fmt.Print(core.Describe(env, folders))
	fmt.Println("Манифест корректен.")
	return nil
}

// cmdApply: `envsync apply` — привести Syncthing в соответствие с манифестом (dry-run и подтверждение).
func (e *env) cmdApply(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "не спрашивать подтверждение")
	file := fs.String("file", "", "путь к манифесту")
	backup := fs.Bool("backup-confirmed", false, "подтверждаю, что сделал резервную копию и точку восстановления (нужно один раз перед синком реальных данных)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ef := envFlags{file: file, machine: new(string), user: new(string), home: new(string)}
	menv, folders, err := ef.resolve(e.s)
	if err != nil {
		return err
	}

	// Защита реальных данных: до этапа 6 синкаются только папки, помеченные test: true.
	if !e.s.AllowLiveData {
		var live []string
		for _, f := range folders {
			if !f.Test {
				live = append(live, f.ID)
			}
		}
		if len(live) > 0 {
			return fmt.Errorf("синк реальных данных выключен (allowLiveData=false), а в манифесте есть не тестовые папки: %s. "+
				"Это решение владельца: включите \"allowLiveData\": true в settings.json, когда будете готовы "+
				"(предохранитель массового удаления и откат уже работают, см. envsync guard status)", strings.Join(live, ", "))
		}
	} else {
		var live []string
		for _, f := range folders {
			if !f.Test {
				live = append(live, f.ID)
			}
		}
		if len(live) > 0 {
			if err := e.ackLiveData(live, *backup); err != nil {
				return err
			}
		}
	}

	key, err := transport.ReadAPIKey(e.s.ConfigXMLPath())
	if err != nil {
		return errors.New("конфига Syncthing нет: выполните envsync init и envsync run")
	}
	c := transport.NewClient(e.s.GUIPort, key)
	selfID, err := c.MyID(ctx)
	if err != nil {
		return errors.New("Syncthing не запущен: запустите envsync run в другом окне")
	}
	peers, err := c.DeviceIDs(ctx, selfID)
	if err != nil {
		return err
	}
	if len(peers) == 0 {
		return errors.New("в Syncthing нет второго устройства: сначала выполните envsync pair")
	}

	fmt.Print(core.Describe(menv, folders))
	fmt.Printf("Будет открыто для устройств: %s\n", strings.Join(peers, ", "))
	fmt.Println("Папки создаются, если их нет; существующие приводятся к манифесту (путь существующей папки не меняется).")
	if !*yes && !confirm("Применить? [y/N] ") {
		fmt.Println("Отменено, ничего не изменено.")
		return nil
	}
	for _, f := range folders {
		if err := transport.ApplyFolder(ctx, c, selfID, peers, transport.FolderSpecFrom(f, e.s)); err != nil {
			return err
		}
		fmt.Printf("  %s: готово\n", f.ID)
	}
	// Служебная папка состояний нужна для обмена настройками; обеспечиваем её тем же вызовом.
	if err := transport.ApplyFolder(ctx, c, selfID, peers, transport.StateFolderSpec(e.s.StateDir(), e.s.WatcherDelayS, e.s.MaxConflicts)); err != nil {
		return err
	}
	fmt.Printf("  %s: готово\n", transport.StateFolderID)
	return nil
}
