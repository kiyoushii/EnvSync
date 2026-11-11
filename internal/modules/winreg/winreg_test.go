package winreg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kiyoushii/EnvSync/internal/core"
)

// fakeBackend — реестр в памяти. failSetOn — имя значения, на котором Set падает.
type fakeBackend struct {
	vals      map[string]*Value
	failSetOn string
	sets      int
}

func newFake() *fakeBackend { return &fakeBackend{vals: map[string]*Value{}} }

func k(key, name string) string { return key + "|" + name }

func (f *fakeBackend) Get(key, name string) (*Value, error) { return f.vals[k(key, name)], nil }
func (f *fakeBackend) Set(key, name string, v Value) error {
	if name == f.failSetOn {
		return errors.New("отказано в доступе")
	}
	f.sets++
	f.vals[k(key, name)] = &v
	return nil
}
func (f *fakeBackend) Delete(key, name string) error { delete(f.vals, k(key, name)); return nil }

type countNotifier struct{ n int }

func (c *countNotifier) Notify(context.Context) error { c.n++; return nil }

var testSpecs = []Spec{
	{ID: "a.dword", Key: `K\A`, Name: "Num", Type: "dword"},
	{ID: "a.string", Key: `K\A`, Name: "Str", Type: "string"},
	{ID: "a.bin", Key: `K\A`, Name: "Bin", Type: "binary"},
	{ID: "a.opt", Key: `K\A`, Name: "Optional", Type: "dword", Apply: Explorer},
}

func desired(t *testing.T, vals map[string]*Value) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(state{Values: vals})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func dw(n string) *Value { return &Value{Type: "dword", Data: n} }

func TestExportReadsAllWhitelistedKeys(t *testing.T) {
	be := newFake()
	be.vals[k(`K\A`, "Num")] = dw("5")
	m := New("m", testSpecs, be, nil)
	data, err := m.Export(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	var st state
	_ = json.Unmarshal(data, &st)
	if len(st.Values) != 4 || st.Values["a.dword"].Data != "5" || st.Values["a.str"] != nil {
		t.Fatalf("export = %+v", st.Values)
	}
	if v, present := st.Values["a.opt"]; !present || v != nil {
		t.Fatalf("отсутствующее значение должно передаваться как null: %+v", st.Values)
	}
}

func TestPlanShowsOnlyDifferencesAndHints(t *testing.T) {
	be := newFake()
	be.vals[k(`K\A`, "Num")] = dw("1")
	be.vals[k(`K\A`, "Str")] = &Value{Type: "string", Data: "same"}
	m := New("m", testSpecs, be, nil)

	changes, err := m.Plan(context.Background(), desired(t, map[string]*Value{
		"a.dword":  dw("2"),
		"a.string": {Type: "string", Data: "same"},
		"a.opt":    dw("1"),
	}), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 {
		t.Fatalf("changes = %+v", changes)
	}
	if !strings.HasSuffix(changes[0].Target, `K\A\Num`) || changes[0].Before != "dword:1" || changes[0].After != "dword:2" {
		t.Errorf("change[0] = %+v", changes[0])
	}
	if changes[1].Restart != "explorer" || changes[1].Before != "<нет значения>" {
		t.Errorf("change[1] = %+v", changes[1])
	}
}

func TestWhitelistIsEnforcedAndTypesChecked(t *testing.T) {
	be := newFake()
	m := New("m", testSpecs, be, nil)
	ctx := context.Background()

	// Значения вне белого списка игнорируются, ничего не пишется.
	d := desired(t, map[string]*Value{"evil.key": dw("1")})
	if changes, err := m.Plan(ctx, d, ""); err != nil || len(changes) != 0 {
		t.Fatalf("changes=%v err=%v", changes, err)
	}
	snap := newSnap(t)
	if err := m.Apply(ctx, d, "", snap); err != nil || be.sets != 0 {
		t.Fatalf("err=%v sets=%d", err, be.sets)
	}

	// Неверный тип отклоняется до записи.
	bad := desired(t, map[string]*Value{"a.dword": {Type: "string", Data: "oops"}})
	if _, err := m.Plan(ctx, bad, ""); err == nil {
		t.Fatal("несовпадение типа должно быть ошибкой")
	}
}

func newSnap(t *testing.T) *core.Snapshot {
	t.Helper()
	store := &core.SnapshotStore{Dir: filepath.Join(t.TempDir(), "snaps")}
	run, err := store.NewRun("PC", "", timeNow())
	if err != nil {
		t.Fatal(err)
	}
	snap, err := run.Snapshot("m")
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// Цикл «применил → проверил → откатил».
func TestApplyVerifyRollback(t *testing.T) {
	be := newFake()
	nt := &countNotifier{}
	be.vals[k(`K\A`, "Num")] = dw("1")
	be.vals[k(`K\A`, "Bin")] = &Value{Type: "binary", Data: "0a0b"}
	be.vals[k(`K\A`, "Optional")] = dw("7") // есть сейчас, а в желаемом состоянии нет
	m := New("m", testSpecs, be, nt)
	ctx := context.Background()
	snap := newSnap(t)

	want := desired(t, map[string]*Value{
		"a.dword":  dw("2"),                        // изменить
		"a.string": {Type: "string", Data: "new"},  // создать
		"a.bin":    {Type: "binary", Data: "0a0b"}, // без изменений
		"a.opt":    nil,                            // удалить
	})
	if err := m.Apply(ctx, want, "", snap); err != nil {
		t.Fatal(err)
	}

	// Проверил: состояние стало желаемым, повторный план пуст (идемпотентность).
	if be.vals[k(`K\A`, "Num")].Data != "2" || be.vals[k(`K\A`, "Str")].Data != "new" || be.vals[k(`K\A`, "Optional")] != nil {
		t.Fatalf("состояние после применения: %+v", be.vals)
	}
	if changes, _ := m.Plan(ctx, want, ""); len(changes) != 0 {
		t.Fatalf("повторный план не пуст: %+v", changes)
	}
	if nt.n != 1 {
		t.Errorf("уведомлений системы: %d", nt.n)
	}

	// Откатил: всё как было, созданное удалено, удалённое восстановлено.
	if err := m.Rollback(ctx, snap); err != nil {
		t.Fatal(err)
	}
	if be.vals[k(`K\A`, "Num")].Data != "1" {
		t.Errorf("Num не вернулся: %v", be.vals[k(`K\A`, "Num")])
	}
	if be.vals[k(`K\A`, "Str")] != nil {
		t.Errorf("созданное значение Str не удалено: %v", be.vals[k(`K\A`, "Str")])
	}
	if be.vals[k(`K\A`, "Optional")] == nil || be.vals[k(`K\A`, "Optional")].Data != "7" {
		t.Errorf("удалённое значение не восстановлено: %v", be.vals[k(`K\A`, "Optional")])
	}
	if be.vals[k(`K\A`, "Bin")].Data != "0a0b" {
		t.Errorf("Bin изменился: %v", be.vals[k(`K\A`, "Bin")])
	}
}

// Сбой посреди записи: снимок уже есть, откат возвращает всё.
func TestRollbackAfterPartialFailure(t *testing.T) {
	be := newFake()
	be.vals[k(`K\A`, "Num")] = dw("1")
	be.vals[k(`K\A`, "Str")] = &Value{Type: "string", Data: "old"}
	be.failSetOn = "Str" // первая запись пройдёт, вторая упадёт
	m := New("m", testSpecs, be, nil)
	ctx := context.Background()
	snap := newSnap(t)

	err := m.Apply(ctx, desired(t, map[string]*Value{"a.dword": dw("2"), "a.string": {Type: "string", Data: "new"}}), "", snap)
	if err == nil || !strings.Contains(err.Error(), "Str") {
		t.Fatalf("ожидали ошибку записи Str: %v", err)
	}
	if be.vals[k(`K\A`, "Num")].Data != "2" {
		t.Fatal("тест предполагал, что Num уже изменён")
	}
	be.failSetOn = ""
	if err := m.Rollback(ctx, snap); err != nil {
		t.Fatal(err)
	}
	if be.vals[k(`K\A`, "Num")].Data != "1" || be.vals[k(`K\A`, "Str")].Data != "old" {
		t.Fatalf("после отката: %+v", be.vals)
	}
}

func TestSpecsAreSane(t *testing.T) {
	seen := map[string]bool{}
	for _, list := range [][]Spec{AppearanceSpecs, ExplorerSpecs, RegionalSpecs} {
		for _, s := range list {
			if seen[s.ID] {
				t.Errorf("повтор ID %s", s.ID)
			}
			seen[s.ID] = true
			if !strings.HasPrefix(s.Key, "Software\\") && !strings.HasPrefix(s.Key, "Control Panel\\") {
				t.Errorf("%s: ключ вне разрешённых веток HKCU: %s", s.ID, s.Key)
			}
			switch s.Type {
			case "dword", "string", "expand", "binary":
			default:
				t.Errorf("%s: неизвестный тип %q", s.ID, s.Type)
			}
		}
	}
	if len(seen) < 20 {
		t.Errorf("белый список подозрительно короткий: %d", len(seen))
	}
	_ = fmt.Sprint
}
