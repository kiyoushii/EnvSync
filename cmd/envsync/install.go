package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"

	"github.com/kiyoushii/EnvSync/internal/core"
	"github.com/kiyoushii/EnvSync/internal/install"
	"github.com/kiyoushii/EnvSync/internal/transport"
)

// installedExe — постоянное место программы: оттуда её запускает планировщик.
func (e *env) installedExe() string { return filepath.Join(e.s.BaseDir(), "bin", "envsync.exe") }

func samePath(a, b string) bool { return strings.EqualFold(filepath.Clean(a), filepath.Clean(b)) }

// cmdInstall копирует программу в постоянную папку и включает автозапуск при входе пользователя.
func (e *env) cmdInstall(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "не спрашивать подтверждение")
	start := fs.Bool("start", false, "сразу запустить агента в фоне (без вопроса)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	target := e.installedExe()
	user, err := install.CurrentUser()
	if err != nil {
		return err
	}
	needCopy := !samePath(self, target)

	fmt.Println("Будет сделано:")
	if needCopy {
		fmt.Printf("  1. Скопировать %s в %s\n", self, target)
	}
	fmt.Printf("  2. Создать задачу планировщика %q: при входе пользователя %s запускать\n     %s run --background\n", install.DefaultTaskName, user, target)
	fmt.Println("     (обычные права, без администратора; перезапуск при сбое; агент пишет лог в agent.log в папке агента)")
	fmt.Println("Удалить автозапуск: envsync uninstall")
	if !*yes && !confirm("Продолжить? [y/N] ") {
		fmt.Println("Отменено, ничего не изменено.")
		return nil
	}
	if needCopy {
		if err := core.CopyFileAtomic(self, target); err != nil {
			return fmt.Errorf("не удалось скопировать программу (возможно, агент уже запущен из этой папки: закройте его через значок в трее): %w", err)
		}
	}
	if err := (install.Task{Name: install.DefaultTaskName, Exe: target, Args: "run --background", User: user}).Create(ctx); err != nil {
		return err
	}
	fmt.Println("Автозапуск включён. Агент стартует при следующем входе в систему.")
	if *start || (!*yes && confirm("Запустить агента сейчас? [y/N] ")) {
		cmd := exec.Command(target, "run", "--background")
		if err := cmd.Start(); err != nil {
			return err
		}
		fmt.Println("Агент запущен в фоне (значок в трее, интерфейс: envsync ui).")
	}
	return nil
}

// cmdUninstall убирает автозапуск (агент и данные остаются).
func (e *env) cmdUninstall(ctx context.Context) error {
	if err := install.Delete(ctx, install.DefaultTaskName); err != nil {
		return err
	}
	fmt.Println("Автозапуск удалён. Запущенный агент не остановлен (выход: значок в трее). Данные и настройки не тронуты.")
	return nil
}

// cmdVersion показывает версии агента и его зависимостей.
func (e *env) cmdVersion(ctx context.Context) error {
	fmt.Println("envsync", version)
	if out, err := exec.CommandContext(ctx, e.stBin, "--version").Output(); err == nil {
		fmt.Println(strings.TrimSpace(string(out)))
	}
	if out, err := exec.CommandContext(ctx, e.ts.Bin, "version").Output(); err == nil {
		fmt.Println("tailscale", strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0])
	}
	ok, _ := install.Exists(ctx, install.DefaultTaskName)
	fmt.Println("автозапуск:", map[bool]string{true: "включён", false: "выключен"}[ok])
	return nil
}

// cmdUpdate обновляет Syncthing и Tailscale через winget (после подтверждения). Сам агент обновляется
// повторным запуском scripts/bootstrap.ps1 или сборкой из репозитория.
func (e *env) cmdUpdate(ctx context.Context) error {
	ids := []string{"Syncthing.Syncthing", "Tailscale.Tailscale"}
	fmt.Println("Будет выполнено через winget (принимаются лицензионные соглашения пакетов, возможен запрос UAC):")
	for _, id := range ids {
		fmt.Printf("  winget upgrade --id %s --exact --source winget\n", id)
	}
	fmt.Println("Агент останавливать не нужно, но Syncthing после обновления перезапустится сам (агентом).")
	if !confirm("Обновить? [y/N] ") {
		fmt.Println("Отменено.")
		return nil
	}
	var errs []error
	for _, id := range ids {
		cmd := exec.CommandContext(ctx, "winget", "upgrade", "--id", id, "--exact", "--source", "winget", "--silent",
			"--accept-package-agreements", "--accept-source-agreements")
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			// Код возврата «обновлений нет» тоже ненулевой: сообщаем, но не считаем фатальным.
			fmt.Printf("  %s: winget вернул %v (возможно, обновлений нет)\n", id, err)
		}
	}
	return errors.Join(errs...)
}

// cmdWait ждёт, пока Syncthing запущенного агента начнёт отвечать. Нужна скриптам: код возврата 0 = готов.
func (e *env) cmdWait(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("wait", flag.ContinueOnError)
	timeout := fs.Int("timeout", 60, "сколько секунд ждать")
	if err := fs.Parse(args); err != nil {
		return err
	}
	deadline := time.Now().Add(time.Duration(*timeout) * time.Second)
	for {
		if key, err := transport.ReadAPIKey(e.s.ConfigXMLPath()); err == nil {
			c := transport.NewClient(e.s.GUIPort, key)
			wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			_, err := c.MyID(wctx)
			cancel()
			if err == nil {
				fmt.Println("Syncthing агента отвечает.")
				return nil
			}
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return fmt.Errorf("Syncthing не ответил за %d с (проверьте agent.log и запрос брандмауэра)", *timeout)
		}
		time.Sleep(2 * time.Second)
	}
}

// openAgentLog открывает agent.log для режима --background, ротируя по размеру (5 МБ, одна старая копия).
func openAgentLog(path string) (*os.File, error) {
	if fi, err := os.Stat(path); err == nil && fi.Size() > 5<<20 {
		_ = os.Remove(path + ".1")
		_ = os.Rename(path, path+".1")
	}
	return os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
}

// hideConsole отсоединяет окно консоли: в фоне агенту оно не нужно (значок в трее остаётся).
func hideConsole() {
	_, _, _ = windows.NewLazySystemDLL("kernel32.dll").NewProc("FreeConsole").Call()
}

func backgroundLogger(path string) (*log.Logger, error) {
	f, err := openAgentLog(path)
	if err != nil {
		return nil, err
	}
	return log.New(f, "", log.LstdFlags), nil
}
