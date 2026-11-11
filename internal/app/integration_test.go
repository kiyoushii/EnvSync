package app

import (
	"context"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/kiyoushii/EnvSync/internal/core"
	"github.com/kiyoushii/EnvSync/internal/transport"
)

// Бэкенд интерфейса против настоящего Syncthing на loopback. Запускается явно: ENVSYNC_IT=1.
func TestIntegrationBackendAgainstRealSyncthing(t *testing.T) {
	if os.Getenv("ENVSYNC_IT") != "1" {
		t.Skip("интеграционный тест: задайте ENVSYNC_IT=1")
	}
	bin, err := transport.FindSyncthing(transport.Settings{})
	if err != nil {
		t.Skip(err)
	}

	base := t.TempDir()
	s, err := transport.LoadSettings(base)
	if err != nil {
		t.Fatal(err)
	}
	home := s.SyncthingHome()
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(bin, "generate", "--home", home, "--no-port-probing").CombinedOutput(); err != nil {
		t.Fatalf("generate: %v: %s", err, out)
	}
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	s.GUIPort = l.Addr().(*net.TCPAddr).Port
	l.Close()
	l2, _ := net.Listen("tcp", "127.0.0.1:0")
	syncPort := l2.Addr().(*net.TCPAddr).Port
	l2.Close()
	if err := transport.HardenConfigFile(s.ConfigXMLPath(), transport.HardenParams{ListenIP: netip.MustParseAddr("127.0.0.1"), SyncPort: syncPort, GUIPort: s.GUIPort}); err != nil {
		t.Fatal(err)
	}
	key, err := transport.ReadAPIKey(s.ConfigXMLPath())
	if err != nil {
		t.Fatal(err)
	}
	client := transport.NewClient(s.GUIPort, key)

	cmd := exec.Command(bin, "serve", "--home", home, "--no-browser", "--no-upgrade", "--no-restart", "--no-port-probing")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = client.Shutdown(sctx)
		time.Sleep(time.Second)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	ctx := context.Background()
	if err := client.WaitReady(ctx, 60*time.Second); err != nil {
		t.Fatal(err)
	}
	selfID, err := client.MyID(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Папка с одним обычным файлом и одной конфликтной копией.
	docs := filepath.Join(t.TempDir(), "docs")
	if err := transport.ApplyFolder(ctx, client, selfID, nil, transport.FolderSpec{ID: "docs", Path: docs}); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"план.txt": "обычный файл",
		"план.sync-conflict-20261111-120000-ABCDEFG.txt": "конфликтная копия",
	} {
		if err := os.WriteFile(filepath.Join(docs, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_ = client.Rescan(ctx, "docs")

	b := &Backend{
		S: s, Machine: "TEST-PC", Version: "test",
		TS: &transport.Tailscale{Bin: "tailscale", Run: func(context.Context, string, ...string) ([]byte, error) {
			return []byte(`{"BackendState":"Running","Self":{"HostName":"TEST-PC","TailscaleIPs":["100.1.1.1"]}}`), nil
		}},
		Guard: &transport.Guard{C: client, Cfg: core.MassDeleteDef{MaxFiles: 1000, MaxPercent: 5, MinFiles: 10}, StatePath: s.GuardFile()},
		Audit: &core.Journal{Path: s.JournalFile()},
	}

	// Self видит настоящий Syncthing и его папки.
	var found bool
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && !found {
		st, err := b.Self(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !st.Syncthing.Running || st.Syncthing.DeviceID != selfID {
			t.Fatalf("Syncthing должен быть виден: %+v", st.Syncthing)
		}
		for _, f := range st.Syncthing.Folders {
			if f.ID == "docs" && f.LocalFiles >= 2 {
				found = true
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !found {
		t.Fatal("папка docs с файлами не видна в статусе")
	}

	// Конфликтная копия находится, обычный файл нет; оригинальное имя восстановлено.
	b.conflictsAt = time.Time{}
	conflicts, err := b.Conflicts(ctx)
	if err != nil || len(conflicts) != 1 || conflicts[0].Original != "план.txt" || conflicts[0].Folder != "docs" {
		t.Fatalf("conflicts=%+v err=%v", conflicts, err)
	}

	// Пауза всего не падает и отражается в журнале.
	if err := b.SetPaused(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := b.SetPaused(ctx, false); err != nil {
		t.Fatal(err)
	}
	if entries, _ := b.Journal(10); len(entries) != 2 {
		t.Errorf("в журнале должны быть пауза и возобновление: %v", entries)
	}

	// Статус записывается в папку состояний.
	if err := b.ExportStatus(ctx); err != nil {
		t.Fatal(err)
	}
	if sf, err := (core.StateDir{Root: s.StateDir()}).Read("TEST-PC", "status"); err != nil || sf == nil {
		t.Fatalf("статус не записан: %v %v", sf, err)
	}
}
