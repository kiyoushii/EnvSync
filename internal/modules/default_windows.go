//go:build windows

// Пакет modules собирает набор модулей настроек для этой машины.
package modules

import (
	"os"
	"path/filepath"

	"github.com/kiyoushii/EnvSync/internal/core"
	"github.com/kiyoushii/EnvSync/internal/modules/appconfig"
	"github.com/kiyoushii/EnvSync/internal/modules/wallpaper"
	"github.com/kiyoushii/EnvSync/internal/modules/winreg"
)

// Options — настройки сборки набора модулей.
type Options struct {
	DataDir        string // постоянная папка агента (например, для принятых обоев)
	WallpaperStyle string // режим подгонки обоев этой машины; пусто = как у источника
}

// Default возвращает модули для работы с настоящей системой текущего пользователя.
func Default(env core.Env, o Options) []core.Module {
	wp := wallpaper.New(wallpaper.RealBackend{}, filepath.Join(o.DataDir, "wallpapers"), os.Getenv("SystemRoot"))
	wp.StyleOverride = o.WallpaperStyle

	roots := map[appconfig.Root]string{
		appconfig.Home:         env.Home,
		appconfig.AppData:      env.AppData,
		appconfig.LocalAppData: env.LocalAppData,
	}
	return []core.Module{
		winreg.New("appearance", winreg.AppearanceSpecs, winreg.RealBackend{}, winreg.ThemeNotifier{}),
		wp,
		winreg.New("explorer", winreg.ExplorerSpecs, winreg.RealBackend{}, nil),
		winreg.New("regional", winreg.RegionalSpecs, winreg.RealBackend{}, nil),
		appconfig.New(appconfig.DefaultApps(), roots, appconfig.RunningProcesses),
	}
}
