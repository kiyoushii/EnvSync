package transport

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const (
	selfID = "AAAAAAA-BBBBBBB-CCCCCCC-DDDDDDD-EEEEEEE-FFFFFFF-GGGGGGG-HHHHHHH"
	peerID = "IE4XOXX-PR6QRY2-PH243IR-TWYCAJ3-3E47HU6-35ZDQGD-RBWYNO6-6MMNUQY"
)

func testSettings(t *testing.T) Settings {
	t.Helper()
	return Settings{SyncPort: 22000, GUIPort: 18384, TestFolderID: "envsync-test", TestFolderPath: filepath.Join(t.TempDir(), "EnvSyncTest")}
}

func TestNormalizeDeviceID(t *testing.T) {
	if id, err := NormalizeDeviceID(" " + strings.ToLower(peerID) + "\n"); err != nil || id != peerID {
		t.Fatalf("id=%q err=%v", id, err)
	}
	for _, bad := range []string{"", "abc", peerID + "X", strings.ReplaceAll(peerID, "-", ""), strings.Replace(peerID, "I", "1", 1)} {
		if _, err := NormalizeDeviceID(bad); err == nil {
			t.Errorf("%q не должен приниматься", bad)
		}
	}
}

func TestBuildPairPlan(t *testing.T) {
	ts, _ := ParseTSStatus([]byte(tsRunningJSON))
	s := testSettings(t)

	plan, err := BuildPairPlan(s, ts, "desktop-mgfilo0", peerID, selfID)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Address != "tcp://100.83.157.0:22000" || plan.PeerName != "DESKTOP-MGFILO0" {
		t.Fatalf("plan = %+v", plan)
	}
	if d := plan.Describe(); !strings.Contains(d, "tcp://100.83.157.0:22000") {
		t.Errorf("в dry-run нет адреса: %s", d)
	}
}

func TestBuildPairPlanRefusals(t *testing.T) {
	ts, _ := ParseTSStatus([]byte(tsRunningJSON))
	s := testSettings(t)

	cases := map[string]func() error{
		"машины нет в tailnet": func() error { _, err := BuildPairPlan(s, ts, "stranger", peerID, selfID); return err },
		"свой же Device ID":    func() error { _, err := BuildPairPlan(s, ts, "desktop-mgfilo0", selfID, selfID); return err },
		"кривой Device ID":     func() error { _, err := BuildPairPlan(s, ts, "desktop-mgfilo0", "nope", selfID); return err },
		"Tailscale не в сети": func() error {
			down, _ := ParseTSStatus([]byte(`{"BackendState":"Stopped"}`))
			_, err := BuildPairPlan(s, down, "desktop-mgfilo0", peerID, selfID)
			return err
		},
	}
	for name, f := range cases {
		if f() == nil {
			t.Errorf("%s: ожидали отказ", name)
		}
	}
}

// fakeSyncthing имитирует REST Syncthing: хранит устройства и папки в памяти
// и записывает, что в них клали.
type fakeSyncthing struct {
	mu      sync.Mutex
	devices map[string]map[string]any
	folders map[string]map[string]any
}

func (f *fakeSyncthing) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("X-API-Key") != "k" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	p := r.URL.Path
	switch {
	case p == "/rest/config/defaults/device":
		_ = json.NewEncoder(w).Encode(map[string]any{"deviceID": "", "addresses": []string{"dynamic"}, "autoAcceptFolders": true})
	case p == "/rest/config/defaults/folder":
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "", "type": "sendreceive", "fsWatcherEnabled": true})
	case strings.HasPrefix(p, "/rest/config/devices/"):
		f.store(w, r, f.devices, strings.TrimPrefix(p, "/rest/config/devices/"))
	case strings.HasPrefix(p, "/rest/config/folders/"):
		f.store(w, r, f.folders, strings.TrimPrefix(p, "/rest/config/folders/"))
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeSyncthing) store(w http.ResponseWriter, r *http.Request, m map[string]map[string]any, id string) {
	switch r.Method {
	case http.MethodGet:
		if v, ok := m[id]; ok {
			_ = json.NewEncoder(w).Encode(v)
		} else {
			http.NotFound(w, r)
		}
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		var v map[string]any
		_ = json.Unmarshal(body, &v)
		m[id] = v
	}
}

func newFake(t *testing.T) (*Client, *fakeSyncthing) {
	t.Helper()
	f := &fakeSyncthing{devices: map[string]map[string]any{}, folders: map[string]map[string]any{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	port, _ := strconv.Atoi(srv.URL[strings.LastIndex(srv.URL, ":")+1:])
	return NewClient(port, "k"), f
}

func TestApplyPair(t *testing.T) {
	c, f := newFake(t)
	ts, _ := ParseTSStatus([]byte(tsRunningJSON))
	s := testSettings(t)
	plan, _ := BuildPairPlan(s, ts, "desktop-mgfilo0", peerID, selfID)

	// Дважды: повторный запуск должен приводить к тому же состоянию, а не плодить дубли.
	for i := 0; i < 2; i++ {
		if err := ApplyPair(context.Background(), c, selfID, plan); err != nil {
			t.Fatal(err)
		}
	}

	dev := f.devices[peerID]
	addrs, _ := dev["addresses"].([]any)
	if len(addrs) != 1 || addrs[0] != "tcp://100.83.157.0:22000" {
		t.Errorf("адреса устройства = %v (должен быть один статический, без dynamic)", addrs)
	}
	if dev["autoAcceptFolders"] != false || dev["introducer"] != false {
		t.Errorf("autoAcceptFolders/introducer должны быть false: %v", dev)
	}

	folder := f.folders["envsync-test"]
	if folder["path"] != s.TestFolderPath {
		t.Errorf("path = %v", folder["path"])
	}
	devs, _ := folder["devices"].([]any)
	if len(devs) != 2 {
		t.Errorf("в папке должно быть ровно 2 устройства (мы и сосед), а есть %v", devs)
	}
	if v, _ := folder["versioning"].(map[string]any); v["type"] != "trashcan" {
		t.Errorf("versioning = %v", folder["versioning"])
	}
}

func TestClientSendsAPIKey(t *testing.T) {
	c, _ := newFake(t)
	bad := NewClient(c.portForTest(), "wrong")
	if err := bad.PutDevice(context.Background(), "X", cfgObject{}); err == nil {
		t.Fatal("с неверным ключом запрос должен отклоняться")
	}
}

func (c *Client) portForTest() int {
	n, _ := strconv.Atoi(c.base[strings.LastIndex(c.base, ":")+1:])
	return n
}
