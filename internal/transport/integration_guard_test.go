package transport

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kiyoushii/EnvSync/internal/core"
)

// Сценарии предохранителя массового удаления на двух настоящих Syncthing.

func countFiles(dir string) int {
	n := 0
	for rel := range snapshot(dir) {
		if rel != ".stignore" {
			n++
		}
	}
	return n
}

func fileName(i int) string { return filepath.Join("много", fmt.Sprintf("файл-%03d.txt", i)) }
func fileBody(i int) string { return fmt.Sprintf("содержимое %d", i) }

// guardEnv — две машины с 300 синхронизированными файлами и предохранителем на машине B.
type guardEnv struct {
	*pairEnv
	g       *Guard
	journal *core.Journal
}

func newGuardEnv(t *testing.T, n int) *guardEnv {
	t.Helper()
	e := newPairEnv(t)
	for i := 0; i < n; i++ {
		mustWrite(t, filepath.Join(e.dirA, fileName(i)), fileBody(i))
	}
	e.rescan(t)
	waitFor(t, "все файлы доехали до B", 120*time.Second, func() bool { return countFiles(e.dirB) == n })
	e.waitIdle(t, e.b)

	dir := t.TempDir()
	j := &core.Journal{Path: filepath.Join(dir, "journal.jsonl")}
	g := &Guard{
		C:         e.b.client,
		Cfg:       core.MassDeleteDef{MaxFiles: 1000, MaxPercent: 5, MinFiles: 10},
		Interval:  100 * time.Millisecond,
		StatePath: filepath.Join(dir, "guard.json"),
		Journal:   j,
		Machine:   "B",
	}
	ctx, cancel := context.WithCancel(context.Background())
	go g.Run(ctx)
	t.Cleanup(cancel)
	return &guardEnv{pairEnv: e, g: g, journal: j}
}

func (e *guardEnv) deleteOnA(t *testing.T, from, to int) {
	t.Helper()
	for i := from; i < to; i++ {
		if err := os.Remove(filepath.Join(e.dirA, fileName(i))); err != nil {
			t.Fatal(err)
		}
	}
	e.rescan(t)
}

func (e *guardEnv) waitTripped(t *testing.T) Tripped {
	t.Helper()
	var got Tripped
	waitFor(t, "предохранитель сработал", 30*time.Second, func() bool {
		tr, err := e.g.Tripped()
		if err == nil && len(tr) == 1 {
			got = tr[0]
			return true
		}
		return false
	})
	return got
}

func journalActions(t *testing.T, j *core.Journal) []string {
	t.Helper()
	entries, err := j.Recent(0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Action)
	}
	return out
}

// Критерий этапа 6: случайное массовое удаление останавливается.
func TestIntegrationMassDeleteIsStoppedAndAcceptKeepsVersions(t *testing.T) {
	const n, deleted = 300, 250
	e := newGuardEnv(t, n)

	e.deleteOnA(t, 0, deleted)
	tr := e.waitTripped(t)
	if tr.NeedDeletes < 200 || tr.GlobalFiles == 0 || len(tr.Pending) < 200 || tr.Truncated {
		t.Fatalf("trip = %+v (pending %d)", tr, len(tr.Pending))
	}
	folder, err := e.b.client.Folder(context.Background(), folderID)
	if err != nil || folder["paused"] != true {
		t.Fatalf("папка должна быть на паузе: %v err=%v", folder["paused"], err)
	}
	time.Sleep(2 * time.Second) // если бы пауза не работала, удаления уже применились бы
	survived := countFiles(e.dirB)
	t.Logf("после срабатывания на B осталось %d файлов из %d (A удалил %d)", survived, n, deleted)
	if survived < n-50 {
		t.Fatalf("предохранитель не остановил удаление: осталось %d из %d", survived, n)
	}

	// Принимаем удаление: оно применяется, а файлы остаются в «корзине».
	if err := e.g.Accept(context.Background(), folderID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "удаление применилось после accept", 60*time.Second, func() bool { return countFiles(e.dirB) == n-deleted })
	versioned := 0
	for rel := range snapshotAll(filepath.Join(e.dirB, ".stversions")) {
		_ = rel
		versioned++
	}
	if versioned < deleted-50 {
		t.Errorf("в .stversions %d файлов, ожидалось около %d", versioned, deleted)
	}
	if left, _ := e.g.Tripped(); len(left) != 0 {
		t.Errorf("после accept ждущих решений быть не должно: %v", left)
	}
	acts := journalActions(t, e.journal)
	if len(acts) != 2 || acts[0] != "trip" || acts[1] != "accept" {
		t.Errorf("журнал: %v", acts)
	}
}

func snapshotAll(dir string) map[string]struct{} {
	out := map[string]struct{}{}
	_ = filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			out[rel] = struct{}{}
		}
		return nil
	})
	return out
}

// Отклонение: все файлы возвращаются и на B, и на A (изменение побеждает удаление).
func TestIntegrationMassDeleteRejectBringsFilesBackEverywhere(t *testing.T) {
	const n, deleted = 300, 250
	e := newGuardEnv(t, n)

	e.deleteOnA(t, 0, deleted)
	e.waitTripped(t)
	res, err := e.g.Reject(context.Background(), folderID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("reject: оставлено %d, возвращено из корзины %d, не найдено %d", res.Kept, res.Restored, res.Missing)
	if res.Missing != 0 || res.Kept+res.Restored < deleted {
		t.Fatalf("reject = %+v", res)
	}

	waitFor(t, "все файлы вернулись на A и остались на B", 120*time.Second, func() bool {
		return countFiles(e.dirA) >= n && countFiles(e.dirB) >= n
	})
	for i := 0; i < n; i++ {
		for name, dir := range map[string]string{"A": e.dirA, "B": e.dirB} {
			if got := readOrEmpty(filepath.Join(dir, fileName(i))); got != fileBody(i) {
				t.Fatalf("файл %d на %s: %q", i, name, got)
			}
		}
	}
	acts := journalActions(t, e.journal)
	if len(acts) != 2 || acts[1] != "reject" {
		t.Errorf("журнал: %v", acts)
	}
}

// Обычные удаления не должны срабатывать: меньше минимума, ниже процента, выше процента.
func TestIntegrationOrdinaryDeletionsDoNotTripButLargeOnesDo(t *testing.T) {
	const n = 300
	e := newGuardEnv(t, n)

	e.deleteOnA(t, 0, 5) // 5 файлов: меньше MinFiles
	waitFor(t, "5 удалений применились", 60*time.Second, func() bool { return countFiles(e.dirB) == n-5 })
	e.deleteOnA(t, 5, 17) // 12 файлов: 4% < 5%
	waitFor(t, "12 удалений применились", 60*time.Second, func() bool { return countFiles(e.dirB) == n-17 })
	if tr, _ := e.g.Tripped(); len(tr) != 0 {
		t.Fatalf("обычные удаления не должны срабатывать: %+v", tr)
	}

	e.deleteOnA(t, 17, 57) // 40 файлов из ~283: больше 5%
	tr := e.waitTripped(t)
	if tr.NeedDeletes < 30 {
		t.Fatalf("trip = %+v", tr)
	}
}
