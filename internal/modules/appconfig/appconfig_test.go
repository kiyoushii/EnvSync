package appconfig

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kiyoushii/EnvSync/internal/core"
)

// machine — «компьютер» с собственными папками home/appdata/localappdata.
type machine struct {
	roots map[Root]string
}

func newMachine(t *testing.T) machine {
	t.Helper()
	base := t.TempDir()
	m := machine{roots: map[Root]string{
		Home: filepath.Join(base, "home"), AppData: filepath.Join(base, "appdata"), LocalAppData: filepath.Join(base, "local"),
	}}
	for _, d := range m.roots {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

func (m machine) put(t *testing.T, root Root, rel, content string) string {
	t.Helper()
	p := filepath.Join(m.roots[root], filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func (m machine) read(root Root, rel string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(m.roots[root], filepath.FromSlash(rel)))
	return string(data), err == nil
}

var apps = []App{
	{ID: "git", Files: []FileSpec{{Root: Home, Path: ".gitconfig"}}},
	{ID: "vscode", Files: []FileSpec{
		{Root: AppData, Path: "Code/User/settings.json"},
		{Root: AppData, Path: "Code/User/snippets", Dir: true},
	}, BusyProcesses: []string{"Code.exe"}},
}

func snap(t *testing.T) *core.Snapshot {
	t.Helper()
	store := &core.SnapshotStore{Dir: filepath.Join(t.TempDir(), "snaps")}
	run, err := store.NewRun("PC", "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	s, err := run.Snapshot("appconfig")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func export(t *testing.T, a machine) (json.RawMessage, string) {
	t.Helper()
	assets := filepath.Join(t.TempDir(), "assets")
	data, err := New(apps, a.roots, nil).Export(context.Background(), assets)
	if err != nil {
		t.Fatal(err)
	}
	return data, assets
}

// Цикл «применил → проверил → откатил» на конфигах двух машин.
func TestApplyVerifyRollback(t *testing.T) {
	ctx := context.Background()
	a, b := newMachine(t), newMachine(t)
	a.put(t, Home, ".gitconfig", "[user]\n\tname = Dev\n")
	a.put(t, AppData, "Code/User/settings.json", `{"editor.fontSize": 14}`)
	a.put(t, AppData, "Code/User/snippets/go.json", `{"main": {}}`)
	a.put(t, AppData, "Code/User/snippets/deep/py.json", `{"x": 1}`)
	state, assets := export(t, a)

	// На B: .gitconfig другой, settings.json нет совсем, один сниппет уже совпадает.
	b.put(t, Home, ".gitconfig", "[user]\n\tname = Old\n")
	b.put(t, AppData, "Code/User/snippets/go.json", `{"main": {}}`)
	mB := New(apps, b.roots, nil)

	changes, err := mB.Plan(ctx, state, assets)
	if err != nil || len(changes) != 3 { // gitconfig, settings.json, deep/py.json; go.json совпал
		t.Fatalf("changes=%+v err=%v", changes, err)
	}
	sn := snap(t)
	if err := mB.Apply(ctx, state, assets, sn); err != nil {
		t.Fatal(err)
	}
	// Проверил: файлы стали такими же, повторный план пуст.
	for rel, root := range map[string]Root{".gitconfig": Home, "Code/User/settings.json": AppData, "Code/User/snippets/deep/py.json": AppData} {
		got, ok := b.read(root, rel)
		want, _ := a.read(root, rel)
		if !ok || got != want {
			t.Errorf("%s: %q, ожидалось %q", rel, got, want)
		}
	}
	if changes, _ := mB.Plan(ctx, state, assets); len(changes) != 0 {
		t.Fatalf("повторный план не пуст: %+v", changes)
	}
	// Откатил: изменённый вернулся, созданные удалены, совпавший не тронут.
	if err := mB.Rollback(ctx, sn); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.read(Home, ".gitconfig"); got != "[user]\n\tname = Old\n" {
		t.Errorf(".gitconfig после отката: %q", got)
	}
	for _, rel := range []string{"Code/User/settings.json", "Code/User/snippets/deep/py.json"} {
		if _, ok := b.read(AppData, rel); ok {
			t.Errorf("созданный файл %s не удалён при откате", rel)
		}
	}
	if got, _ := b.read(AppData, "Code/User/snippets/go.json"); got != `{"main": {}}` {
		t.Errorf("совпавший файл изменился: %q", got)
	}
}

func TestNothingIsEverDeleted(t *testing.T) {
	ctx := context.Background()
	a, b := newMachine(t), newMachine(t)
	a.put(t, Home, ".gitconfig", "a")
	state, assets := export(t, a)
	b.put(t, AppData, "Code/User/settings.json", "только на B")
	if err := New(apps, b.roots, nil).Apply(ctx, state, assets, snap(t)); err != nil {
		t.Fatal(err)
	}
	if got, ok := b.read(AppData, "Code/User/settings.json"); !ok || got != "только на B" {
		t.Fatal("файл, которого нет на источнике, не должен удаляться")
	}
}

func TestBusyApplicationIsSkipped(t *testing.T) {
	ctx := context.Background()
	a, b := newMachine(t), newMachine(t)
	a.put(t, Home, ".gitconfig", "[user]\n\tname = New\n")
	a.put(t, AppData, "Code/User/settings.json", `{"new": true}`)
	state, assets := export(t, a)
	b.put(t, AppData, "Code/User/settings.json", `{"old": true}`)

	running := true
	mB := New(apps, b.roots, func() (map[string]bool, error) {
		if running {
			return map[string]bool{"code.exe": true}, nil
		}
		return map[string]bool{}, nil
	})
	changes, err := mB.Plan(ctx, state, assets)
	if err != nil {
		t.Fatal(err)
	}
	var skipped, work int
	for _, c := range changes {
		if c.Skip {
			skipped++
			if !strings.Contains(c.After, "Code.exe") {
				t.Errorf("причина пропуска не названа: %q", c.After)
			}
		} else {
			work++
		}
	}
	if skipped != 1 || work != 1 {
		t.Fatalf("skipped=%d work=%d: %+v", skipped, work, changes)
	}
	if err := mB.Apply(ctx, state, assets, snap(t)); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.read(AppData, "Code/User/settings.json"); got != `{"old": true}` {
		t.Fatalf("конфиг запущенного приложения изменён: %q", got)
	}
	if got, _ := b.read(Home, ".gitconfig"); !strings.Contains(got, "New") {
		t.Fatalf("конфиг незапущенного приложения должен был примениться: %q", got)
	}
	running = false // приложение закрыли: теперь можно
	if err := mB.Apply(ctx, state, assets, snap(t)); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.read(AppData, "Code/User/settings.json"); got != `{"new": true}` {
		t.Fatalf("после закрытия приложения конфиг не применился: %q", got)
	}
}

func TestSecretsAreNotExported(t *testing.T) {
	a := newMachine(t)
	a.put(t, Home, ".gitconfig", "[credential]\n\thelper = manager\n[github]\n\ttoken = ghp_abcdefghijklmnopqrstuvwxyz0123\n")
	a.put(t, AppData, "Code/User/settings.json", `{"editor.fontSize": 14, "myservice.apiKey": "abcd1234efgh5678"}`)
	a.put(t, AppData, "Code/User/snippets/ok.json", `{"fine": "just a snippet about tokens and passwords in docs"}`)
	a.put(t, AppData, "Code/User/snippets/key.json", "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n")
	state, assets := export(t, a)

	var st state2
	_ = json.Unmarshal(state, &st)
	if _, ok := st.Apps["git"].Files["home/.gitconfig"]; ok {
		t.Error(".gitconfig с токеном не должен экспортироваться")
	}
	if _, ok := st.Apps["vscode"].Files["appdata/Code/User/settings.json"]; ok {
		t.Error("settings.json с apiKey не должен экспортироваться")
	}
	if _, ok := st.Apps["vscode"].Files["appdata/Code/User/snippets/key.json"]; ok {
		t.Error("закрытый ключ не должен экспортироваться")
	}
	if _, ok := st.Apps["vscode"].Files["appdata/Code/User/snippets/ok.json"]; !ok {
		t.Error("обычный сниппет, где слова token/password только в тексте, должен экспортироваться")
	}
	if len(st.Apps["git"].Skipped) != 1 || !strings.Contains(st.Apps["git"].Skipped["home/.gitconfig"], "секрет") {
		t.Errorf("причина пропуска не записана: %+v", st.Apps["git"].Skipped)
	}
	// Секретов нет и в assets (то, что реально поедет по сети).
	_ = filepath.WalkDir(assets, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			data, _ := os.ReadFile(p)
			if LooksSecret(data) {
				t.Errorf("секрет попал в assets: %s", p)
			}
		}
		return nil
	})
}

type state2 struct {
	Apps map[string]appState `json:"apps"`
}

func TestLooksSecretDoesNotFlagOrdinaryConfigs(t *testing.T) {
	for _, ok := range []string{
		`{"editor.fontSize": 14, "files.autoSave": "afterDelay"}`,
		"[user]\n\tname = Dev\n\temail = dev@example.com\n[core]\n\tautocrlf = true\n",
		`{"profiles": {"list": [{"name": "PowerShell", "commandline": "pwsh.exe"}]}}`,
	} {
		if LooksSecret([]byte(ok)) {
			t.Errorf("обычный конфиг принят за секрет: %q", ok)
		}
	}
}

// Состояние пришло с другой машины: доверять ключам слепо нельзя.
func TestHostileKeysAreRejected(t *testing.T) {
	ctx := context.Background()
	b := newMachine(t)
	mB := New(apps, b.roots, nil)
	for _, key := range []string{"home/../evil.txt", "home/../../Windows/x", "unknown/x", "home/", "nokey"} {
		desired, _ := json.Marshal(state2{Apps: map[string]appState{"git": {Files: map[string]fileInfo{key: {SHA256: "00000000", Size: 1}}}}})
		if _, err := mB.Plan(ctx, desired, t.TempDir()); err == nil {
			t.Errorf("ключ %q должен быть отклонён", key)
		}
	}
}

func TestCorruptOrMissingAssetFailsPlan(t *testing.T) {
	ctx := context.Background()
	a, b := newMachine(t), newMachine(t)
	a.put(t, Home, ".gitconfig", "x")
	state, assets := export(t, a)
	mB := New(apps, b.roots, nil)

	asset := filepath.Join(assets, "git", "home", ".gitconfig")
	if err := os.WriteFile(asset, []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := mB.Plan(ctx, state, assets); err == nil || !strings.Contains(err.Error(), "хеш") {
		t.Fatalf("err=%v", err)
	}
	_ = os.Remove(asset)
	if _, err := mB.Plan(ctx, state, assets); err == nil || !strings.Contains(err.Error(), "не доехал") {
		t.Fatalf("err=%v", err)
	}
}

func TestExportRemovesStaleAssetsAndSkipsLargeFiles(t *testing.T) {
	ctx := context.Background()
	a := newMachine(t)
	a.put(t, Home, ".gitconfig", "x")
	assets := filepath.Join(t.TempDir(), "assets")
	m := New(apps, a.roots, nil)
	if _, err := m.Export(ctx, assets); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(a.roots[Home], ".gitconfig")); err != nil {
		t.Fatal(err)
	}
	a.put(t, AppData, "Code/User/settings.json", strings.Repeat("x", MaxFileSize+1))
	data, err := m.Export(ctx, assets)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(assets, "git", "home", ".gitconfig")); err == nil {
		t.Error("устаревшая копия .gitconfig осталась в assets")
	}
	if !strings.Contains(string(data), "больше 1 МБ") {
		t.Errorf("большой файл должен быть в списке пропущенных: %s", data)
	}
}

func TestSkipDirsAreNotExported(t *testing.T) {
	a := newMachine(t)
	a.put(t, Home, ".claude/skills/mine/SKILL.md", "my skill")
	a.put(t, Home, ".claude/skills/synced/acct-123/SKILL.md", "managed by the account")
	a.put(t, Home, ".claude/skills/nested/Synced/x.md", "also skipped, any depth and case")
	app := App{ID: "claude", Files: []FileSpec{{Root: Home, Path: ".claude/skills", Dir: true, SkipDirs: []string{"synced"}}}}
	data, err := New([]App{app}, a.roots, nil).Export(context.Background(), filepath.Join(t.TempDir(), "assets"))
	if err != nil {
		t.Fatal(err)
	}
	var st state2
	_ = json.Unmarshal(data, &st)
	files := st.Apps["claude"].Files
	if len(files) != 1 {
		t.Fatalf("ожидался один файл, есть %v", files)
	}
	if _, ok := files["home/.claude/skills/mine/SKILL.md"]; !ok {
		t.Fatalf("свой навык не экспортирован: %v", files)
	}
}

func TestDefaultAppsAreSane(t *testing.T) {
	seen := map[string]bool{}
	for _, app := range DefaultApps() {
		if seen[app.ID] {
			t.Errorf("повтор приложения %s", app.ID)
		}
		seen[app.ID] = true
		for _, f := range app.Files {
			if strings.Contains(f.Path, "..") || strings.HasPrefix(f.Path, "/") || strings.Contains(f.Path, `\`) {
				t.Errorf("%s: плохой путь %q", app.ID, f.Path)
			}
			p := strings.ToLower(f.Path)
			if strings.Contains(p, "credential") || strings.Contains(p, "settings.json") && app.ID == "claude" {
				t.Errorf("%s: файл с секретами в белом списке: %s", app.ID, f.Path)
			}
		}
	}
	for _, want := range []string{"git", "vscode", "windows-terminal", "claude"} {
		if !seen[want] {
			t.Errorf("нет приложения %s", want)
		}
	}
}

func TestRunningProcessesFindsOurself(t *testing.T) {
	procs, err := RunningProcesses()
	if err != nil {
		t.Skip("tasklist недоступен:", err)
	}
	if len(procs) == 0 {
		t.Fatal("список процессов пуст")
	}
}
