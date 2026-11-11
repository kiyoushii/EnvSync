package ui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kiyoushii/EnvSync/internal/core"
)

type fakeBackend struct {
	accepted, rejected []string
	applied            []string
	paused             []bool
	failWith           error
	journal            []core.Entry
}

func (f *fakeBackend) Self(context.Context) (Status, error) {
	return Status{Machine: "PC", At: time.Now(), Syncthing: SyncthingStatus{Running: true}, Guard: []GuardTrip{{Folder: "docs", NeedDeletes: 250, GlobalFiles: 300}}}, f.failWith
}
func (f *fakeBackend) Others(context.Context) ([]Status, error) { return nil, nil }
func (f *fakeBackend) Journal(n int) ([]core.Entry, error)      { return f.journal, nil }
func (f *fakeBackend) Conflicts(context.Context) ([]Conflict, error) {
	return nil, nil
}
func (f *fakeBackend) NotSynced(context.Context) ([]NotSyncedSection, error) {
	return core.NotSyncedStatic(), nil
}
func (f *fakeBackend) SettingsPlan(context.Context) ([]PlanItem, error) { return nil, nil }
func (f *fakeBackend) ApplySettings(_ context.Context, module string) (string, error) {
	f.applied = append(f.applied, "apply:"+module)
	return "применено", f.failWith
}
func (f *fakeBackend) KeepSettings(_ context.Context, module string) error {
	f.applied = append(f.applied, "keep:"+module)
	return f.failWith
}
func (f *fakeBackend) GuardAccept(_ context.Context, folder string) error {
	f.accepted = append(f.accepted, folder)
	return f.failWith
}
func (f *fakeBackend) GuardReject(_ context.Context, folder string) (string, error) {
	f.rejected = append(f.rejected, folder)
	return "возвращено", f.failWith
}
func (f *fakeBackend) SetPaused(_ context.Context, p bool) error {
	f.paused = append(f.paused, p)
	return f.failWith
}

const token = "secret-token-123"

func newServer(t *testing.T) (*httptest.Server, *fakeBackend, *Server) {
	t.Helper()
	fb := &fakeBackend{}
	s := &Server{B: fb, Token: token}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, fb, s
}

func do(t *testing.T, ts *httptest.Server, method, path string, hdr map[string]string, cookie bool) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(method, ts.URL+path, nil)
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
		} else {
			req.Header.Set(k, v)
		}
	}
	if cookie {
		req.AddCookie(&http.Cookie{Name: cookieName, Value: token})
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func TestAPIRequiresToken(t *testing.T) {
	ts, _, _ := newServer(t)
	if resp, _ := do(t, ts, "GET", "/api/status", nil, false); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("без токена: %d", resp.StatusCode)
	}
	if resp, _ := do(t, ts, "GET", "/api/status", map[string]string{"X-EnvSync-Token": "wrong"}, false); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("с неверным токеном: %d", resp.StatusCode)
	}
	for name, hdr := range map[string]map[string]string{"заголовок": {"X-EnvSync-Token": token}} {
		if resp, body := do(t, ts, "GET", "/api/status", hdr, false); resp.StatusCode != 200 || !strings.Contains(body, `"machine":"PC"`) {
			t.Fatalf("%s: %d %s", name, resp.StatusCode, body)
		}
	}
	if resp, _ := do(t, ts, "GET", "/api/status", nil, true); resp.StatusCode != 200 {
		t.Fatalf("с кукой: %d", resp.StatusCode)
	}
}

func TestTokenInURLBecomesCookieAndIsRemovedFromAddress(t *testing.T) {
	ts, _, _ := newServer(t)
	resp, _ := do(t, ts, "GET", "/?t="+token, nil, false)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("status=%d location=%q", resp.StatusCode, resp.Header.Get("Location"))
	}
	var c *http.Cookie
	for _, k := range resp.Cookies() {
		if k.Name == cookieName {
			c = k
		}
	}
	if c == nil || c.Value != token || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
		t.Fatalf("кука = %+v", c)
	}
	if resp, _ := do(t, ts, "GET", "/?t=wrong", nil, false); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("неверный токен в адресе: %d", resp.StatusCode)
	}
}

// Защита от DNS rebinding: чужое имя в Host отклоняется даже с верным токеном.
func TestForeignHostIsRejected(t *testing.T) {
	ts, _, s := newServer(t)
	s.port = 4242
	for _, host := range []string{"evil.example.com", "evil.example.com:4242", "127.0.0.1.evil.com:4242", "127.0.0.1:9999"} {
		if resp, _ := do(t, ts, "GET", "/api/status", map[string]string{"Host": host}, true); resp.StatusCode != http.StatusForbidden {
			t.Errorf("Host %q принят: %d", host, resp.StatusCode)
		}
	}
	for _, host := range []string{"127.0.0.1:4242", "localhost:4242"} {
		if resp, _ := do(t, ts, "GET", "/api/status", map[string]string{"Host": host}, true); resp.StatusCode != 200 {
			t.Errorf("Host %q отклонён: %d", host, resp.StatusCode)
		}
	}
}

// Защита от CSRF: изменяющие запросы только POST, со служебным заголовком и своим Origin.
func TestMutatingRequestsAreProtected(t *testing.T) {
	ts, fb, _ := newServer(t)
	path := "/api/guard/accept?folder=docs"
	own := ts.URL

	cases := []struct {
		name   string
		method string
		hdr    map[string]string
		want   int
	}{
		{"GET вместо POST", "GET", map[string]string{"X-Requested-With": "envsync"}, 405},
		{"нет служебного заголовка (простая форма с чужого сайта)", "POST", nil, 403},
		{"чужой Origin", "POST", map[string]string{"X-Requested-With": "envsync", "Origin": "https://evil.example.com"}, 403},
		{"всё верно", "POST", map[string]string{"X-Requested-With": "envsync", "Origin": own}, 200},
		{"всё верно, без Origin", "POST", map[string]string{"X-Requested-With": "envsync"}, 200},
	}
	for _, c := range cases {
		resp, body := do(t, ts, c.method, path, c.hdr, true)
		if resp.StatusCode != c.want {
			t.Errorf("%s: %d (ожидалось %d) %s", c.name, resp.StatusCode, c.want, body)
		}
	}
	if len(fb.accepted) != 2 {
		t.Fatalf("бэкенд вызван %d раз, ожидалось 2 (только валидные запросы): %v", len(fb.accepted), fb.accepted)
	}
	// Без токена изменяющий запрос не проходит вообще.
	if resp, _ := do(t, ts, "POST", path, map[string]string{"X-Requested-With": "envsync"}, false); resp.StatusCode != 401 {
		t.Fatalf("без токена: %d", resp.StatusCode)
	}
}

func TestDecisionsAndPauseReachBackend(t *testing.T) {
	ts, fb, _ := newServer(t)
	h := map[string]string{"X-Requested-With": "envsync"}
	if resp, body := do(t, ts, "POST", "/api/guard/reject?folder=docs", h, true); resp.StatusCode != 200 || !strings.Contains(body, "возвращено") {
		t.Fatalf("reject: %d %s", resp.StatusCode, body)
	}
	do(t, ts, "POST", "/api/pause?on=1", h, true)
	do(t, ts, "POST", "/api/pause?on=0", h, true)
	if len(fb.rejected) != 1 || fb.rejected[0] != "docs" || len(fb.paused) != 2 || !fb.paused[0] || fb.paused[1] {
		t.Fatalf("rejected=%v paused=%v", fb.rejected, fb.paused)
	}
	if resp, _ := do(t, ts, "POST", "/api/guard/accept", h, true); resp.StatusCode != 500 {
		t.Fatalf("без параметра folder ожидалась ошибка, получили %d", resp.StatusCode)
	}
}

// Подтверждение на ПК: кнопки «Применить» и «Оставить как здесь» доходят до бэкенда и защищены как все изменяющие запросы.
func TestSettingsConfirmationEndpoints(t *testing.T) {
	ts, fb, _ := newServer(t)
	h := map[string]string{"X-Requested-With": "envsync"}
	for _, path := range []string{"/api/settings/apply?module=appearance", "/api/settings/keep?module=explorer", "/api/settings/apply"} {
		if resp, body := do(t, ts, "POST", path, h, true); resp.StatusCode != 200 {
			t.Fatalf("%s: %d %s", path, resp.StatusCode, body)
		}
	}
	want := []string{"apply:appearance", "keep:explorer", "apply:"}
	if strings.Join(fb.applied, ",") != strings.Join(want, ",") {
		t.Fatalf("applied = %v", fb.applied)
	}
	// Без служебного заголовка (форма с чужого сайта) и без токена — нельзя.
	if resp, _ := do(t, ts, "POST", "/api/settings/apply", nil, true); resp.StatusCode != 403 {
		t.Fatalf("без заголовка: %d", resp.StatusCode)
	}
	if resp, _ := do(t, ts, "POST", "/api/settings/apply", h, false); resp.StatusCode != 401 {
		t.Fatalf("без токена: %d", resp.StatusCode)
	}
	// Имя модуля с путём или точками отклоняется.
	if resp, _ := do(t, ts, "POST", "/api/settings/apply?module=../x", h, true); resp.StatusCode != 500 {
		t.Fatalf("плохое имя модуля: %d", resp.StatusCode)
	}
	if len(fb.applied) != 3 {
		t.Fatalf("лишние вызовы бэкенда: %v", fb.applied)
	}
}

func TestBackendErrorBecomesJSONError(t *testing.T) {
	ts, fb, _ := newServer(t)
	fb.failWith = errors.New("Syncthing не запущен")
	resp, body := do(t, ts, "GET", "/api/status", nil, true)
	var out map[string]string
	_ = json.Unmarshal([]byte(body), &out)
	if resp.StatusCode != 500 || out["error"] != "Syncthing не запущен" {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
}

func TestJournalIsNewestFirstAndEmptyListsAreArrays(t *testing.T) {
	ts, fb, _ := newServer(t)
	fb.journal = []core.Entry{{Module: "a", Action: "first"}, {Module: "b", Action: "second"}}
	_, body := do(t, ts, "GET", "/api/journal?n=10", nil, true)
	if strings.Index(body, "second") > strings.Index(body, "first") {
		t.Fatalf("новые записи должны идти первыми: %s", body)
	}
	for _, path := range []string{"/api/conflicts", "/api/plan"} {
		if _, body := do(t, ts, "GET", path, nil, true); strings.TrimSpace(body) != "[]" {
			t.Errorf("%s: пустой список должен быть [], а не %q", path, body)
		}
	}
	_, status := do(t, ts, "GET", "/api/status", nil, true)
	if !strings.Contains(status, `"others":[]`) {
		t.Errorf("others должен быть []: %s", status)
	}
}

func TestStaticFilesAndSecurityHeaders(t *testing.T) {
	ts, _, _ := newServer(t)
	resp, body := do(t, ts, "GET", "/", nil, false)
	if resp.StatusCode != 200 || !strings.Contains(body, "<title>EnvSync</title>") {
		t.Fatalf("index: %d", resp.StatusCode)
	}
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") || strings.Contains(csp, "unsafe-inline") || !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("CSP = %q", csp)
	}
	if resp.Header.Get("X-Frame-Options") != "DENY" || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("нет защитных заголовков")
	}
	for _, f := range []string{"/app.js", "/style.css"} {
		if r, b := do(t, ts, "GET", f, nil, false); r.StatusCode != 200 || len(b) < 100 {
			t.Errorf("%s: %d", f, r.StatusCode)
		}
	}
}

// Страница не использует innerHTML: имена файлов приходят извне и не должны становиться разметкой.
func TestFrontendNeverUsesInnerHTML(t *testing.T) {
	data, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval("} {
		if strings.Contains(string(data), bad) {
			t.Errorf("app.js содержит %s", bad)
		}
	}
}

func TestNewTokenIsRandomAndLongEnough(t *testing.T) {
	a, _ := NewToken()
	b, _ := NewToken()
	if len(a) < 32 || a == b {
		t.Fatalf("токены: %q %q", a, b)
	}
}
