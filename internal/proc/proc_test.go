package proc

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestCommandHasNoConsoleWindow(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("только Windows")
	}
	cmd := Command(context.Background(), "cmd", "/c", "echo", "ok")
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW == 0 || !cmd.SysProcAttr.HideWindow {
		t.Fatalf("у дочернего процесса будет окно: %+v", cmd.SysProcAttr)
	}
	out, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(out)) != "ok" {
		t.Fatalf("вывод без окна должен читаться: %q err=%v", out, err)
	}
}

// Всё, что запускает работающий в фоне агент, должно идти через proc.Command. Прямой exec.Command допустим
// только для команд, которые человек запускает сам в своём терминале (их вывод он видит).
func TestAgentCodeDoesNotSpawnConsoleWindows(t *testing.T) {
	allowed := map[string]string{
		"cmd/envsync/install.go":              "version и update запускаются человеком в его терминале",
		"cmd/envsync/tray_windows.go":         "rundll32 — графическая программа без консоли",
		"internal/modules/software/winget.go": "установка программ запускается человеком (envsync software apply), ход установки нужен в его консоли",
	}
	root := filepath.Join("..", "..")
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, "internal/proc/") {
			return nil
		}
		data, _ := os.ReadFile(p)
		if strings.Contains(string(data), "exec.Command") {
			if _, ok := allowed[rel]; !ok {
				t.Errorf("%s запускает процесс напрямую через exec.Command: в фоновом агенте это мигающее окно консоли; используйте proc.Command", rel)
			}
		}
		return nil
	})
}
