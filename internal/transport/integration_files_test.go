package transport

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
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
