package wallpaper

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kiyoushii/EnvSync/internal/core"
)

type fakeBackend struct {
	cur  Setting
	sets []Setting
}

func (f *fakeBackend) Get() (Setting, error) { return f.cur, nil }
func (f *fakeBackend) Set(s Setting) error   { f.cur = s; f.sets = append(f.sets, s); return nil }

type env struct {
	winDir, dataDir, srcImgs string
}

func newEnv(t *testing.T) env {
	t.Helper()
	root := t.TempDir()
	e := env{winDir: filepath.Join(root, "Windows"), dataDir: filepath.Join(root, "data"), srcImgs: filepath.Join(root, "imgs")}
	for _, d := range []string{e.winDir, e.srcImgs} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func write(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func snap(t *testing.T) *core.Snapshot {
	t.Helper()
	store := &core.SnapshotStore{Dir: filepath.Join(t.TempDir(), "snaps")}
	run, err := store.NewRun("PC", "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	s, err := run.Snapshot("wallpaper")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Цикл «применил → проверил → откатил» с картинкой, которая едет между машинами.
func TestCustomImageTravelsAndRollsBack(t *testing.T) {
	ctx := context.Background()
	a, b := newEnv(t), newEnv(t)

	// Машина A: своя картинка.
	img := write(t, filepath.Join(a.srcImgs, "красота.JPG"), "pixels-of-the-picture")
	beA := &fakeBackend{cur: Setting{Path: img, Style: "10", Tile: "0"}}
	mA := New(beA, a.dataDir, a.winDir)
	assets := filepath.Join(t.TempDir(), "assets")
	state, err := mA.Export(ctx, assets)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(assets, "wallpaper.jpg")); string(got) != "pixels-of-the-picture" {
		t.Fatalf("картинка не скопирована в assets: %q", got)
	}

	// Машина B: другие обои, другой режим.
	old := write(t, filepath.Join(b.srcImgs, "old.png"), "old-pixels")
	beB := &fakeBackend{cur: Setting{Path: old, Style: "6", Tile: "0"}}
	mB := New(beB, b.dataDir, b.winDir)

	changes, err := mB.Plan(ctx, state, assets)
	if err != nil || len(changes) != 1 || !strings.Contains(changes[0].Before, "old.png") {
		t.Fatalf("changes=%+v err=%v", changes, err)
	}
	sn := snap(t)
	if err := mB.Apply(ctx, state, assets, sn); err != nil {
		t.Fatal(err)
	}
	// Проверил: обои B теперь указывают на копию в её постоянной папке, режим как у A.
	if !strings.HasPrefix(beB.cur.Path, b.dataDir) || beB.cur.Style != "10" {
		t.Fatalf("после применения: %+v", beB.cur)
	}
	if got, _ := os.ReadFile(beB.cur.Path); string(got) != "pixels-of-the-picture" {
		t.Fatalf("картинка на B: %q", got)
	}
	if changes, _ := mB.Plan(ctx, state, assets); len(changes) != 0 {
		t.Fatalf("повторный план не пуст (идемпотентность): %+v", changes)
	}
	// Откатил.
	if err := mB.Rollback(ctx, sn); err != nil {
		t.Fatal(err)
	}
	if beB.cur != (Setting{Path: old, Style: "6", Tile: "0"}) {
		t.Fatalf("после отката: %+v", beB.cur)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatal("прежняя картинка должна остаться на месте")
	}
}

func TestSameImageInDifferentPlaceIsNotAChange(t *testing.T) {
	ctx := context.Background()
	a, b := newEnv(t), newEnv(t)
	imgA := write(t, filepath.Join(a.srcImgs, "x.png"), "same-pixels")
	mA := New(&fakeBackend{cur: Setting{Path: imgA, Style: "10", Tile: "0"}}, a.dataDir, a.winDir)
	assets := filepath.Join(t.TempDir(), "assets")
	state, _ := mA.Export(ctx, assets)

	imgB := write(t, filepath.Join(b.srcImgs, "другое-имя.png"), "same-pixels")
	mB := New(&fakeBackend{cur: Setting{Path: imgB, Style: "10", Tile: "0"}}, b.dataDir, b.winDir)
	if changes, err := mB.Plan(ctx, state, assets); err != nil || len(changes) != 0 {
		t.Fatalf("та же картинка не должна быть изменением: %+v err=%v", changes, err)
	}
}

func TestSystemImageIsNotCopied(t *testing.T) {
	ctx := context.Background()
	a, b := newEnv(t), newEnv(t)
	sysImg := write(t, filepath.Join(a.winDir, "web", "wallpaper", "img19.jpg"), "win")
	mA := New(&fakeBackend{cur: Setting{Path: sysImg, Style: "10", Tile: "0"}}, a.dataDir, a.winDir)
	assets := filepath.Join(t.TempDir(), "assets")
	state, err := mA.Export(ctx, assets)
	if err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(assets); len(entries) != 0 {
		t.Fatalf("стандартная картинка не должна копироваться: %v", entries)
	}

	// На B та же стандартная картинка по её пути (папка Windows своя у каждой машины).
	// Путь в состоянии абсолютный: на B его нет, поэтому план сообщает об этом, а не падает.
	mB := New(&fakeBackend{cur: Setting{Style: "10", Tile: "0"}}, b.dataDir, b.winDir)
	if err := os.Remove(sysImg); err != nil { // «на машине B этого файла нет»
		t.Fatal(err)
	}
	if _, err := mB.Plan(ctx, state, assets); err == nil || !strings.Contains(err.Error(), "нет стандартной картинки") {
		t.Fatalf("ожидали сообщение об отсутствии картинки, err=%v", err)
	}
	write(t, sysImg, "win") // положили такой же файл по тому же пути, как на второй машине с тем же Windows
	beB := &fakeBackend{cur: Setting{Style: "10", Tile: "0"}}
	mB = New(beB, b.dataDir, b.winDir)
	if err := mB.Apply(ctx, state, assets, snap(t)); err != nil || beB.cur.Path != sysImg {
		t.Fatalf("err=%v cur=%+v", err, beB.cur)
	}
}

func TestPlanFailsGracefullyWhenImageHasNotArrived(t *testing.T) {
	ctx := context.Background()
	a, b := newEnv(t), newEnv(t)
	img := write(t, filepath.Join(a.srcImgs, "x.png"), "pixels")
	mA := New(&fakeBackend{cur: Setting{Path: img, Style: "10", Tile: "0"}}, a.dataDir, a.winDir)
	assets := filepath.Join(t.TempDir(), "assets")
	state, _ := mA.Export(ctx, assets)
	mB := New(&fakeBackend{}, b.dataDir, b.winDir)

	_ = os.Remove(filepath.Join(assets, "wallpaper.png"))
	if _, err := mB.Plan(ctx, state, assets); err == nil || !strings.Contains(err.Error(), "не доехала") {
		t.Fatalf("err = %v", err)
	}
	// Повреждённый (докачивающийся) файл тоже не применяется.
	write(t, filepath.Join(assets, "wallpaper.png"), "half")
	if _, err := mB.Plan(ctx, state, assets); err == nil || !strings.Contains(err.Error(), "хеш") {
		t.Fatalf("err = %v", err)
	}
}

func TestStyleOverrideFromMachineProfile(t *testing.T) {
	ctx := context.Background()
	a, b := newEnv(t), newEnv(t)
	img := write(t, filepath.Join(a.srcImgs, "x.png"), "pixels")
	mA := New(&fakeBackend{cur: Setting{Path: img, Style: "10", Tile: "0"}}, a.dataDir, a.winDir)
	assets := filepath.Join(t.TempDir(), "assets")
	state, _ := mA.Export(ctx, assets)

	beB := &fakeBackend{}
	mB := New(beB, b.dataDir, b.winDir)
	mB.StyleOverride = "6" // у ноутбука другое разрешение: подгонка «по размеру»
	if err := mB.Apply(ctx, state, assets, snap(t)); err != nil || beB.cur.Style != "6" {
		t.Fatalf("err=%v cur=%+v", err, beB.cur)
	}
	if changes, _ := mB.Plan(ctx, state, assets); len(changes) != 0 {
		t.Fatalf("с переопределением режима повторный план должен быть пуст: %+v", changes)
	}
}

func TestExportKeepsOnlyCurrentAsset(t *testing.T) {
	ctx := context.Background()
	a := newEnv(t)
	assets := filepath.Join(t.TempDir(), "assets")
	be := &fakeBackend{cur: Setting{Path: write(t, filepath.Join(a.srcImgs, "one.png"), "1"), Style: "10", Tile: "0"}}
	m := New(be, a.dataDir, a.winDir)
	if _, err := m.Export(ctx, assets); err != nil {
		t.Fatal(err)
	}
	be.cur.Path = write(t, filepath.Join(a.srcImgs, "two.jpg"), "2")
	if _, err := m.Export(ctx, assets); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(assets)
	if len(entries) != 1 || entries[0].Name() != "wallpaper.jpg" {
		t.Fatalf("в assets должна остаться одна текущая картинка: %v", entries)
	}
}
