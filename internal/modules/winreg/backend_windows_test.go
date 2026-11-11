//go:build windows

package winreg

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"golang.org/x/sys/windows/registry"
)

func timeNow() time.Time { return time.Now() }

// Проверка настоящего реестра на собственном тестовом ключе. Ключ создаётся и удаляется самим тестом
// в HKCU\Software\EnvSyncTest, реальные настройки пользователя не затрагиваются.
func TestRealBackendOnPrivateTestKey(t *testing.T) {
	root := fmt.Sprintf(`Software\EnvSyncTest\run%d`, time.Now().UnixNano())
	t.Cleanup(func() {
		_ = registry.DeleteKey(registry.CURRENT_USER, root)
		_ = registry.DeleteKey(registry.CURRENT_USER, `Software\EnvSyncTest`)
	})
	be := RealBackend{}

	// Отсутствие ключа и значения — не ошибка.
	if v, err := be.Get(root, "Nope"); err != nil || v != nil {
		t.Fatalf("v=%v err=%v", v, err)
	}
	if err := be.Delete(root, "Nope"); err != nil {
		t.Fatal(err)
	}

	for _, v := range []Value{
		{Type: "dword", Data: "4292114432"}, // значение выше int32, как AccentColor
		{Type: "string", Data: "C:\\WINDOWS\\web\\wallpaper\\Windows\\img19.jpg"},
		{Type: "expand", Data: "%SystemRoot%\\x"},
		{Type: "binary", Data: "99ebff00"},
	} {
		name := "v_" + v.Type
		if err := be.Set(root, name, v); err != nil {
			t.Fatalf("Set %s: %v", v.Type, err)
		}
		got, err := be.Get(root, name)
		if err != nil || got == nil || *got != v {
			t.Fatalf("%s: записано %v, прочитано %v err=%v", v.Type, v, got, err)
		}
	}

	if err := be.Set(root, "x", Value{Type: "dword", Data: "-1"}); err == nil {
		t.Error("отрицательный dword должен отклоняться")
	}
	if err := be.Delete(root, "v_dword"); err != nil {
		t.Fatal(err)
	}
	if v, _ := be.Get(root, "v_dword"); v != nil {
		t.Fatalf("значение не удалено: %v", v)
	}
}

// Весь цикл модуля на настоящем реестре: применил, проверил, откатил, на тестовых ключах.
func TestRealBackendFullCycle(t *testing.T) {
	root := fmt.Sprintf(`Software\EnvSyncTest\cycle%d`, time.Now().UnixNano())
	t.Cleanup(func() {
		_ = registry.DeleteKey(registry.CURRENT_USER, root)
		_ = registry.DeleteKey(registry.CURRENT_USER, `Software\EnvSyncTest`)
	})
	specs := []Spec{
		{ID: "t.dword", Key: root, Name: "Num", Type: "dword"},
		{ID: "t.string", Key: root, Name: "Str", Type: "string"},
		{ID: "t.absent", Key: root, Name: "Gone", Type: "dword"},
	}
	be := RealBackend{}
	_ = be.Set(root, "Num", Value{Type: "dword", Data: "1"})
	_ = be.Set(root, "Gone", Value{Type: "dword", Data: "9"})
	m := New("cycle", specs, be, nil)
	ctx := context.Background()
	snap := newSnap(t)

	want, _ := json.Marshal(state{Values: map[string]*Value{
		"t.dword": {Type: "dword", Data: "2"}, "t.string": {Type: "string", Data: "привет"}, "t.absent": nil,
	}})
	if err := m.Apply(ctx, want, "", snap); err != nil {
		t.Fatal(err)
	}
	if changes, _ := m.Plan(ctx, want, ""); len(changes) != 0 {
		t.Fatalf("план после применения не пуст: %v", changes)
	}
	if err := m.Rollback(ctx, snap); err != nil {
		t.Fatal(err)
	}
	if v, _ := be.Get(root, "Num"); v == nil || v.Data != "1" {
		t.Errorf("Num: %v", v)
	}
	if v, _ := be.Get(root, "Str"); v != nil {
		t.Errorf("Str должен быть удалён: %v", v)
	}
	if v, _ := be.Get(root, "Gone"); v == nil || v.Data != "9" {
		t.Errorf("Gone должен быть восстановлен: %v", v)
	}
}
