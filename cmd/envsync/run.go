package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/kiyoushii/EnvSync/internal/app"
	"github.com/kiyoushii/EnvSync/internal/core"
	"github.com/kiyoushii/EnvSync/internal/transport"
	"github.com/kiyoushii/EnvSync/internal/ui"
)

// version — версия агента (проставляется при сборке: -ldflags "-X main.version=...").
var version = "dev"

// cmdRun запускает всё сразу: Syncthing под присмотром, предохранитель, веб-интерфейс, экспорт статуса
// и значок в трее.
func (e *env) cmdRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	noUI := fs.Bool("no-ui", false, "не запускать веб-интерфейс и значок в трее")
	noTray := fs.Bool("no-tray", false, "не показывать значок в трее (веб-интерфейс остаётся)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cenv, err := core.CurrentEnv()
	if err != nil {
		return err
	}

	a := e.agent()
	// Ключ REST создаётся при первой генерации конфига; он нужен предохранителю и интерфейсу до запуска Syncthing.
	if _, err := a.Init(ctx, e.stBin); err != nil {
		return err
	}
	key, err := transport.ReadAPIKey(e.s.ConfigXMLPath())
	if err != nil {
		return err
	}
	client := transport.NewClient(e.s.GUIPort, key)
	guard := &transport.Guard{
		C: client, Cfg: a.MassDelete, StatePath: e.s.GuardFile(),
		Journal: a.Journal, Machine: cenv.Machine, Log: e.logger,
	}
	a.Guard = guard

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var be *app.Backend
	var uiURL func() string
	if !*noUI {
		be = &app.Backend{
			S: e.s, TS: e.ts, Machine: cenv.Machine, Version: version, Guard: guard,
			Engine: e.engine, Audit: a.Journal,
		}
		token, err := ui.NewToken()
		if err != nil {
			return err
		}
		if err := os.WriteFile(e.s.UITokenFile(), []byte(token), 0o600); err != nil {
			return err
		}
		l, err := ui.Listen(e.s.UIPort)
		if err != nil {
			return fmt.Errorf("порт интерфейса %d занят (измените uiPort в settings.json): %w", e.s.UIPort, err)
		}
		srv := &ui.Server{B: be, Token: token}
		go func() {
			if err := srv.Serve(runCtx, l); err != nil {
				e.logger.Printf("веб-интерфейс остановился: %v", err)
			}
		}()
		go be.StatusLoop(runCtx, 15*time.Second)
		uiURL = func() string { return ui.URL(e.s.UIPort, token) }
		fmt.Println("Веб-интерфейс:", uiURL())
	}

	agentErr := make(chan error, 1)
	go func() { agentErr <- a.Run(runCtx); cancel() }()

	fmt.Println("Агент запущен. Остановить: Ctrl+C.")
	if be != nil && !*noTray {
		runTray(runCtx, cancel, be, uiURL) // блокирует до выхода из меню трея
		cancel()
	}
	<-runCtx.Done()
	select {
	case err := <-agentErr:
		return err
	case <-time.After(15 * time.Second):
		return errors.New("агент не остановился за 15 секунд")
	}
}

// cmdUI открывает веб-интерфейс запущенного агента в браузере.
func (e *env) cmdUI() error {
	data, err := os.ReadFile(e.s.UITokenFile())
	if err != nil {
		return errors.New("агент не запущен (нет токена интерфейса): запустите envsync run")
	}
	url := ui.URL(e.s.UIPort, strings.TrimSpace(string(data)))
	fmt.Println("Открываю", "http://127.0.0.1:"+strconv.Itoa(e.s.UIPort)+"/")
	openBrowser(url)
	return nil
}

// cmdChecklist печатает чек-лист ручной настройки новой машины.
func (e *env) cmdChecklist() error {
	var manual []core.ManualItem
	if m, err := core.LoadManifest(e.s.ManifestFile()); err == nil && m.Software != nil {
		manual = m.Software.Manual
	}
	fmt.Print(core.Checklist(core.NotSyncedStatic(), manual))
	return nil
}
