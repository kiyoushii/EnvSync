// Команда envsync — агент синхронизации среды Windows (этап 1: сеть и сопряжение).
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/kiyoushii/EnvSync/internal/transport"
)

const usage = `envsync — синхронизация среды Windows (этап 1)

Команды:
  init                          создать ключи и конфиг Syncthing, показать Device ID
  id                            показать Device ID и имя этой машины в Tailscale
  run                           запустить Syncthing и следить за Tailscale (работает, пока не нажмёте Ctrl+C)
  status                        состояние Tailscale, Syncthing и связи со вторым устройством
  audit                         проверить, что конфиг Syncthing слушает только Tailscale
  pair <имя-в-tailscale> <id>   добавить вторую машину (dry-run и подтверждение)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := dispatch(ctx, os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "ошибка:", err)
		os.Exit(1)
	}
}

// env собирает всё, что нужно командам: настройки, пути к программам, Tailscale.
type env struct {
	s      transport.Settings
	stBin  string
	ts     *transport.Tailscale
	logger *log.Logger
}

func loadEnv() (*env, error) {
	base, err := transport.DefaultBaseDir()
	if err != nil {
		return nil, err
	}
	s, err := transport.LoadSettings(base)
	if err != nil {
		return nil, err
	}
	stBin, err := transport.FindSyncthing(s)
	if err != nil {
		return nil, err
	}
	tsBin, err := transport.FindTailscale(s)
	if err != nil {
		return nil, err
	}
	return &env{s: s, stBin: stBin, ts: &transport.Tailscale{Bin: tsBin}, logger: log.New(os.Stderr, "", log.LstdFlags)}, nil
}

func dispatch(ctx context.Context, cmd string, args []string) error {
	e, err := loadEnv()
	if err != nil {
		return err
	}
	switch cmd {
	case "init":
		return e.cmdInit(ctx)
	case "id":
		return e.cmdID(ctx)
	case "run":
		a := &transport.Agent{S: e.s, TS: e.ts, Log: e.logger}
		fmt.Println("Агент запущен. Остановить: Ctrl+C.")
		return a.Run(ctx)
	case "status":
		return e.cmdStatus(ctx)
	case "audit":
		return e.cmdAudit()
	case "pair":
		return e.cmdPair(ctx, args)
	default:
		fmt.Print(usage)
		return fmt.Errorf("неизвестная команда %q", cmd)
	}
}

func (e *env) agent() *transport.Agent {
	return &transport.Agent{S: e.s, TS: e.ts, Log: e.logger}
}

func (e *env) cmdInit(ctx context.Context) error {
	id, err := e.agent().Init(ctx, e.stBin)
	if err != nil {
		return err
	}
	fmt.Println("Папка агента:", e.s.BaseDir())
	fmt.Println("Device ID:", id)
	return nil
}

func (e *env) cmdID(ctx context.Context) error {
	id, err := transport.DeviceID(ctx, e.stBin, e.s.SyncthingHome())
	if err != nil {
		return fmt.Errorf("%w (если конфига ещё нет, выполните: envsync init)", err)
	}
	fmt.Println("Device ID:", id)
	if st, err := e.ts.Status(ctx); err == nil && st.Running() {
		fmt.Printf("Tailscale: %s (%v)\n", st.Self.Name, st.Self.IPv4)
	}
	return nil
}

func (e *env) cmdAudit() error {
	bad, err := transport.AuditConfigFile(e.s.ConfigXMLPath())
	if err != nil {
		return err
	}
	if len(bad) == 0 {
		fmt.Println("OK: Syncthing слушает только Tailscale/loopback, обнаружение, релеи и NAT выключены.")
		return nil
	}
	for _, b := range bad {
		fmt.Println("НАРУШЕНИЕ:", b)
	}
	return errors.New("конфиг не соответствует правилам; `envsync run` перезапишет его при старте")
}

func (e *env) cmdStatus(ctx context.Context) error {
	st, err := e.ts.Status(ctx)
	switch {
	case err != nil:
		fmt.Println("Tailscale: не удалось получить статус:", err)
	case !st.Running():
		fmt.Printf("Tailscale: не подключён (%s)\n", st.BackendState)
	default:
		fmt.Printf("Tailscale: подключён как %s (%v)\n", st.Self.Name, st.Self.IPv4)
		for _, p := range st.Peers {
			fmt.Printf("  сосед %s %v в сети: %v\n", p.Name, p.IPv4, p.Online)
		}
	}

	key, err := transport.ReadAPIKey(e.s.ConfigXMLPath())
	if err != nil {
		fmt.Println("Syncthing: конфига ещё нет (выполните: envsync init)")
		return nil
	}
	c := transport.NewClient(e.s.GUIPort, key)
	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	id, err := c.MyID(rctx)
	if err != nil {
		fmt.Println("Syncthing: не запущен (запустите: envsync run)")
		return nil
	}
	fmt.Println("Syncthing: запущен, Device ID", id)
	conns, err := c.Connections(rctx)
	if err != nil {
		return err
	}
	for devID, cn := range conns {
		fmt.Printf("  устройство %s: подключено=%v адрес=%s\n", devID, cn.Connected, cn.Address)
	}
	return nil
}

func (e *env) cmdPair(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pair", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "не спрашивать подтверждение")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return errors.New("использование: envsync pair [--yes] <имя-машины-в-tailscale> <device-id-второй-машины>")
	}
	peerName, peerID := fs.Arg(0), fs.Arg(1)

	key, err := transport.ReadAPIKey(e.s.ConfigXMLPath())
	if err != nil {
		return errors.New("конфига Syncthing нет: выполните envsync init и envsync run")
	}
	c := transport.NewClient(e.s.GUIPort, key)
	selfID, err := c.MyID(ctx)
	if err != nil {
		return errors.New("Syncthing не запущен: запустите envsync run в другом окне")
	}
	ts, err := e.ts.Status(ctx)
	if err != nil {
		return err
	}
	plan, err := transport.BuildPairPlan(e.s, ts, peerName, peerID, selfID)
	if err != nil {
		return err
	}

	fmt.Print(plan.Describe())
	if !*yes && !confirm("Применить? [y/N] ") {
		fmt.Println("Отменено, ничего не изменено.")
		return nil
	}
	if err := transport.ApplyPair(ctx, c, selfID, plan); err != nil {
		return err
	}
	fmt.Println("Готово. Теперь выполните то же самое на второй машине, указав ID этой:", selfID)
	return nil
}

func confirm(prompt string) bool {
	fmt.Print(prompt)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.EqualFold(strings.TrimSpace(line), "y")
}
