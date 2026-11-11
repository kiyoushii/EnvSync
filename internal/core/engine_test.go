package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeModule — «настройка» в памяти: одно строковое значение.
type fakeModule struct {
	name    string
	value   string
	failOn  string // если Apply получает это значение, он падает (после изменения)
	applied int
}

func (f *fakeModule) Name() string { return f.name }

func (f *fakeModule) Export(context.Context, string) (json.RawMessage, error) {
	return json.Marshal(map[string]string{"v": f.value})
}

func (f *fakeModule) want(desired json.RawMessage) string {
	var m map[string]string
	_ = json.Unmarshal(desired, &m)
	return m["v"]
}

func (f *fakeModule) Plan(_ context.Context, desired json.RawMessage, _ string) ([]Change, error) {
	if w := f.want(desired); w != f.value {
		return []Change{{Target: f.name, Before: f.value, After: w}}, nil
	}
	return nil, nil
}

func (f *fakeModule) Apply(_ context.Context, desired json.RawMessage, _ string, snap *Snapshot) error {
	if err := snap.PutJSON("before.json", f.value); err != nil {
		return err
	}
	f.value = f.want(desired)
	f.applied++
	if f.value == f.failOn {
		return errors.New("сбой при применении")
	}
	return nil
}

func (f *fakeModule) Rollback(_ context.Context, snap *Snapshot) error {
	return snap.GetJSON("before.json", &f.value)
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { c.t = c.t.Add(time.Minute); return c.t }

// machine — «машина» с модулями и собственным движком, общая папка состояний имитирует Syncthing.
func machine(t *testing.T, name, state string, clk *clock, mods ...Module) *Engine {
	t.Helper()
	base := t.TempDir()
	return &Engine{
		Machine: name,
		State:   StateDir{Root: state},
		Snaps:   &SnapshotStore{Dir: filepath.Join(base, "snapshots")},
		Journal: &Journal{Path: filepath.Join(base, "journal.jsonl")},
		Modules: mods,
		Now:     clk.now,
	}
}

func TestEngineSyncsLastWriterWinsWithoutPingPong(t *testing.T) {
	state, clk := t.TempDir(), &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	ctx := context.Background()
	a1, b1 := &fakeModule{name: "theme", value: "dark"}, &fakeModule{name: "theme", value: "dark"}
	A, B := machine(t, "A", state, clk, a1), machine(t, "B", state, clk, b1)

	// Обе машины запущены с одинаковым состоянием: менять нечего.
	for _, e := range []*Engine{A, B} {
		if run, _, err := e.Apply(ctx, nil); err != nil || run != nil {
			t.Fatalf("run=%v err=%v", run, err)
		}
	}

	// Пользователь сменил тему на A. B видит и применяет.
	a1.value = "light"
	plan, err := A.Plan(ctx, nil) // фиксирует правку A как новую
	if err != nil || len(plan) != 0 {
		t.Fatalf("у A нечего применять: %v err=%v", plan, err)
	}
	plan, err = B.Plan(ctx, nil)
	if err != nil || len(plan) != 1 || plan[0].Source != "A" || plan[0].Changes[0].After != "light" {
		t.Fatalf("plan B = %+v err=%v", plan, err)
	}
	run, _, err := B.Apply(ctx, nil)
	if err != nil || run == nil || b1.value != "light" {
		t.Fatalf("run=%v value=%s err=%v", run, b1.value, err)
	}

	// Нет пинг-понга: после применения у B состояние «его», A ничего не получает обратно.
	if plan, _ := A.Plan(ctx, nil); len(plan) != 0 {
		t.Fatalf("A получил своё же состояние обратно: %+v", plan)
	}
	if plan, _ := B.Plan(ctx, nil); len(plan) != 0 {
		t.Fatalf("B получил что-то ещё: %+v", plan)
	}

	// Побеждает последняя правка: B меняет позже, A применяет.
	b1.value = "dark"
	if plan, _ := B.Plan(ctx, nil); len(plan) != 0 {
		t.Fatal("у B нечего применять")
	}
	if _, _, err := A.Apply(ctx, nil); err != nil || a1.value != "dark" {
		t.Fatalf("A value=%s err=%v", a1.value, err)
	}
}

func TestLocalChangeNewerThanRemoteIsNotOverwritten(t *testing.T) {
	state, clk := t.TempDir(), &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	ctx := context.Background()
	a1, b1 := &fakeModule{name: "theme", value: "x"}, &fakeModule{name: "theme", value: "x"}
	A, B := machine(t, "A", state, clk, a1), machine(t, "B", state, clk, b1)
	A.Export(ctx)
	B.Export(ctx)

	a1.value = "from-A" // правка на A
	A.Export(ctx)
	b1.value = "from-B-later" // правка на B, позже
	if plan, _ := B.Plan(ctx, nil); len(plan) != 0 {
		t.Fatalf("локальная правка B новее, её нельзя затирать: %+v", plan)
	}
	if b1.value != "from-B-later" {
		t.Fatal("значение B изменилось")
	}
}

func TestMachinesIgnoresServiceFolders(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"A", "B", ".stfolder", ".stversions"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	got, err := StateDir{Root: root}.Machines()
	if err != nil || len(got) != 2 || got[0] != "A" || got[1] != "B" {
		t.Fatalf("machines = %v err=%v", got, err)
	}
}

// changedExport имитирует «машина уже была в сети, потом на ней что-то поменяли»: сначала базовый снимок
// с другими данными, затем настоящий. Второй снимок — правка, у него свежий ChangedAt.
func changedExport(ctx context.Context, e *Engine) {
	for _, m := range e.Modules {
		_, _ = e.State.Write(e.Machine, m.Name(), json.RawMessage(`{"v":"<до правки>"}`), e.now())
	}
	_, _ = e.Export(ctx)
}

// Первый снимок машины — это «база», а не правка: иначе машина, запустившая агент позже, выглядела бы новее
// и навязала бы свои настройки эталонной машине.
func TestFirstExportIsBaselineNotAChange(t *testing.T) {
	state, clk := t.TempDir(), &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	ctx := context.Background()
	pc, laptop := &fakeModule{name: "theme", value: "pc-theme"}, &fakeModule{name: "theme", value: "laptop-theme"}
	PC, Laptop := machine(t, "PC", state, clk, pc), machine(t, "LAPTOP", state, clk, laptop)

	PC.Export(ctx)
	Laptop.Export(ctx) // ноутбук запустил агент позже
	if sf, _ := Laptop.State.Read("LAPTOP", "theme"); !sf.ChangedAt.IsZero() {
		t.Fatalf("первый снимок должен быть базой (нулевое время), а не %v", sf.ChangedAt)
	}
	for name, e := range map[string]*Engine{"ПК": PC, "ноутбук": Laptop} {
		if plan, err := e.Plan(ctx, nil); err != nil || len(plan) != 0 {
			t.Fatalf("%s: при двух базовых снимках ничего не должно применяться само: %+v err=%v", name, plan, err)
		}
	}
	if pc.value != "pc-theme" || laptop.value != "laptop-theme" {
		t.Fatal("значения изменились")
	}
}

// Первичное выравнивание делается явно: «взять настройки с эталонной машины».
func TestPlanFromForcesSourceMachine(t *testing.T) {
	state, clk := t.TempDir(), &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	ctx := context.Background()
	pc, laptop := &fakeModule{name: "theme", value: "pc-theme"}, &fakeModule{name: "theme", value: "laptop-theme"}
	PC, Laptop := machine(t, "PC", state, clk, pc), machine(t, "LAPTOP", state, clk, laptop)
	PC.Export(ctx)
	Laptop.Export(ctx)

	plan, err := Laptop.PlanFrom(ctx, "pc", nil) // имя машины без учёта регистра
	if err != nil || len(plan) != 1 || plan[0].Source != "PC" || plan[0].Changes[0].After != "pc-theme" {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	if _, _, err := Laptop.ApplyFrom(ctx, "PC", nil); err != nil || laptop.value != "pc-theme" {
		t.Fatalf("value=%s err=%v", laptop.value, err)
	}
	// После выравнивания ПК не получает ничего обратно: данные те же.
	if plan, _ := PC.Plan(ctx, nil); len(plan) != 0 {
		t.Fatalf("ПК получил план после выравнивания: %+v", plan)
	}
	if _, err := Laptop.PlanFrom(ctx, "NOBODY", nil); err == nil {
		t.Fatal("неизвестная машина-источник должна быть ошибкой")
	}
	if _, err := Laptop.PlanFrom(ctx, "LAPTOP", nil); err == nil {
		t.Fatal("взять настройки у самой себя нельзя")
	}
}

func TestUnchangedExportKeepsChangedAt(t *testing.T) {
	state, clk := t.TempDir(), &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	A := machine(t, "A", state, clk, &fakeModule{name: "m", value: "v"})
	A.Export(context.Background())
	first, _ := A.State.Read("A", "m")
	changed, _ := A.Export(context.Background())
	second, _ := A.State.Read("A", "m")
	if len(changed) != 0 || !first.ChangedAt.Equal(second.ChangedAt) {
		t.Fatalf("повторный снимок без изменений поменял ChangedAt: %v → %v, changed=%v", first.ChangedAt, second.ChangedAt, changed)
	}
}

// Цикл «применил → проверил → откатил».
func TestApplyVerifyRollback(t *testing.T) {
	state, clk := t.TempDir(), &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	ctx := context.Background()
	a1, b1 := &fakeModule{name: "theme", value: "new"}, &fakeModule{name: "theme", value: "old"}
	A, B := machine(t, "A", state, clk, a1), machine(t, "B", state, clk, b1)
	B.Export(ctx)
	changedExport(ctx, A) // на A была правка: A новее

	run, _, err := B.Apply(ctx, nil)
	if err != nil || b1.value != "new" {
		t.Fatalf("применение: value=%s err=%v", b1.value, err)
	}
	if run2, err := B.Rollback(ctx, ""); err != nil || run2.ID != run.ID || b1.value != "old" {
		t.Fatalf("откат: value=%s err=%v", b1.value, err)
	}
	if _, err := B.Rollback(ctx, run.ID); err == nil {
		t.Fatal("повторный откат того же запуска должен быть отклонён")
	}
	// После отката B не получает снова то же состояние от A: его откат новее.
	if plan, _ := B.Plan(ctx, nil); len(plan) != 0 {
		t.Fatalf("после отката план не пуст: %+v", plan)
	}
}

func TestFailureRollsBackWholeRun(t *testing.T) {
	state, clk := t.TempDir(), &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	ctx := context.Background()
	a1, a2 := &fakeModule{name: "one", value: "A1"}, &fakeModule{name: "two", value: "A2"}
	b1, b2 := &fakeModule{name: "one", value: "old1"}, &fakeModule{name: "two", value: "old2", failOn: "A2"}
	A, B := machine(t, "A", state, clk, a1, a2), machine(t, "B", state, clk, b1, b2)
	B.Export(ctx)
	changedExport(ctx, A)

	_, _, err := B.Apply(ctx, nil)
	if err == nil {
		t.Fatal("ожидали ошибку второго модуля")
	}
	if b1.value != "old1" || b2.value != "old2" {
		t.Fatalf("после сбоя значения должны вернуться: one=%s two=%s", b1.value, b2.value)
	}
	entries, _ := B.Journal.Recent(0)
	var sawErr, sawRollback bool
	for _, e := range entries {
		sawErr = sawErr || e.Action == "error"
		sawRollback = sawRollback || e.Action == "rollback"
	}
	if !sawErr || !sawRollback {
		t.Fatalf("в журнале должны быть ошибка и откат: %v", entries)
	}
}

// brokenPlanModule не может построить план (например, нужный файл ещё не доехал).
type brokenPlanModule struct{ fakeModule }

func (b *brokenPlanModule) Plan(context.Context, json.RawMessage, string) ([]Change, error) {
	return nil, errors.New("картинка ещё не доехала")
}

func TestBrokenModulePlanDoesNotStopOthers(t *testing.T) {
	state, clk := t.TempDir(), &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	ctx := context.Background()
	a1, a2 := &fakeModule{name: "good", value: "new"}, &fakeModule{name: "broken", value: "new"}
	b1 := &fakeModule{name: "good", value: "old"}
	b2 := &brokenPlanModule{fakeModule{name: "broken", value: "old"}}
	A, B := machine(t, "A", state, clk, a1, a2), machine(t, "B", state, clk, b1, b2)
	B.Export(ctx)
	changedExport(ctx, A)

	plan, err := B.Plan(ctx, nil)
	if err != nil || len(plan) != 2 || plan[1].Err == nil {
		t.Fatalf("plan=%+v err=%v: проблема модуля должна быть видна в плане", plan, err)
	}
	if _, _, err := B.Apply(ctx, nil); err != nil || b1.value != "new" || b2.value != "old" {
		t.Fatalf("good=%s broken=%s err=%v", b1.value, b2.value, err)
	}
}

func TestOnlyFilterAndJournalReadable(t *testing.T) {
	state, clk := t.TempDir(), &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	ctx := context.Background()
	mk := func(v string) (*fakeModule, *fakeModule) {
		return &fakeModule{name: "one", value: v}, &fakeModule{name: "two", value: v}
	}
	a1, a2 := mk("new")
	b1, b2 := mk("old")
	A, B := machine(t, "A", state, clk, a1, a2), machine(t, "B", state, clk, b1, b2)
	B.Export(ctx)
	changedExport(ctx, A)

	if _, _, err := B.Apply(ctx, []string{"two"}); err != nil {
		t.Fatal(err)
	}
	if b1.value != "old" || b2.value != "new" {
		t.Fatalf("one=%s two=%s: должен был измениться только two", b1.value, b2.value)
	}
	entries, _ := B.Journal.Recent(1)
	if len(entries) != 1 || fmt.Sprint(entries[0]) == "" {
		t.Fatal("журнал пуст")
	}
}
