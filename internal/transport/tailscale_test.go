package transport

import (
	"context"
	"errors"
	"net/netip"
	"testing"
)

// Урезанный, но реальный по структуре вывод `tailscale status --json`.
const tsRunningJSON = `{
  "BackendState": "Running",
  "Self": {"HostName": "DESKTOP-LS890QQ", "DNSName": "desktop-ls890qq.tail1234.ts.net.",
           "TailscaleIPs": ["fd7a:115c:a1e0::1", "100.106.41.73"], "Online": true},
  "Peer": {
    "nodekey:aaa": {"HostName": "DESKTOP-MGFILO0", "DNSName": "desktop-mgfilo0.tail1234.ts.net.",
                    "TailscaleIPs": ["100.83.157.0", "fd7a:115c:a1e0::2"], "Online": true}
  }
}`

func TestParseTSStatus(t *testing.T) {
	st, err := ParseTSStatus([]byte(tsRunningJSON))
	if err != nil {
		t.Fatal(err)
	}
	if !st.Running() {
		t.Fatal("ожидали Running")
	}
	// IPv6 идёт первым в списке, но нужен именно IPv4.
	if st.Self.IPv4 != netip.MustParseAddr("100.106.41.73") {
		t.Fatalf("self ip = %v", st.Self.IPv4)
	}
	if len(st.Peers) != 1 || st.Peers[0].IPv4 != netip.MustParseAddr("100.83.157.0") {
		t.Fatalf("peers = %+v", st.Peers)
	}
}

func TestFindPeer(t *testing.T) {
	st, _ := ParseTSStatus([]byte(tsRunningJSON))
	for _, name := range []string{"DESKTOP-MGFILO0", "desktop-mgfilo0", " Desktop-MgFiLo0 "} {
		if _, ok := st.FindPeer(name); !ok {
			t.Errorf("сосед %q не найден", name)
		}
	}
	if _, ok := st.FindPeer("desktop-ls890qq"); ok {
		t.Error("себя соседом считать нельзя")
	}
	if _, ok := st.FindPeer("stranger"); ok {
		t.Error("чужое имя найдено")
	}
}

func TestNotRunningStates(t *testing.T) {
	st, err := ParseTSStatus([]byte(`{"BackendState":"NeedsLogin","Self":null,"Peer":null}`))
	if err != nil {
		t.Fatal(err)
	}
	if st.Running() {
		t.Fatal("NeedsLogin не должен считаться Running")
	}
	// Running без адреса тоже не годится: слушать было бы не на чем.
	st, _ = ParseTSStatus([]byte(`{"BackendState":"Running","Self":{"HostName":"x","TailscaleIPs":[]}}`))
	if st.Running() {
		t.Fatal("Running без IPv4 не должен считаться готовым")
	}
}

func TestStatusUsesJSONEvenOnNonZeroExit(t *testing.T) {
	ts := &Tailscale{Bin: "tailscale", Run: func(context.Context, string, ...string) ([]byte, error) {
		return []byte(`{"BackendState":"Stopped"}`), errors.New("exit status 1")
	}}
	st, err := ts.Status(context.Background())
	if err != nil || st.BackendState != "Stopped" {
		t.Fatalf("st=%+v err=%v", st, err)
	}

	ts.Run = func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("no binary") }
	if _, err := ts.Status(context.Background()); err == nil {
		t.Fatal("ожидали ошибку, когда вывода нет")
	}
}
