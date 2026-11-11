package transport

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/kiyoushii/EnvSync/internal/core"
)

// Интеграционные тесты с настоящими Syncthing. Запускаются только явно:
//
//	set ENVSYNC_IT=1 && go test ./internal/transport -run Integration -timeout 15m -v
//
// Все экземпляры живут во временных папках и слушают только 127.0.0.1,
// поэтому ни сеть, ни брандмауэр, ни настоящие данные не затрагиваются.

// skipUnlessIT пропускает тест без ENVSYNC_IT=1 и возвращает путь к syncthing.exe.
func skipUnlessIT(t *testing.T) string {
	t.Helper()
	if os.Getenv("ENVSYNC_IT") != "1" {
		t.Skip("интеграционный тест: задайте ENVSYNC_IT=1")
	}
	bin, err := FindSyncthing(Settings{})
	if err != nil {
		t.Skip(err)
	}
	return bin
}

// instance — один запущенный (или остановленный) Syncthing с собственной папкой.
type instance struct {
	name     string
	bin      string
	home     string
	syncPort int
	guiPort  int
	client   *Client
	id       string

	cmd  *exec.Cmd
	done chan struct{}
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

// newInstance генерирует конфиг и применяет ТОТ ЖЕ HardenConfigFile, что и боевой агент.
func newInstance(t *testing.T, bin, name string) *instance {
	t.Helper()
	home := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(bin, "generate", "--home", home, "--no-port-probing").CombinedOutput(); err != nil {
		t.Fatalf("generate: %v: %s", err, out)
	}
	in := &instance{name: name, bin: bin, home: home, syncPort: freePort(t), guiPort: freePort(t)}
	cfg := filepath.Join(home, "config.xml")
	if err := HardenConfigFile(cfg, HardenParams{ListenIP: netip.MustParseAddr("127.0.0.1"), SyncPort: in.syncPort, GUIPort: in.guiPort}); err != nil {
		t.Fatal(err)
	}
	if bad, err := AuditConfigFile(cfg); err != nil || len(bad) > 0 {
		t.Fatalf("аудит: %v %v", bad, err)
	}
	key, err := ReadAPIKey(cfg)
	if err != nil {
		t.Fatal(err)
	}
	in.client = NewClient(in.guiPort, key)
	return in
}

// start запускает Syncthing с теми же флагами, что и агент, и ждёт готовности REST.
func (in *instance) start(t *testing.T) {
	t.Helper()
	cmd := exec.Command(in.bin, "serve", "--home", in.home, "--no-browser", "--no-upgrade", "--no-restart", "--no-port-probing")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	in.cmd, in.done = cmd, done

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := in.client.WaitReady(ctx, 60*time.Second); err != nil {
		t.Fatal(err)
	}
	id, err := in.client.MyID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	in.id = id
}

// stop останавливает Syncthing: корректно (через REST) или «жёстко» (как обрыв питания/сети).
func (in *instance) stop(graceful bool) {
	if in.cmd == nil {
		return
	}
	if graceful {
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = in.client.Shutdown(sctx)
		cancel()
		select {
		case <-in.done:
		case <-time.After(10 * time.Second):
			_ = in.cmd.Process.Kill()
			<-in.done
		}
	} else {
		_ = in.cmd.Process.Kill()
		<-in.done
	}
	in.cmd = nil
}

// startInstance — новый экземпляр, запущенный и автоматически останавливаемый в конце теста.
func startInstance(t *testing.T, bin, name string) *instance {
	t.Helper()
	in := newInstance(t, bin, name)
	in.start(t)
	t.Cleanup(func() { in.stop(true) })
	return in
}

// pairPlan собирает план так же, как BuildPairPlan, но с адресом на loopback.
// Задержку отклика ставим 1 с, чтобы тесты шли быстрее.
func pairPlan(to *instance, folderPath string) *PairPlan {
	return &PairPlan{
		PeerName:      to.name,
		PeerID:        to.id,
		Address:       "tcp://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(to.syncPort)),
		FolderID:      "envsync-test",
		FolderPath:    folderPath,
		Ignores:       core.DefaultIgnores,
		WatcherDelayS: 1,
		MaxConflicts:  10,
	}
}

func pairWith(t *testing.T, from, to *instance, folderPath string) {
	t.Helper()
	if err := ApplyPair(context.Background(), from.client, from.id, pairPlan(to, folderPath)); err != nil {
		t.Fatalf("pair %s→%s: %v", from.name, to.name, err)
	}
}

// pairEnv — две сопряжённые машины с общей тестовой папкой.
type pairEnv struct {
	a, b       *instance
	dirA, dirB string
}

func newPairEnv(t *testing.T) *pairEnv {
	t.Helper()
	bin := skipUnlessIT(t)
	e := &pairEnv{
		a:    startInstance(t, bin, "A"),
		b:    startInstance(t, bin, "B"),
		dirA: filepath.Join(t.TempDir(), "sync"),
		dirB: filepath.Join(t.TempDir(), "sync"),
	}
	pairWith(t, e.a, e.b, e.dirA)
	pairWith(t, e.b, e.a, e.dirB)
	e.waitConnected(t)
	return e
}

func (e *pairEnv) waitConnected(t *testing.T) {
	t.Helper()
	waitFor(t, "A и B соединились", 60*time.Second, func() bool {
		c, err := e.a.client.Connections(context.Background())
		return err == nil && c[e.b.id].Connected
	})
}

func (e *pairEnv) rescan(t *testing.T) {
	t.Helper()
	for _, in := range []*instance{e.a, e.b} {
		_ = in.client.Rescan(context.Background(), "envsync-test")
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

// --- работа с файлами в тестах ---

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readOrEmpty(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// writeRandomFile пишет файл из случайных байт и возвращает его SHA-256.
func writeRandomFile(t *testing.T, path string, mb int) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyN(io.MultiWriter(f, h), rand.Reader, int64(mb)<<20); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func fileHash(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// --- сценарии этапа 1 ---

func TestIntegrationPairAndSync(t *testing.T) {
	e := newPairEnv(t)

	// Файл, созданный на A, доезжает до B, и наоборот.
	mustWrite(t, filepath.Join(e.dirA, "из-A.txt"), "привет от A")
	waitFor(t, "файл из A появился в B", 120*time.Second, func() bool {
		return readOrEmpty(filepath.Join(e.dirB, "из-A.txt")) == "привет от A"
	})
	mustWrite(t, filepath.Join(e.dirB, "из-B.txt"), "ответ от B")
	waitFor(t, "файл из B появился в A", 120*time.Second, func() bool {
		return readOrEmpty(filepath.Join(e.dirA, "из-B.txt")) == "ответ от B"
	})
}

// Негативная проверка критерия этапа 1: чужое устройство подключиться не может
// и автоматически не добавляется.
func TestIntegrationStrangerIsRejected(t *testing.T) {
	bin := skipUnlessIT(t)
	a := startInstance(t, bin, "A")
	stranger := startInstance(t, bin, "stranger")
	ctx := context.Background()

	// Чужой знает адрес A и сам добавляет A у себя, но A чужого не добавлял.
	plan := pairPlan(a, filepath.Join(t.TempDir(), "x"))
	plan.PeerName = "A"
	if err := ApplyPair(ctx, stranger.client, stranger.id, plan); err != nil {
		t.Fatal(err)
	}

	// Даём время на несколько попыток подключения.
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
