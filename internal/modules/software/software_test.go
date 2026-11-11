package software

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kiyoushii/EnvSync/internal/core"
)

// fakeWinget — «установленные программы» в памяти.
type fakeWinget struct {
	pkgs      map[string]Package
	failOn    string // id, на котором Install падает
	installed []string
	removed   []string
}

func newFake(ids ...string) *fakeWinget {
	f := &fakeWinget{pkgs: map[string]Package{}}
	for _, id := range ids {
		f.pkgs[strings.ToLower(id)] = Package{ID: id, Version: "1.0"}
	}
	return f
}

func (f *fakeWinget) Installed(context.Context) (map[string]Package, error) {
	out := map[string]Package{}
	for k, v := range f.pkgs {
		out[k] = v
	}
	return out, nil
}

func (f *fakeWinget) Install(_ context.Context, p core.WingetPkg) error {
	if strings.EqualFold(p.ID, f.failOn) {
		return errors.New("установщик вернул код 1603")
	}
	v := p.Version
	if v == "" {
		v = "2.0"
	}
	f.pkgs[strings.ToLower(p.ID)] = Package{ID: p.ID, Version: v}
	f.installed = append(f.installed, p.ID)
	return nil
}

func (f *fakeWinget) Uninstall(_ context.Context, id string) error {
	delete(f.pkgs, strings.ToLower(id))
	f.removed = append(f.removed, id)
	return nil
}

func manager(t *testing.T, machine string, w Winget, state string) *Manager {
	t.Helper()
	base := t.TempDir()
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return &Manager{
		W: w, Machine: machine, State: core.StateDir{Root: state},
		Snaps:   &core.SnapshotStore{Dir: filepath.Join(base, "snaps")},
		Journal: &core.Journal{Path: filepath.Join(base, "journal.jsonl")},
		Now:     func() time.Time { clock = clock.Add(time.Minute); return clock },
	}
}

func pkgs(ids ...string) []core.WingetPkg {
	var out []core.WingetPkg
	for _, id := range ids {
		out = append(out, core.WingetPkg{ID: id})
	}
	return out
}

func yes(string) bool { return true }
func no(string) bool  { return false }

// Критерий этапа 5: программа, добавленная в манифест, ставится на второй машине после подтверждения.
func TestProgramAddedToManifestIsInstalledOnSecondMachine(t *testing.T) {
	ctx := context.Background()
	state := t.TempDir()
	pc := manager(t, "PC", newFake("Git.Git", "Microsoft.VisualStudioCode"), state)
	laptopW := newFake("Git.Git")
	laptop := manager(t, "LAPTOP", laptopW, state)

	// Манифест (один на обе машины) содержит три программы; на ноутбуке не хватает двух.
	set := core.SoftwareSet{Install: pkgs("Git.Git", "Microsoft.VisualStudioCode", "GoLang.Go")}
	plan, err := laptop.Plan(ctx, set)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Install) != 2 || plan.AlreadyOK != 1 || !plan.HasWork() {
		t.Fatalf("plan = %+v", plan)
	}
	// dry-run ничего не ставит.
	if len(laptopW.installed) != 0 {
		t.Fatal("план не должен ничего устанавливать")
	}

	res, err := laptop.Apply(ctx, plan, ApplyOptions{})
	if err != nil || len(res.Installed) != 2 || len(res.Failed) != 0 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if _, ok := laptopW.pkgs["golang.go"]; !ok {
		t.Fatal("GoLang.Go не установлен")
	}
	// Идемпотентность: повторный план пуст.
	plan2, _ := laptop.Plan(ctx, set)
	if plan2.HasWork() {
		t.Fatalf("повторный план не пуст: %+v", plan2)
	}
	_ = pc
}

func TestNewProgramOnOneMachineIsSuggestedForManifest(t *testing.T) {
	ctx := context.Background()
	state := t.TempDir()
	pc := manager(t, "PC", newFake("Git.Git", "Valve.Steam", "Microsoft.VCRedist.2015+.x64"), state)
	laptop := manager(t, "LAPTOP", newFake("Git.Git"), state)
	set := core.SoftwareSet{Install: pkgs("Git.Git")}

	if _, err := pc.Plan(ctx, set); err != nil { // ПК записывает своё состояние
		t.Fatal(err)
	}
	plan, err := laptop.Plan(ctx, set)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Install) != 0 {
		t.Fatalf("в манифесте ничего не добавилось, ставить нечего: %+v", plan.Install)
	}
	if len(plan.Suggest) != 1 || plan.Suggest[0].ID != "Valve.Steam" || plan.Suggest[0].Where[0] != "PC" {
		t.Fatalf("suggest = %+v (ожидался Valve.Steam с ПК; системный VCRedist не предлагается)", plan.Suggest)
	}
}

// Удаление: только из remove, только с флагом, только после подтверждения каждого пакета.
func TestUninstallNeedsExplicitListFlagAndConfirmation(t *testing.T) {
	ctx := context.Background()
	state := t.TempDir()
	w := newFake("Git.Git", "Old.App", "Unlisted.App")
	m := manager(t, "PC", w, state)
	set := core.SoftwareSet{Install: pkgs("Git.Git"), Remove: []string{"Old.App", "Not.Installed"}}

	plan, _ := m.Plan(ctx, set)
	if len(plan.Remove) != 1 || plan.Remove[0] != "Old.App" || len(plan.Install) != 0 {
		t.Fatalf("plan = %+v", plan)
	}

	// Без флага: ничего не удаляется.
	if res, err := m.Apply(ctx, plan, ApplyOptions{Confirm: yes}); err != nil || len(res.Removed) != 0 || len(w.removed) != 0 {
		t.Fatalf("без --uninstall удалено: %+v err=%v", w.removed, err)
	}
	// С флагом, но без подтверждения: тоже нет.
	if _, err := m.Apply(ctx, plan, ApplyOptions{Uninstall: true, Confirm: no}); err != nil || len(w.removed) != 0 {
		t.Fatalf("без подтверждения удалено: %+v err=%v", w.removed, err)
	}
	// С флагом и подтверждением: удаляется только Old.App, но не Unlisted.App.
	res, err := m.Apply(ctx, plan, ApplyOptions{Uninstall: true, Confirm: yes})
	if err != nil || len(res.Removed) != 1 || res.Removed[0] != "Old.App" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if _, ok := w.pkgs["unlisted.app"]; !ok {
		t.Fatal("пакет вне списка remove удалён")
	}
}

func TestOneFailedInstallDoesNotStopOthers(t *testing.T) {
	ctx := context.Background()
	w := newFake()
	w.failOn = "Broken.App"
	m := manager(t, "PC", w, t.TempDir())
	plan, _ := m.Plan(ctx, core.SoftwareSet{Install: pkgs("A.One", "Broken.App", "C.Three")})
	res, err := m.Apply(ctx, plan, ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Installed) != 2 || res.Failed["Broken.App"] == nil {
		t.Fatalf("res = %+v", res)
	}
	entries, _ := m.Journal.Recent(0)
	var sawError bool
	for _, e := range entries {
		sawError = sawError || (e.Action == "error" && strings.Contains(e.Detail, "Broken.App"))
	}
	if !sawError {
		t.Fatal("сбой установки должен быть в журнале")
	}
}

// Цикл «применил → проверил → откатил».
func TestInstallVerifyRollback(t *testing.T) {
	ctx := context.Background()
	w := newFake("Keep.Me", "Old.App")
	m := manager(t, "PC", w, t.TempDir())
	plan, _ := m.Plan(ctx, core.SoftwareSet{Install: pkgs("Keep.Me", "New.App"), Remove: []string{"Old.App"}})
	res, err := m.Apply(ctx, plan, ApplyOptions{Uninstall: true, Confirm: yes})
	if err != nil || len(res.Installed) != 1 || len(res.Removed) != 1 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if _, ok := w.pkgs["new.app"]; !ok {
		t.Fatal("New.App не установлен")
	}

	// Откат без подтверждения: поставленное не удаляется, запуск не считается откатанным.
	if _, err := m.Rollback(ctx, "", no); err != nil {
		t.Fatal(err)
	}
	if _, ok := w.pkgs["new.app"]; !ok {
		t.Fatal("без подтверждения откат не должен удалять программу")
	}
	// Откат с подтверждением: поставленное удалено, удалённое возвращено, прочее не тронуто.
	res2, err := m.Rollback(ctx, "", yes)
	if err != nil {
		t.Fatalf("res=%+v err=%v", res2, err)
	}
	if _, ok := w.pkgs["new.app"]; ok {
		t.Error("New.App должен быть удалён откатом")
	}
	if _, ok := w.pkgs["old.app"]; !ok {
		t.Error("Old.App должен быть возвращён откатом")
	}
	if _, ok := w.pkgs["keep.me"]; !ok {
		t.Error("Keep.Me стоял и до запуска, откат его трогать не должен")
	}
	if _, err := m.Rollback(ctx, "", yes); err == nil {
		t.Fatal("повторный откат того же запуска должен быть отклонён")
	}
}

func TestPinnedVersionMismatchIsReportedNotChanged(t *testing.T) {
	ctx := context.Background()
	w := newFake("Pinned.App") // версия 1.0
	m := manager(t, "PC", w, t.TempDir())
	plan, _ := m.Plan(ctx, core.SoftwareSet{Install: []core.WingetPkg{{ID: "Pinned.App", Version: "3.1"}}})
	if len(plan.Install) != 0 || len(plan.Mismatch) != 1 || !strings.Contains(plan.Mismatch[0], "установлена 1.0, в манифесте 3.1") {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestScanWritesMachineStateOnlyOnChange(t *testing.T) {
	ctx := context.Background()
	state := t.TempDir()
	w := newFake("A.One")
	m := manager(t, "PC", w, state)
	_, _ = m.Scan(ctx)
	first, _ := m.State.Read("PC", "software")
	_, _ = m.Scan(ctx)
	second, _ := m.State.Read("PC", "software")
	if !first.ChangedAt.Equal(second.ChangedAt) {
		t.Fatal("повторный Scan без изменений двигает ChangedAt")
	}
	w.pkgs["b.two"] = Package{ID: "B.Two", Version: "1"}
	_, _ = m.Scan(ctx)
	third, _ := m.State.Read("PC", "software")
	if !third.ChangedAt.After(first.ChangedAt) {
		t.Fatal("после установки программы состояние должно обновиться")
	}
}

func TestParseExport(t *testing.T) {
	data := []byte(`{"$schema": "x", "Sources": [{"Packages": [
		{"PackageIdentifier": "Git.Git", "Version": "2.50.0"},
		{"PackageIdentifier": "Docker.DockerDesktop", "Version": "4.76.0"},
		{"PackageIdentifier": ""}]}]}`)
	got, err := ParseExport(data)
	if err != nil || len(got) != 2 || got["git.git"].Version != "2.50.0" || got["docker.dockerdesktop"].ID != "Docker.DockerDesktop" {
		t.Fatalf("got=%v err=%v", got, err)
	}
	if _, err := ParseExport([]byte("not json")); err == nil {
		t.Fatal("ожидали ошибку")
	}
}

// Только чтение: настоящий winget export на этой машине. Запускается явно (ENVSYNC_IT=1).
func TestRealWingetExportIsReadable(t *testing.T) {
	if os.Getenv("ENVSYNC_IT") != "1" {
		t.Skip("интеграционный тест: задайте ENVSYNC_IT=1")
	}
	got, err := RealWinget{}.Installed(context.Background())
	if err != nil {
		t.Skip("winget недоступен:", err)
	}
	if len(got) == 0 {
		t.Fatal("winget export вернул пустой список")
	}
	t.Logf("на этой машине пакетов winget: %d", len(got))
}
