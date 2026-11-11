//go:build windows

package winreg

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// RealBackend читает и пишет настоящий реестр текущего пользователя (HKCU). Права администратора не нужны.
type RealBackend struct{}

func (RealBackend) Get(key, name string) (*Value, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, key, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer k.Close()

	_, typ, err := k.GetValue(name, nil)
	if errors.Is(err, registry.ErrNotExist) {
		return nil, nil
	}
	if err != nil && !errors.Is(err, registry.ErrShortBuffer) {
		return nil, err
	}
	switch typ {
	case registry.DWORD, registry.QWORD:
		n, _, err := k.GetIntegerValue(name)
		if err != nil {
			return nil, err
		}
		return &Value{Type: "dword", Data: strconv.FormatUint(n, 10)}, nil
	case registry.SZ:
		s, _, err := k.GetStringValue(name)
		return &Value{Type: "string", Data: s}, err
	case registry.EXPAND_SZ:
		s, _, err := k.GetStringValue(name)
		return &Value{Type: "expand", Data: s}, err
	case registry.BINARY:
		b, _, err := k.GetBinaryValue(name)
		return &Value{Type: "binary", Data: hex.EncodeToString(b)}, err
	default:
		return nil, fmt.Errorf("неподдерживаемый тип значения реестра %d", typ)
	}
}

func (RealBackend) Set(key, name string, v Value) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, key, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	switch v.Type {
	case "dword":
		n, err := ParseUint32(v.Data)
		if err != nil {
			return fmt.Errorf("dword %q: %w", v.Data, err)
		}
		return k.SetDWordValue(name, n)
	case "string":
		return k.SetStringValue(name, v.Data)
	case "expand":
		return k.SetExpandStringValue(name, v.Data)
	case "binary":
		b, err := hex.DecodeString(v.Data)
		if err != nil {
			return fmt.Errorf("binary: %w", err)
		}
		return k.SetBinaryValue(name, b)
	default:
		return fmt.Errorf("неизвестный тип %q", v.Type)
	}
}

func (RealBackend) Delete(key, name string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, key, registry.SET_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer k.Close()
	if err := k.DeleteValue(name); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}

// ThemeNotifier рассылает системе WM_SETTINGCHANGE("ImmersiveColorSet"), чтобы открытые окна
// подхватили смену темы и акцентного цвета без выхода из системы.
type ThemeNotifier struct{}

func (ThemeNotifier) Notify(context.Context) error {
	user32 := windows.NewLazySystemDLL("user32.dll")
	send := user32.NewProc("SendMessageTimeoutW")
	param, err := windows.UTF16PtrFromString("ImmersiveColorSet")
	if err != nil {
		return err
	}
	const (
		hwndBroadcast   = 0xFFFF
		wmSettingChange = 0x001A
		smtoAbortIfHung = 0x0002
	)
	var result uintptr
	r, _, callErr := send.Call(hwndBroadcast, wmSettingChange, 0, uintptr(unsafe.Pointer(param)), smtoAbortIfHung, 3000, uintptr(unsafe.Pointer(&result)))
	if r == 0 && callErr != windows.ERROR_SUCCESS {
		// Не критично: настройки уже записаны, окна подхватят их позже.
		return nil
	}
	return nil
}
