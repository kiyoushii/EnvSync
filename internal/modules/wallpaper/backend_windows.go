//go:build windows

package wallpaper

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const desktopKey = `Control Panel\Desktop`

// RealBackend работает с настоящими обоями текущего пользователя.
type RealBackend struct{}

func (RealBackend) Get() (Setting, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, desktopKey, registry.QUERY_VALUE)
	if err != nil {
		return Setting{}, err
	}
	defer k.Close()
	read := func(name string) string {
		s, _, err := k.GetStringValue(name)
		if err != nil {
			return ""
		}
		return s
	}
	return Setting{Path: read("WallPaper"), Style: read("WallpaperStyle"), Tile: read("TileWallpaper")}, nil
}

// Set записывает режим подгонки в реестр, затем просит систему сменить обои (SPI_SETDESKWALLPAPER).
func (RealBackend) Set(s Setting) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, desktopKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	if s.Style != "" {
		if err := k.SetStringValue("WallpaperStyle", s.Style); err != nil {
			k.Close()
			return err
		}
	}
	if s.Tile != "" {
		if err := k.SetStringValue("TileWallpaper", s.Tile); err != nil {
			k.Close()
			return err
		}
	}
	k.Close()

	const (
		spiSetDeskWallpaper = 0x0014
		spifUpdateIniFile   = 0x01
		spifSendChange      = 0x02
	)
	path, err := windows.UTF16PtrFromString(s.Path)
	if err != nil {
		return err
	}
	spi := windows.NewLazySystemDLL("user32.dll").NewProc("SystemParametersInfoW")
	r, _, callErr := spi.Call(spiSetDeskWallpaper, 0, uintptr(unsafe.Pointer(path)), spifUpdateIniFile|spifSendChange)
	if r == 0 {
		if callErr != nil && !errors.Is(callErr, windows.ERROR_SUCCESS) {
			return fmt.Errorf("SystemParametersInfo: %w", callErr)
		}
		return errors.New("SystemParametersInfo не удалась")
	}
	return nil
}
