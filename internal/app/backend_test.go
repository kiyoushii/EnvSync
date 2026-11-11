package app

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kiyoushii/EnvSync/internal/core"
	"github.com/kiyoushii/EnvSync/internal/transport"
	"github.com/kiyoushii/EnvSync/internal/ui"
)

func newBackend(t *testing.T, machine string, tsJSON string) *Backend {
	t.Helper()
	dir := t.TempDir()
	s, err := transport.LoadSettings(dir)
	if err != nil {
		t.Fatal(err)
	}
	ts := &transport.Tailscale{Bin: "tailscale", Run: func(context.Context, string, ...string) ([]byte, error) {
		if tsJSON == "" {
			return nil, errors.New("tailscale не найден")
		}
		return []byte(tsJSON), nil
	}}
	return &Backend{
		S: s, TS: ts, Machine: machine, Version: "test",
		Guard: &transport.Guard{StatePath: s.GuardFile()},
		Audit: &core.Journal{Path: s.JournalFile()},
	}
}

const tsOK = `{"BackendState":"Running","Self":{"HostName":"PC","TailscaleIPs":["100.1.1.1"]},
 "Peer":{"k":{"HostName":"LAPTOP","TailscaleIPs":["100.1.1.2"],"Online":true}}}`

func TestSelfWithoutSyncthingStillReportsTailscale(t *testing.T) {
	b := newBackend(t, "PC", tsOK)
	st, err := b.Self(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Syncthing.Running || st.Tailscale.State != "Running" || st.Tailscale.IP != "100.1.1.1" || len(st.Tailscale.Peers) != 1 || !st.Tailscale.Peers[0].Online {
		t.Fatalf("status = %+v", st)
	}
	// Пустые списки, а не null: интерфейс не должен падать на отсутствующих данных.
	data, _ := json.Marshal(st)
	if strings.Contains(string(data), "null") {
		t.Fatalf("в JSON есть null: %s", data)
	}
}

func TestSelfWhenTailscaleIsUnavailable(t *testing.T) {
	st, err := newBackend(t, "PC", "").Self(context.Background())
	if err != nil || !strings.Contains(st.Tailscale.State, "недоступен") {
		t.Fatalf("st=%+v err=%v", st, err)
	}
}

// Статус одной машины доезжает до другой через папку состояний.
func TestStatusTravelsThroughStateDir(t *testing.T) {
	ctx := context.Background()
	laptop := newBackend(t, "LAPTOP", tsOK)
	pc := newBackend(t, "PC", tsOK)
	pc.S, _ = transport.LoadSettings(laptop.S.BaseDir()) // общая папка состояний (её на деле синкает Syncthing)

	if err := laptop.ExportStatus(ctx); err != nil {
		t.Fatal(err)
	}
	if err := pc.ExportStatus(ctx); err != nil {
		t.Fatal(err)
	}
	others, err := pc.Others(ctx)
	if err != nil || len(others) != 1 || others[0].Machine != "LAPTOP" {
		t.Fatalf("others=%+v err=%v", others, err)
	}
	if time.Since(others[0].At) > time.Minute {
		t.Errorf("снимок слишком старый: %v", others[0].At)
	}
	// Своё состояние в чужих не показывается.
	for _, o := range others {
		if o.Machine == "PC" {
			t.Error("своя машина попала в список других")
		}
	}
}

func TestNotSyncedHasStaticAndDynamicParts(t *testing.T) {
	b := newBackend(t, "PC", tsOK)
	secs, err := b.NotSynced(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	var all strings.Builder
	for _, s := range secs {
		titles = append(titles, s.Title)
		for _, it := range s.Items {
			all.WriteString(it.What + " " + it.Why + "\n")
		}
	}
	text := all.String()
	for _, want := range []string{"Драйверы", "Лицензия и активация Windows", "node_modules", ".git", "Раскладки клавиатуры"} {
		if !strings.Contains(text, want) {
			t.Errorf("в экране «не синхронизируется» нет %q", want)
		}
	}
	if len(titles) < 4 {
		t.Errorf("разделов %d: %v", len(titles), titles)
	}
}

func TestNotSyncedShowsSkippedSecretsFromState(t *testing.T) {
	b := newBackend(t, "PC", tsOK)
	data, _ := json.Marshal(map[string]any{"apps": map[string]any{"git": map[string]any{
		"files": map[string]any{}, "skipped": map[string]string{"home/.gitconfig": "похоже на секрет (токен, пароль или ключ)"},
	}}})
	if _, err := (core.StateDir{Root: b.S.StateDir()}).Write("PC", "appconfig", data, time.Now()); err != nil {
		t.Fatal(err)
	}
	secs, _ := b.NotSynced(context.Background())
	var found bool
	for _, s := range secs {
		for _, it := range s.Items {
			found = found || (strings.Contains(it.What, ".gitconfig") && strings.Contains(it.Why, "секрет"))
		}
	}
	if !found {
		t.Fatal("пропущенный как секрет .gitconfig должен быть виден на экране")
	}
}

func TestConflictNameParsing(t *testing.T) {
	cases := map[string]string{
		"doc.sync-conflict-20260101-120000-ABCDEFG.txt": "doc.txt",
		"архив.sync-conflict-20260101-120000-ABCDEFG":   "архив",
		"a.b.tar.sync-conflict-20260101-120000.gz":      "a.b.tar.gz", // Syncthing вставляет пометку перед последним расширением
	}
	for name, want := range cases {
		m := conflictRe.FindStringSubmatch(name)
		if m == nil {
			t.Errorf("%s не распознан", name)
			continue
		}
		if got := m[1] + m[2]; got != want {
			t.Errorf("%s: оригинал %q, ожидалось %q", name, got, want)
		}
	}
	for _, name := range []string{"обычный файл.txt", "sync-conflict.txt", "x.sync-conflict-bad.txt"} {
		if conflictRe.MatchString(name) {
			t.Errorf("%s принят за конфликт", name)
		}
	}
}

// valueModule — «настройка» в памяти для проверки синхронизации ПК ↔ ноутбук.
type valueModule struct{ value string }

func (m *valueModule) Name() string { return "theme" }
func (m *valueModule) Export(context.Context, string) (json.RawMessage, error) {
	return json.Marshal(map[string]string{"v": m.value})
}
func (m *valueModule) want(d json.RawMessage) string {
	var st map[string]string
	_ = json.Unmarshal(d, &st)
	return st["v"]
}
func (m *valueModule) Plan(_ context.Context, d json.RawMessage, _ string) ([]core.Change, error) {
	if w := m.want(d); w != m.value {
		return []core.Change{{Target: "theme", Before: m.value, After: w}}, nil
	}
	return nil, nil
}
func (m *valueModule) Apply(_ context.Context, d json.RawMessage, _ string, snap *core.Snapshot) error {
	if err := snap.PutJSON("before.json", m.value); err != nil {
		return err
	}
	m.value = m.want(d)
	return nil
}
func (m *valueModule) Rollback(_ context.Context, snap *core.Snapshot) error {
	return snap.GetJSON("before.json", &m.value)
}

// Режим «ноутбук подтягивается сам, ПК с подтверждением».
func TestTwoWaySyncAutoOnLaptopConfirmOnPC(t *testing.T) {
	ctx := context.Background()
	state := t.TempDir()
	newMachine := func(name string, auto bool, mod *valueModule) *Backend {
		b := newBackend(t, name, tsOK)
		eng := &core.Engine{
			Machine: name, State: core.StateDir{Root: state},
			Snaps:   &core.SnapshotStore{Dir: filepath.Join(t.TempDir(), "snaps")},
			Journal: b.Audit, Modules: []core.Module{mod},
		}
		b.Engine = func() (*core.Engine, error) { return eng, nil }
		b.AutoApply = auto
		return b
	}
	pcMod, laptopMod := &valueModule{"dark"}, &valueModule{"light"}
	pc, laptop := newMachine("PC", false, pcMod), newMachine("LAPTOP", true, laptopMod)
	tick := func(b *Backend) {
		t.Helper()
		if err := b.SettingsTick(ctx); err != nil {
			t.Fatal(err)
		}
	}

	// Обе машины снимают базу: ничего само не применяется, хотя значения разные.
	tick(pc)
	tick(laptop)
	if laptopMod.value != "light" || pc.Pending() != 0 {
		t.Fatalf("база не должна применяться: laptop=%s pending=%d", laptopMod.value, pc.Pending())
	}
	// Первичное выравнивание ноутбука с ПК (явно, как в bootstrap): ноутбук стал как ПК.
	eng, _ := laptop.Engine()
	if _, _, err := eng.ApplyFrom(ctx, "PC", nil); err != nil || laptopMod.value != "dark" {
		t.Fatalf("выравнивание: %s %v", laptopMod.value, err)
	}

	// Правка на ПК → ноутбук применяет сам при следующем шаге.
	pcMod.value = "blue"
	tick(pc)
	tick(laptop)
	if laptopMod.value != "blue" {
		t.Fatalf("ноутбук должен подтянуть правку ПК сам: %s", laptopMod.value)
	}
	tick(pc)
	if pc.Pending() != 0 {
		t.Fatalf("после того как ноутбук подтянулся, на ПК ничего не должно ждать: %d", pc.Pending())
	}

	// Правка на ноутбуке → на ПК ждёт подтверждения и сама не применяется.
	laptopMod.value = "green"
	tick(laptop)
	tick(pc)
	if pcMod.value != "blue" || pc.Pending() != 1 {
		t.Fatalf("ПК не должен применять сам: pc=%s pending=%d", pcMod.value, pc.Pending())
	}
	st, _ := pc.Self(ctx)
	if st.PendingSettings != 1 || st.AutoApply {
		t.Fatalf("статус ПК: pending=%d auto=%v", st.PendingSettings, st.AutoApply)
	}

	// Подтверждение на ПК → применено.
	if msg, err := pc.ApplySettings(ctx, "theme"); err != nil || pcMod.value != "green" || pc.Pending() != 0 {
		t.Fatalf("подтверждение: pc=%s pending=%d msg=%q err=%v", pcMod.value, pc.Pending(), msg, err)
	}

	// Ещё одна правка на ноутбуке, но на ПК «Оставить как здесь» → ноутбук возвращается к варианту ПК.
	laptopMod.value = "red"
	tick(laptop)
	tick(pc)
	if pc.Pending() != 1 {
		t.Fatalf("правка ноутбука должна ждать: %d", pc.Pending())
	}
	if err := pc.KeepSettings(ctx, "theme"); err != nil {
		t.Fatal(err)
	}
	tick(laptop)
	if laptopMod.value != "green" || pcMod.value != "green" || pc.Pending() != 0 {
		t.Fatalf("после «оставить как на ПК»: laptop=%s pc=%s pending=%d", laptopMod.value, pcMod.value, pc.Pending())
	}
}

func TestJournalAndPlanWithoutEngine(t *testing.T) {
	b := newBackend(t, "PC", tsOK)
	if entries, err := b.Journal(10); err != nil || len(entries) != 0 {
		t.Fatalf("entries=%v err=%v", entries, err)
	}
	if _, err := b.SettingsPlan(context.Background()); err == nil {
		t.Fatal("без движка план недоступен и должен сообщать об этом")
	}
	var _ ui.Backend = b
	_ = filepath.Join
}
