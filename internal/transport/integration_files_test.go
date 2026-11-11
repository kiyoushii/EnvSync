package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kiyoushii/EnvSync/internal/core"
)

// Сценарии этапа 2 (файловый MVP). Идут на двух настоящих Syncthing на loopback
// с теми же настройками папки, что применяет `envsync pair`.

const folderID = "envsync-test"

// snapshot возвращает «относительный путь → SHA-256» для всех файлов папки,
// кроме служебных (.stfolder, .stversions, временные файлы Syncthing).
func snapshot(dir string) map[string]string {
	out := map[string]string{}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if name == ".stfolder" || name == ".stversions" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(name, ".syncthing.") && strings.HasSuffix(name, ".tmp") {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		out[filepath.ToSlash(rel)] = fileHash(p)
		return nil
	})
	return out
}

func (e *pairEnv) waitSameTree(t *testing.T, timeout time.Duration) {
	t.Helper()
	waitFor(t, "папки A и B совпали", timeout, func() bool {
		a, b := snapshot(e.dirA), snapshot(e.dirB)
		return len(a) > 0 && reflect.DeepEqual(a, b)
	})
}

func (e *pairEnv) bothDisconnect(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if err := e.a.client.PauseDevice(ctx, e.b.id); err != nil {
		t.Fatal(err)
	}
	if err := e.b.client.PauseDevice(ctx, e.a.id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "связь A↔B разорвана", 30*time.Second, func() bool {
		c, err := e.a.client.Connections(ctx)
		return err == nil && !c[e.b.id].Connected
	})
}

func (e *pairEnv) bothReconnect(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	_ = e.a.client.ResumeDevice(ctx, e.b.id)
	_ = e.b.client.ResumeDevice(ctx, e.a.id)
	e.waitConnected(t)
}

func inBytes(t *testing.T, in *instance, peer string) int64 {
	t.Helper()
	c, err := in.client.Connections(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return c[peer].InBytesTotal
}

func (e *pairEnv) waitIdle(t *testing.T, in *instance) {
	t.Helper()
	waitFor(t, "папка "+in.name+" в покое", 60*time.Second, func() bool {
		st, err := in.client.FolderStatus(context.Background(), folderID)
		return err == nil && st.State == "idle" && st.NeedBytes == 0
	})
}

// Критерий этапа 2, часть 1: папка, переложенная и переименованная на одной машине,
// оказывается на второй без повторной передачи данных.
func TestIntegrationRenameFolderNoRetransfer(t *testing.T) {
	e := newPairEnv(t)

	const sizeMB = 24
	hash := writeRandomFile(t, filepath.Join(e.dirA, "проект", "big.bin"), sizeMB)
	mustWrite(t, filepath.Join(e.dirA, "проект", "readme.txt"), "описание")
	e.rescan(t)
	waitFor(t, "big.bin доехал до B", 120*time.Second, func() bool {
		return fileHash(filepath.Join(e.dirB, "проект", "big.bin")) == hash
	})
	e.waitIdle(t, e.b)

	before := inBytes(t, e.b, e.a.id)

	// Одновременно и перемещаем в другую папку, и переименовываем.
	if err := os.MkdirAll(filepath.Join(e.dirA, "архив"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(e.dirA, "проект"), filepath.Join(e.dirA, "архив", "проект-2024")); err != nil {
		t.Fatal(err)
	}
	e.rescan(t)

	newBig := filepath.Join(e.dirB, "архив", "проект-2024", "big.bin")
	waitFor(t, "папка переехала на B", 120*time.Second, func() bool {
		return fileHash(newBig) == hash && !exists(filepath.Join(e.dirB, "проект"))
	})
	e.waitIdle(t, e.b)

	moved := inBytes(t, e.b, e.a.id) - before
	t.Logf("файл %d МБ, при переносе B получил от A %d КБ", sizeMB, moved>>10)
	if limit := int64(sizeMB<<20) / 8; moved > limit {
		t.Fatalf("при переносе передано %d байт, больше 1/8 размера файла (%d): файл передан заново", moved, limit)
	}
}

// Конфликт: независимые правки одного файла на обеих сторонах не теряются.
func TestIntegrationConflictKeepsBothVersions(t *testing.T) {
	e := newPairEnv(t)

	mustWrite(t, filepath.Join(e.dirA, "doc.txt"), "исходный текст")
	waitFor(t, "doc.txt доехал до B", 60*time.Second, func() bool {
		return readOrEmpty(filepath.Join(e.dirB, "doc.txt")) == "исходный текст"
	})

	e.bothDisconnect(t)
	mustWrite(t, filepath.Join(e.dirA, "doc.txt"), "правка на A")
	mustWrite(t, filepath.Join(e.dirB, "doc.txt"), "правка на B")
	// Правка на B новее, значит она «победит», а версия A уйдёт в конфликтную копию.
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(filepath.Join(e.dirB, "doc.txt"), later, later); err != nil {
		t.Fatal(err)
	}
	e.rescan(t)
	e.bothReconnect(t)

	e.waitSameTree(t, 120*time.Second)
	for name, dir := range map[string]string{"A": e.dirA, "B": e.dirB} {
		conflicts, _ := filepath.Glob(filepath.Join(dir, "doc.sync-conflict-*.txt"))
		if len(conflicts) != 1 {
			t.Fatalf("на %s конфликтных копий %d, ожидалась 1: %v", name, len(conflicts), conflicts)
		}
		got := []string{readOrEmpty(filepath.Join(dir, "doc.txt")), readOrEmpty(conflicts[0])}
		sort.Strings(got)
		if got[0] != "правка на A" || got[1] != "правка на B" {
			t.Fatalf("на %s версии потерялись или перепутались: %q", name, got)
		}
		if got := readOrEmpty(filepath.Join(dir, "doc.txt")); got != "правка на B" {
			t.Errorf("на %s победила не более новая версия: %q", name, got)
		}
	}
}

// История версий: удалённый файл уходит в .stversions, а не пропадает.
func TestIntegrationDeletedFileGoesToVersions(t *testing.T) {
	e := newPairEnv(t)

	mustWrite(t, filepath.Join(e.dirA, "del.txt"), "важное содержимое")
	waitFor(t, "del.txt доехал до B", 60*time.Second, func() bool {
		return readOrEmpty(filepath.Join(e.dirB, "del.txt")) == "важное содержимое"
	})

	if err := os.Remove(filepath.Join(e.dirA, "del.txt")); err != nil {
		t.Fatal(err)
	}
	e.rescan(t)
	waitFor(t, "del.txt пропал на B", 60*time.Second, func() bool { return !exists(filepath.Join(e.dirB, "del.txt")) })
	waitFor(t, "копия удалённого файла в .stversions на B", 30*time.Second, func() bool {
		return readOrEmpty(filepath.Join(e.dirB, ".stversions", "del.txt")) == "важное содержимое"
	})
}

// Критерий этапа 2, часть 2: обрыв связи в середине передачи не ломает файл.
func TestIntegrationInterruptedTransferResumes(t *testing.T) {
	e := newPairEnv(t)
	ctx := context.Background()

	// Ограничиваем приём у B до 2 МБ/с (в том числе на loopback), чтобы успеть оборвать передачу.
	if err := e.b.client.PatchOptions(ctx, map[string]any{"maxRecvKbps": 2048, "limitBandwidthInLan": true}); err != nil {
		t.Fatal(err)
	}

	hash := writeRandomFile(t, filepath.Join(e.dirA, "big.bin"), 32)
	e.rescan(t)
	waitFor(t, "B получил часть файла", 60*time.Second, func() bool {
		return inBytes(t, e.b, e.a.id) > 6<<20
	})
	if fileHash(filepath.Join(e.dirB, "big.bin")) == hash {
		t.Fatal("файл уже передан целиком: оборвать передачу не удалось")
	}

	// «Выдёргиваем шнур»: убиваем Syncthing на B без корректного выхода.
	e.b.stop(false)
	if got := fileHash(filepath.Join(e.dirB, "big.bin")); got == hash {
		t.Fatal("файл на B оказался целым после обрыва")
	}

	e.b.start(t)
	if err := e.b.client.PatchOptions(ctx, map[string]any{"maxRecvKbps": 0}); err != nil {
		t.Fatal(err)
	}
	e.waitConnected(t)
	waitFor(t, "после обрыва файл доехал и совпал по хешу", 180*time.Second, func() bool {
		return fileHash(filepath.Join(e.dirB, "big.bin")) == hash
	})
	e.waitSameTree(t, 60*time.Second)
}

// Перемещение на одной машине + правка на другой одновременно. Syncthing не умеет сводить
// их в «файл на новом месте с новым содержимым», поэтому проверяем главное: правка не теряется
// ни на одной стороне, а итоговое дерево на обеих машинах одинаково.
func TestIntegrationMoveAndEditOnDifferentMachines(t *testing.T) {
	e := newPairEnv(t)

	mustWrite(t, filepath.Join(e.dirA, "x", "one.txt"), "строка 1\n")
	waitFor(t, "x/one.txt доехал до B", 60*time.Second, func() bool {
		return readOrEmpty(filepath.Join(e.dirB, "x", "one.txt")) == "строка 1\n"
	})

	e.bothDisconnect(t)
	if err := os.MkdirAll(filepath.Join(e.dirA, "y"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(e.dirA, "x", "one.txt"), filepath.Join(e.dirA, "y", "one.txt")); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(e.dirB, "x", "one.txt"), "строка 1\nдобавлено на B\n")
	e.rescan(t)
	e.bothReconnect(t)

	e.waitSameTree(t, 120*time.Second)
	for name, dir := range map[string]string{"A": e.dirA, "B": e.dirB} {
		snap := snapshot(dir)
		var withEdit []string
		for rel := range snap {
			if strings.Contains(readOrEmpty(filepath.Join(dir, filepath.FromSlash(rel))), "добавлено на B") {
				withEdit = append(withEdit, rel)
			}
		}
		if len(withEdit) == 0 {
			t.Fatalf("на %s правка с B потеряна; файлы: %v", name, snap)
		}
		paths := make([]string, 0, len(snap))
		for rel := range snap {
			paths = append(paths, rel)
		}
		sort.Strings(paths)
		t.Logf("итог на %s: файлы %v, правка в %v", name, paths, withEdit)
	}
}

// Исключения: кэши, .git, незавершённые загрузки и т. п. не синкаются, обычные файлы доезжают.
func TestIntegrationIgnoredFilesAreNotSynced(t *testing.T) {
	e := newPairEnv(t)

	ignored := []string{
		"node_modules/lib/a.js",
		".git/config",
		"Cache/x.dat",
		"загрузка.crdownload",
		"фильм.part",
		"Thumbs.db",
		"~$отчёт.docx",
		"проект/node_modules/b.js", // на любой глубине
	}
	for _, rel := range ignored {
		mustWrite(t, filepath.Join(e.dirA, filepath.FromSlash(rel)), "не должно доехать")
	}
	mustWrite(t, filepath.Join(e.dirA, "проект", "main.go"), "package main")
	mustWrite(t, filepath.Join(e.dirA, "ok.txt"), "обычный файл")
	e.rescan(t)

	waitFor(t, "обычные файлы доехали", 90*time.Second, func() bool {
		return readOrEmpty(filepath.Join(e.dirB, "ok.txt")) == "обычный файл" &&
			readOrEmpty(filepath.Join(e.dirB, "проект", "main.go")) == "package main"
	})
	e.waitIdle(t, e.b)
	time.Sleep(5 * time.Second) // запас: если бы исключения не работали, файлы уже были бы здесь

	for _, rel := range ignored {
		if exists(filepath.Join(e.dirB, filepath.FromSlash(rel))) {
			t.Errorf("файл из исключений доехал до B: %s", rel)
		}
		if !exists(filepath.Join(e.dirA, filepath.FromSlash(rel))) {
			t.Errorf("исходный файл на A пропал: %s", rel)
		}
	}
}

// Критерий этапа 3 на настоящем Syncthing: один манифест, у двух «машин» корень синка лежит
// в разных местах (как на разных дисках), папка с одним ID связывает их, файлы ходят в обе стороны.
func TestIntegrationManifestWithDifferentRoots(t *testing.T) {
	bin := skipUnlessIT(t)
	ctx := context.Background()
	a := startInstance(t, bin, "PC-A")
	b := startInstance(t, bin, "PC-B")

	rootA := filepath.Join(t.TempDir(), "диск-C", "Sync")
	rootB := filepath.Join(t.TempDir(), "диск-D", "Данные", "EnvSync")
	manifest := fmt.Sprintf(`schema: 1
folders:
  - id: projects
    path: '{{sync_root}}\Проекты'
  - id: docs
    path: '{{sync_root}}\Docs'
machines:
  PC-A: {vars: {sync_root: '%s'}}
  PC-B:
    vars: {sync_root: '%s'}
    folders:
      docs: {skip: true}
`, rootA, rootB)
	m, err := core.ParseManifest([]byte(manifest))
	if err != nil {
		t.Fatal(err)
	}
	foldersA, err := m.Resolve(core.Env{Machine: "PC-A", User: "u", Home: filepath.Dir(rootA)})
	if err != nil {
		t.Fatal(err)
	}
	foldersB, err := m.Resolve(core.Env{Machine: "pc-b", User: "u", Home: filepath.Dir(rootB)})
	if err != nil {
		t.Fatal(err)
	}
	if len(foldersA) != 2 || len(foldersB) != 1 {
		t.Fatalf("A=%v B=%v: ожидалось 2 и 1 папка (docs на B пропущена)", foldersA, foldersB)
	}

	// Сопряжение устройств обычным путём (заодно создаёт тестовую папку).
	pairWith(t, a, b, filepath.Join(t.TempDir(), "t"))
	pairWith(t, b, a, filepath.Join(t.TempDir(), "t"))
	s := Settings{WatcherDelayS: 1, MaxConflicts: 10}
	for _, x := range []struct {
		in      *instance
		folders []core.ResolvedFolder
	}{{a, foldersA}, {b, foldersB}} {
		peers, err := x.in.client.DeviceIDs(ctx, x.in.id)
		if err != nil || len(peers) != 1 {
			t.Fatalf("устройства %s: %v err=%v", x.in.name, peers, err)
		}
		for _, f := range x.folders {
			if err := ApplyFolder(ctx, x.in.client, x.in.id, peers, FolderSpecFrom(f, s)); err != nil {
				t.Fatal(err)
			}
		}
	}

	pa, pb := foldersA[0].Path, foldersB[0].Path
	if pa == pb {
		t.Fatalf("пути должны различаться: %s", pa)
	}
	t.Logf("projects: A → %s, B → %s", pa, pb)

	mustWrite(t, filepath.Join(pa, "кириллица.txt"), "с A")
	waitFor(t, "файл доехал с A на B по другому пути", 90*time.Second, func() bool {
		return readOrEmpty(filepath.Join(pb, "кириллица.txt")) == "с A"
	})
	mustWrite(t, filepath.Join(pb, "ответ.txt"), "с B")
	waitFor(t, "файл доехал с B на A", 90*time.Second, func() bool {
		return readOrEmpty(filepath.Join(pa, "ответ.txt")) == "с B"
	})

	// Папка docs пропущена на B: файл из неё туда не попадает, B папку не принимает автоматически.
	mustWrite(t, filepath.Join(foldersA[1].Path, "секрет.txt"), "только на A")
	time.Sleep(8 * time.Second)
	if _, err := os.Stat(filepath.Join(rootB, "Docs", "секрет.txt")); err == nil {
		t.Error("папка docs, пропущенная в профиле B, оказалась на B")
	}
}

// themeModule — минимальный модуль для сквозного теста: значение в «настройках» машины
// и файл-приложение (как обои), которые должны доехать вместе с состоянием.
type themeModule struct {
	value, payload string
}

func (m *themeModule) Name() string { return "theme" }

func (m *themeModule) Export(_ context.Context, assets string) (json.RawMessage, error) {
	if err := os.MkdirAll(assets, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(assets, "payload.txt"), []byte(m.payload), 0o600); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]string{"value": m.value, "payload": m.payload})
}

func (m *themeModule) want(desired json.RawMessage, assets string) (value, payload string, err error) {
	var st map[string]string
	if err = json.Unmarshal(desired, &st); err != nil {
		return
	}
	data, err := os.ReadFile(filepath.Join(assets, "payload.txt"))
	if err != nil {
		return "", "", fmt.Errorf("файл-приложение ещё не доехал: %w", err)
	}
	if string(data) != st["payload"] {
		return "", "", fmt.Errorf("файл-приложение докачивается")
	}
	return st["value"], string(data), nil
}

func (m *themeModule) Plan(_ context.Context, desired json.RawMessage, assets string) ([]core.Change, error) {
	v, p, err := m.want(desired, assets)
	if err != nil {
		return nil, err
	}
	if v == m.value && p == m.payload {
		return nil, nil
	}
	return []core.Change{{Target: "theme", Before: m.value, After: v}}, nil
}

func (m *themeModule) Apply(_ context.Context, desired json.RawMessage, assets string, snap *core.Snapshot) error {
	v, p, err := m.want(desired, assets)
	if err != nil {
		return err
	}
	if err := snap.PutJSON("before.json", map[string]string{"value": m.value, "payload": m.payload}); err != nil {
		return err
	}
	m.value, m.payload = v, p
	return nil
}

func (m *themeModule) Rollback(_ context.Context, snap *core.Snapshot) error {
	var before map[string]string
	if err := snap.GetJSON("before.json", &before); err != nil {
		return err
	}
	m.value, m.payload = before["value"], before["payload"]
	return nil
}

// Настройки между машинами на настоящем Syncthing: состояние и файл-приложение едут через
// служебную папку состояний, вторая машина строит план, применяет, после этого обратно ничего не уходит.
func TestIntegrationSettingsTravelViaSyncthing(t *testing.T) {
	bin := skipUnlessIT(t)
	ctx := context.Background()
	a, b := startInstance(t, bin, "PC-A"), startInstance(t, bin, "PC-B")
	pairWith(t, a, b, filepath.Join(t.TempDir(), "t"))
	pairWith(t, b, a, filepath.Join(t.TempDir(), "t"))

	stateA, stateB := filepath.Join(t.TempDir(), "state"), filepath.Join(t.TempDir(), "state")
	for _, x := range []struct {
		in   *instance
		path string
	}{{a, stateA}, {b, stateB}} {
		peers, err := x.in.client.DeviceIDs(ctx, x.in.id)
		if err != nil || len(peers) != 1 {
			t.Fatalf("peers=%v err=%v", peers, err)
		}
		if err := ApplyFolder(ctx, x.in.client, x.in.id, peers, StateFolderSpec(x.path, 1, 10)); err != nil {
			t.Fatal(err)
		}
	}

	modA := &themeModule{value: "light", payload: "wallpaper-bytes-from-A"}
	modB := &themeModule{value: "dark", payload: "old-wallpaper"}
	engine := func(machine, state string, m core.Module) *core.Engine {
		return &core.Engine{
			Machine: machine, State: core.StateDir{Root: state},
			Snaps:   &core.SnapshotStore{Dir: filepath.Join(t.TempDir(), "snaps")},
			Journal: &core.Journal{Path: filepath.Join(t.TempDir(), "journal.jsonl")},
			Modules: []core.Module{m},
		}
	}
	engA, engB := engine("PC-A", stateA, modA), engine("PC-B", stateB, modB)

	// Обе машины снимают базу (первый снимок не считается правкой), затем на A меняют тему: правка A новее.
	if _, err := engB.Export(ctx); err != nil {
		t.Fatal(err)
	}
	wantValue, wantPayload := modA.value, modA.payload
	modA.value, modA.payload = "dark", "old-wallpaper"
	if _, err := engA.Export(ctx); err != nil {
		t.Fatal(err)
	}
	modA.value, modA.payload = wantValue, wantPayload
	if _, err := engA.Export(ctx); err != nil {
		t.Fatal(err)
	}

	// Состояние и файл-приложение A доезжают до B через Syncthing.
	waitFor(t, "состояние и файл A доехали до B", 90*time.Second, func() bool {
		plan, err := engB.Plan(ctx, nil)
		return err == nil && len(plan) == 1 && plan[0].Err == nil
	})
	plan, _ := engB.Plan(ctx, nil)
	if plan[0].Source != "PC-A" || plan[0].Changes[0].After != "light" {
		t.Fatalf("plan = %+v", plan)
	}
	run, _, err := engB.Apply(ctx, nil)
	if err != nil || run == nil || modB.value != "light" || modB.payload != "wallpaper-bytes-from-A" {
		t.Fatalf("run=%v value=%q payload=%q err=%v", run, modB.value, modB.payload, err)
	}

	// Нет пинг-понга: состояние B дошло до A, но A ничего не применяет.
	waitFor(t, "состояние B доехало до A", 90*time.Second, func() bool {
		sf, err := engA.State.Read("PC-B", "theme")
		return err == nil && sf != nil
	})
	if plan, err := engA.Plan(ctx, nil); err != nil || len(plan) != 0 {
		t.Fatalf("A получил своё же состояние обратно: %+v err=%v", plan, err)
	}

	// Откат на B: значения вернулись, и B больше не получает то же самое от A.
	if _, err := engB.Rollback(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if modB.value != "dark" || modB.payload != "old-wallpaper" {
		t.Fatalf("после отката: %q / %q", modB.value, modB.payload)
	}
	if plan, _ := engB.Plan(ctx, nil); len(plan) != 0 {
		t.Fatalf("после отката план не пуст: %+v", plan)
	}
}

// Сложные имена: кириллица, пробелы, юникод, очень длинное имя и путь длиннее 260 символов.
func TestIntegrationHardFileNames(t *testing.T) {
	e := newPairEnv(t)

	deep := ""
	for i := 0; i < 12; i++ {
		deep = filepath.Join(deep, strings.Repeat("д", 25)) // 12 × 26 = ~312 символов пути
	}
	names := []string{
		filepath.Join("Привет мир", "файл с пробелами.txt"),
		"ёжик №1 (копия) [черновик].txt",
		"Ünïcödé – тест — «кавычки».txt",
		"очень длинное имя " + strings.Repeat("я", 120) + ".txt",
		filepath.Join(deep, "глубокий файл.txt"),
	}
	for _, rel := range names {
		mustWrite(t, filepath.Join(e.dirA, rel), "содержимое: "+rel)
	}
	e.rescan(t)
	e.waitSameTree(t, 120*time.Second)

	for _, rel := range names {
		if got := readOrEmpty(filepath.Join(e.dirB, rel)); got != "содержимое: "+rel {
			t.Errorf("файл %q на B: %q", rel, got)
		}
	}
	if full := filepath.Join(e.dirB, names[len(names)-1]); len(full) < 300 {
		t.Errorf("тест не проверяет длинный путь: длина всего %d", len(full))
	}
}
