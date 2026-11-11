package transport

import (
	"context"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// Интеграционный тест с настоящими Syncthing. Запускается только явно:
//
//	set ENVSYNC_IT=1 && go test ./internal/transport -run Integration -timeout 5m -v
//
// Все три экземпляра живут во временных папках и слушают только 127.0.0.1,
// поэтому ни сеть, ни брандмауэр, ни настоящие данные не затрагиваются.

type instance struct {
	name     string
	home     string
	syncPort int
	client   *Client
	id       string
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// startInstance генерирует конфиг, применяет ТОТ ЖЕ HardenConfigFile, что и боевой агент,
// запускает Syncthing с теми же флагами и ждёт готовности REST.
func startInstance(t *testing.T, bin, name string) *instance {
	t.Helper()
	home := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(bin, "generate", "--home", home, "--no-port-probing").CombinedOutput(); err != nil {
		t.Fatalf("generate: %v: %s", err, out)
	}
	in := &instance{name: name, home: home, syncPort: freePort(t)}
	guiPort := freePort(t)
	cfg := filepath.Join(home, "config.xml")
	if err := HardenConfigFile(cfg, HardenParams{ListenIP: netip.MustParseAddr("127.0.0.1"), SyncPort: in.syncPort, GUIPort: guiPort}); err != nil {
		t.Fatal(err)
	}
	if bad, err := AuditConfigFile(cfg); err != nil || len(bad) > 0 {
		t.Fatalf("аудит: %v %v", bad, err)
	}
	key, err := ReadAPIKey(cfg)
	if err != nil {
		t.Fatal(err)
	}
	in.client = NewClient(guiPort, key)

	cmd := exec.Command(bin, "serve", "--home", home, "--no-browser", "--no-upgrade", "--no-restart", "--no-port-probing")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = in.client.Shutdown(sctx)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := in.client.WaitReady(ctx, 60*time.Second); err != nil {
		t.Fatal(err)
	}
	if in.id, err = in.client.MyID(ctx); err != nil {
		t.Fatal(err)
	}
	return in
}

func pairWith(t *testing.T, from, to *instance, folderPath string) {
	t.Helper()
	plan := &PairPlan{
		PeerName:   to.name,
		PeerID:     to.id,
		Address:    "tcp://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(to.syncPort)),
		FolderID:   "envsync-test",
		FolderPath: folderPath,
	}
	if err := ApplyPair(context.Background(), from.client, from.id, plan); err != nil {
		t.Fatalf("pair %s→%s: %v", from.name, to.name, err)
	}
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("не дождались: %s (за %v)", what, timeout)
}

func TestIntegrationPairAndSync(t *testing.T) {
	if os.Getenv("ENVSYNC_IT") != "1" {
		t.Skip("интеграционный тест: задайте ENVSYNC_IT=1")
	}
	bin, err := FindSyncthing(Settings{})
	if err != nil {
		t.Skip(err)
	}

	a := startInstance(t, bin, "A")
	b := startInstance(t, bin, "B")
	folderA, folderB := filepath.Join(t.TempDir(), "folder"), filepath.Join(t.TempDir(), "folder")

	pairWith(t, a, b, folderA)
	pairWith(t, b, a, folderB)

	// Связь установилась только с тем, кого мы добавили.
	ctx := context.Background()
	waitFor(t, "A подключился к B", 60*time.Second, func() bool {
		c, err := a.client.Connections(ctx)
		return err == nil && c[b.id].Connected
	})

	// Файл, созданный на A, доезжает до B, и наоборот.
	if err := os.WriteFile(filepath.Join(folderA, "из-A.txt"), []byte("привет от A"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "файл из A появился в B", 120*time.Second, func() bool {
		data, err := os.ReadFile(filepath.Join(folderB, "из-A.txt"))
		return err == nil && string(data) == "привет от A"
	})
	if err := os.WriteFile(filepath.Join(folderB, "из-B.txt"), []byte("ответ от B"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "файл из B появился в A", 120*time.Second, func() bool {
		data, err := os.ReadFile(filepath.Join(folderA, "из-B.txt"))
		return err == nil && string(data) == "ответ от B"
	})
}

// Негативная проверка критерия этапа 1: чужое устройство подключиться не может
// и автоматически не добавляется.
func TestIntegrationStrangerIsRejected(t *testing.T) {
	if os.Getenv("ENVSYNC_IT") != "1" {
		t.Skip("интеграционный тест: задайте ENVSYNC_IT=1")
	}
	bin, err := FindSyncthing(Settings{})
	if err != nil {
		t.Skip(err)
	}

	a := startInstance(t, bin, "A")
	stranger := startInstance(t, bin, "stranger")
	ctx := context.Background()

	// Чужой знает адрес A и сам добавляет A у себя, но A чужого не добавлял.
	plan := &PairPlan{
		PeerName:   "A",
		PeerID:     a.id,
		Address:    "tcp://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(a.syncPort)),
		FolderID:   "envsync-test",
		FolderPath: filepath.Join(t.TempDir(), "x"),
	}
	if err := ApplyPair(ctx, stranger.client, stranger.id, plan); err != nil {
		t.Fatal(err)
	}

	// Даём время на несколько попыток подключения (интервал переподключения 20 с в конфиге,
	// первая попытка сразу после добавления).
	time.Sleep(15 * time.Second)

	if c, err := stranger.client.Connections(ctx); err != nil || c[a.id].Connected {
		t.Fatalf("чужой подключился к A: %+v err=%v", c[a.id], err)
	}
	if c, err := a.client.Connections(ctx); err != nil || c[stranger.id].Connected {
		t.Fatalf("A принял чужого: %+v err=%v", c[stranger.id], err)
	}
	if dev, err := a.client.Device(ctx, stranger.id); err != nil || dev != nil {
		t.Fatalf("чужой попал в конфиг A: %v err=%v", dev, err)
	}
	// Тест не вхолостую: попытка дошла до A и была отложена как «ожидающая», а не принята.
	waitFor(t, "A заметил попытку чужого", 30*time.Second, func() bool {
		p, err := a.client.PendingDevices(ctx)
		_, seen := p[stranger.id]
		return err == nil && seen
	})
}
