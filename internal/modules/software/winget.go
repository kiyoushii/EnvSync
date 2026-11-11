package software

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/kiyoushii/EnvSync/internal/core"
	"github.com/kiyoushii/EnvSync/internal/proc"
)

// RealWinget вызывает настоящий winget.exe.
type RealWinget struct {
	Bin    string    // пусто = winget из PATH
	Output io.Writer // куда показывать ход установки; nil = os.Stdout
}

func (w RealWinget) bin() string {
	if w.Bin != "" {
		return w.Bin
	}
	return "winget"
}

func (w RealWinget) out() io.Writer {
	if w.Output != nil {
		return w.Output
	}
	return os.Stdout
}

// ParseExport разбирает JSON из `winget export --include-versions`.
func ParseExport(data []byte) (map[string]Package, error) {
	var ex struct {
		Sources []struct {
			Packages []struct {
				PackageIdentifier string
				Version           string
			}
		}
	}
	if err := json.Unmarshal(data, &ex); err != nil {
		return nil, fmt.Errorf("разбор вывода winget export: %w", err)
	}
	out := map[string]Package{}
	for _, s := range ex.Sources {
		for _, p := range s.Packages {
			if p.PackageIdentifier != "" {
				out[strings.ToLower(p.PackageIdentifier)] = Package{ID: p.PackageIdentifier, Version: p.Version}
			}
		}
	}
	return out, nil
}

// Installed спрашивает у winget список установленных пакетов из источника winget.
func (w RealWinget) Installed(ctx context.Context) (map[string]Package, error) {
	dir, err := os.MkdirTemp("", "envsync-winget-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	file := filepath.Join(dir, "export.json")
	// Список программ агент снимает в фоне раз в час: без окна консоли.
	cmd := proc.Command(ctx, w.bin(), "export", "-o", file, "--source", "winget", "--include-versions", "--accept-source-agreements")
	// winget export возвращает ненулевой код, если часть установленных программ не найдена в источниках;
	// главное — файл. Поэтому код возврата смотрим только когда файла нет.
	out, runErr := cmd.CombinedOutput()
	data, readErr := os.ReadFile(file)
	if readErr != nil {
		return nil, fmt.Errorf("winget export не создал файл: %v: %s", runErr, strings.TrimSpace(string(out)))
	}
	return ParseExport(data)
}

func (w RealWinget) run(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, w.bin(), args...)
	cmd.Stdout, cmd.Stderr = w.out(), w.out()
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("winget %s: %w", strings.Join(args[:min(len(args), 3)], " "), err)
	}
	return nil
}

// Install ставит пакет без вопросов от установщика. Принятие лицензионных соглашений пакета входит в подтверждение
// человека перед применением (агент заранее об этом предупреждает).
func (w RealWinget) Install(ctx context.Context, p core.WingetPkg) error {
	args := []string{"install", "--id", p.ID, "--exact", "--source", "winget", "--silent",
		"--accept-package-agreements", "--accept-source-agreements"}
	if p.Version != "" {
		args = append(args, "--version", p.Version)
	}
	if p.Scope != "" {
		args = append(args, "--scope", p.Scope)
	}
	return w.run(ctx, args...)
}

// Uninstall удаляет пакет.
func (w RealWinget) Uninstall(ctx context.Context, id string) error {
	return w.run(ctx, "uninstall", "--id", id, "--exact", "--source", "winget", "--silent")
}
