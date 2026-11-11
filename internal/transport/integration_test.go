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

// РРЅС‚РµРіСЂР°С†РёРѕРЅРЅС‹Рµ С‚РµСЃС‚С‹ СЃ РЅР°СЃС‚РѕСЏС‰РёРјРё Syncthing. Р—Р°РїСѓСЃРєР°СЋС‚СЃСЏ С‚РѕР»СЊРєРѕ СЏРІРЅРѕ:
//
//	set ENVSYNC_IT=1 && go test ./internal/transport -run Integration -timeout 15m -v
//
// Р’СЃРµ СЌРєР·РµРјРїР»СЏСЂС‹ Р¶РёРІСѓС‚ РІРѕ РІСЂРµРјРµРЅРЅС‹С… РїР°РїРєР°С… Рё СЃР»СѓС€Р°СЋС‚ С‚РѕР»СЊРєРѕ 127.0.0.1,
// РїРѕСЌС‚РѕРјСѓ РЅРё СЃРµС‚СЊ, РЅРё Р±СЂР°РЅРґРјР°СѓСЌСЂ, РЅРё РЅР°СЃС‚РѕСЏС‰РёРµ РґР°РЅРЅС‹Рµ РЅРµ Р·Р°С‚СЂР°РіРёРІР°СЋС‚СЃСЏ.

// skipUnlessIT РїСЂРѕРїСѓСЃРєР°РµС‚ С‚РµСЃС‚ Р±РµР· ENVSYNC_IT=1 Рё РІРѕР·РІСЂР°С‰Р°РµС‚ РїСѓС‚СЊ Рє syncthing.exe.
func skipUnlessIT(t *testing.T) string {
	t.Helper()
	if os.Getenv("ENVSYNC_IT") != "1" {
		t.Skip("РёРЅС‚РµРіСЂР°С†РёРѕРЅРЅС‹Р№ С‚РµСЃС‚: Р·Р°РґР°Р№С‚Рµ ENVSYNC_IT=1")
	}
	bin, err := FindSyncthing(Settings{})
	if err != nil {
		t.Skip(err)
	}
	return bin
}

// instance вЂ” РѕРґРёРЅ Р·Р°РїСѓС‰РµРЅРЅС‹Р№ (РёР»Рё РѕСЃС‚Р°РЅРѕРІР»РµРЅРЅС‹Р№) Syncthing СЃ СЃРѕР±СЃС‚РІРµРЅРЅРѕР№ РїР°РїРєРѕР№.
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

// newInstance РіРµРЅРµСЂРёСЂСѓРµС‚ РєРѕРЅС„РёРі Рё РїСЂРёРјРµРЅСЏРµС‚ РўРћРў Р–Р• HardenConfigFile, С‡С‚Рѕ Рё Р±РѕРµРІРѕР№ Р°РіРµРЅС‚.
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
		t.Fatalf("Р°СѓРґРёС‚: %v %v", bad, err)
	}
	key, err := ReadAPIKey(cfg)
	if err != nil {
		t.Fatal(err)
	}
	in.client = NewClient(in.guiPort, key)
	return in
}

// start Р·Р°РїСѓСЃРєР°РµС‚ Syncthing СЃ С‚РµРјРё Р¶Рµ С„Р»Р°РіР°РјРё, С‡С‚Рѕ Рё Р°РіРµРЅС‚, Рё Р¶РґС‘С‚ РіРѕС‚РѕРІРЅРѕСЃС‚Рё REST.
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

// stop РѕСЃС‚Р°РЅР°РІР»РёРІР°РµС‚ Syncthing: РєРѕСЂСЂРµРєС‚РЅРѕ (С‡РµСЂРµР· REST) РёР»Рё В«Р¶С‘СЃС‚РєРѕВ» (РєР°Рє РѕР±СЂС‹РІ РїРёС‚Р°РЅРёСЏ/СЃРµС‚Рё).
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

// startInstance вЂ” РЅРѕРІС‹Р№ СЌРєР·РµРјРїР»СЏСЂ, Р·Р°РїСѓС‰РµРЅРЅС‹Р№ Рё Р°РІС‚РѕРјР°С‚РёС‡РµСЃРєРё РѕСЃС‚Р°РЅР°РІР»РёРІР°РµРјС‹Р№ РІ РєРѕРЅС†Рµ С‚РµСЃС‚Р°.
func startInstance(t *testing.T, bin, name string) *instance {
	t.Helper()
	in := newInstance(t, bin, name)
	in.start(t)
	t.Cleanup(func() { in.stop(true) })
	return in
}

// pairPlan СЃРѕР±РёСЂР°РµС‚ РїР»Р°РЅ С‚Р°Рє Р¶Рµ, РєР°Рє BuildPairPlan, РЅРѕ СЃ Р°РґСЂРµСЃРѕРј РЅР° loopback.
// Р—Р°РґРµСЂР¶РєСѓ РѕС‚РєР»РёРєР° СЃС‚Р°РІРёРј 1 СЃ, С‡С‚РѕР±С‹ С‚РµСЃС‚С‹ С€Р»Рё Р±С‹СЃС‚СЂРµРµ.
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
		t.Fatalf("pair %sв†’%s: %v", from.name, to.name, err)
	}
}

// pairEnv вЂ” РґРІРµ СЃРѕРїСЂСЏР¶С‘РЅРЅС‹Рµ РјР°С€РёРЅС‹ СЃ РѕР±С‰РµР№ С‚РµСЃС‚РѕРІРѕР№ РїР°РїРєРѕР№.
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
	waitFor(t, "A Рё B СЃРѕРµРґРёРЅРёР»РёСЃСЊ", 60*time.Second, func() bool {
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
	t.Fatalf("РЅРµ РґРѕР¶РґР°Р»РёСЃСЊ: %s (Р·Р° %v)", what, timeout)
}

// --- СЂР°Р±РѕС‚Р° СЃ С„Р°Р№Р»Р°РјРё РІ С‚РµСЃС‚Р°С… ---

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

// writeRandomFile РїРёС€РµС‚ С„Р°Р№Р» РёР· СЃР»СѓС‡Р°Р№РЅС‹С… Р±Р°Р№С‚ Рё РІРѕР·РІСЂР°С‰Р°РµС‚ РµРіРѕ SHA-256.
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

// --- СЃС†РµРЅР°СЂРёРё СЌС‚Р°РїР° 1 ---

func TestIntegrationPairAndSync(t *testing.T) {
	e := newPairEnv(t)

	// Р¤Р°Р№Р», СЃРѕР·РґР°РЅРЅС‹Р№ РЅР° A, РґРѕРµР·Р¶Р°РµС‚ РґРѕ B, Рё РЅР°РѕР±РѕСЂРѕС‚.
	mustWrite(t, filepath.Join(e.dirA, "РёР·-A.txt"), "РїСЂРёРІРµС‚ РѕС‚ A")
	waitFor(t, "С„Р°Р№Р» РёР· A РїРѕСЏРІРёР»СЃСЏ РІ B", 120*time.Second, func() bool {
		return readOrEmpty(filepath.Join(e.dirB, "РёР·-A.txt")) == "РїСЂРёРІРµС‚ РѕС‚ A"
	})
	mustWrite(t, filepath.Join(e.dirB, "РёР·-B.txt"), "РѕС‚РІРµС‚ РѕС‚ B")
	waitFor(t, "С„Р°Р№Р» РёР· B РїРѕСЏРІРёР»СЃСЏ РІ A", 120*time.Second, func() bool {
		return readOrEmpty(filepath.Join(e.dirA, "РёР·-B.txt")) == "РѕС‚РІРµС‚ РѕС‚ B"
	})
}

// РќРµРіР°С‚РёРІРЅР°СЏ РїСЂРѕРІРµСЂРєР° РєСЂРёС‚РµСЂРёСЏ СЌС‚Р°РїР° 1: С‡СѓР¶РѕРµ СѓСЃС‚СЂРѕР№СЃС‚РІРѕ РїРѕРґРєР»СЋС‡РёС‚СЊСЃСЏ РЅРµ РјРѕР¶РµС‚
// Рё Р°РІС‚РѕРјР°С‚РёС‡РµСЃРєРё РЅРµ РґРѕР±Р°РІР»СЏРµС‚СЃСЏ.
func TestIntegrationStrangerIsRejected(t *testing.T) {
	bin := skipUnlessIT(t)
	a := startInstance(t, bin, "A")
	stranger := startInstance(t, bin, "stranger")
	ctx := context.Background()

	// Р§СѓР¶РѕР№ Р·РЅР°РµС‚ Р°РґСЂРµСЃ A Рё СЃР°Рј РґРѕР±Р°РІР»СЏРµС‚ A Сѓ СЃРµР±СЏ, РЅРѕ A С‡СѓР¶РѕРіРѕ РЅРµ РґРѕР±Р°РІР»СЏР».
	plan := pairPlan(a, filepath.Join(t.TempDir(), "x"))
	plan.PeerName = "A"
	if err := ApplyPair(ctx, stranger.client, stranger.id, plan); err != nil {
		t.Fatal(err)
	}

	// Р”Р°С‘Рј РІСЂРµРјСЏ РЅР° РЅРµСЃРєРѕР»СЊРєРѕ РїРѕРїС‹С‚РѕРє РїРѕРґРєР»СЋС‡РµРЅРёСЏ.
	time.Sleep(15 * time.Second)

	if c, err := stranger.client.Connections(ctx); err != nil || c[a.id].Connected {
		t.Fatalf("С‡СѓР¶РѕР№ РїРѕРґРєР»СЋС‡РёР»СЃСЏ Рє A: %+v err=%v", c[a.id], err)
	}
	if c, err := a.client.Connections(ctx); err != nil || c[stranger.id].Connected {
		t.Fatalf("A РїСЂРёРЅСЏР» С‡СѓР¶РѕРіРѕ: %+v err=%v", c[stranger.id], err)
	}
	if dev, err := a.client.Device(ctx, stranger.id); err != nil || dev != nil {
		t.Fatalf("С‡СѓР¶РѕР№ РїРѕРїР°Р» РІ РєРѕРЅС„РёРі A: %v err=%v", dev, err)
	}
	// РўРµСЃС‚ РЅРµ РІС…РѕР»РѕСЃС‚СѓСЋ: РїРѕРїС‹С‚РєР° РґРѕС€Р»Р° РґРѕ A Рё Р±С‹Р»Р° РѕС‚Р»РѕР¶РµРЅР° РєР°Рє В«РѕР¶РёРґР°СЋС‰Р°СЏВ», Р° РЅРµ РїСЂРёРЅСЏС‚Р°.
	waitFor(t, "A Р·Р°РјРµС‚РёР» РїРѕРїС‹С‚РєСѓ С‡СѓР¶РѕРіРѕ", 30*time.Second, func() bool {
		p, err := a.client.PendingDevices(ctx)
		_, seen := p[stranger.id]
		return err == nil && seen
	})
}
