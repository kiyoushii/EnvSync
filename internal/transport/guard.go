package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kiyoushii/EnvSync/internal/core"
)

// Предохранитель массового удаления. Если от другой машины приходит удаление многих файлов
// (не смонтировался диск, случайно удалён корень), агент ставит папку на паузу и ждёт решения человека.
//
// Почему это работает: Syncthing сначала сообщает о ждущих удалениях (needDeletes), а применяет их с
// заметной задержкой (в эксперименте: 250 удалений заметны через 0,8 с, применены к 2,9 с). Агент проверяет статус
// несколько раз в секунду и успевает поставить папку на паузу до первого удаления. Даже если часть удалений
// успела примениться, файлы лежат в «корзине» (.stversions) и возвращаются командой reject.

// maxPendingNames — сколько имён ожидающих удалений запоминается для «отклонить». Дальше список обрезается.
const maxPendingNames = 50000

// Tripped — запись о сработавшем предохранителе.
type Tripped struct {
	FolderID    string    `json:"folderId"`
	At          time.Time `json:"at"`
	NeedDeletes int64     `json:"needDeletes"`
	GlobalFiles int64     `json:"globalFiles"`
	Pending     []string  `json:"pending"`   // имена файлов, которые собирались удалить
	Truncated   bool      `json:"truncated"` // список имён обрезан
}

type guardState struct {
	Folders map[string]Tripped `json:"folders"`
	// Settling — папки, по которым человек уже принял решение: удаления ещё ждут применения, и повторно
	// срабатывать на них нельзя. Запись снимается, когда ждущих удалений стало меньше минимума или вышло время.
	Settling map[string]time.Time `json:"settling,omitempty"`
}

// settleFor — сколько после решения человека папку не проверяем, пока ждущие удаления не рассосутся.
const settleFor = 3 * time.Minute

// Guard следит за папками и срабатывает при массовом удалении.
type Guard struct {
	C         *Client
	Cfg       core.MassDeleteDef
	Interval  time.Duration // как часто проверять; 0 = 250 мс
	StatePath string        // где хранить сработавшие предохранители (переживает перезапуск агента)
	Journal   *core.Journal
	Machine   string
	Log       *log.Logger
	Now       func() time.Time

	mu sync.Mutex
}

func (g *Guard) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

func (g *Guard) logf(format string, a ...any) {
	if g.Log != nil {
		g.Log.Printf(format, a...)
	}
}

func (g *Guard) entry(e core.Entry) {
	if g.Journal != nil {
		e.Time, e.Machine, e.Module = g.now().UTC(), g.Machine, "guard"
		_ = g.Journal.Append(e)
	}
}

func (g *Guard) load() (guardState, error) {
	st := guardState{Folders: map[string]Tripped{}, Settling: map[string]time.Time{}}
	data, err := os.ReadFile(g.StatePath)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return st, fmt.Errorf("%s: %w", g.StatePath, err)
	}
	if st.Folders == nil {
		st.Folders = map[string]Tripped{}
	}
	if st.Settling == nil {
		st.Settling = map[string]time.Time{}
	}
	return st, nil
}

func (g *Guard) save(st guardState) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(g.StatePath), 0o700); err != nil {
		return err
	}
	tmp := g.StatePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, g.StatePath)
}

// Tripped возвращает сработавшие предохранители (папки на паузе, ждущие решения).
func (g *Guard) Tripped() ([]Tripped, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	st, err := g.load()
	if err != nil {
		return nil, err
	}
	out := make([]Tripped, 0, len(st.Folders))
	for _, t := range st.Folders {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FolderID < out[j].FolderID })
	return out, nil
}

type needItem struct {
	Name    string `json:"name"`
	Deleted bool   `json:"deleted"`
}

// pendingDeletes снимает список ожидающих удалений (до паузы: у папки на паузе этот API недоступен).
func (g *Guard) pendingDeletes(ctx context.Context, folderID string) (names []string, truncated bool, err error) {
	const perPage = 1000
	for page := 1; ; page++ {
		var resp struct {
			Progress []needItem `json:"progress"`
			Queued   []needItem `json:"queued"`
			Rest     []needItem `json:"rest"`
		}
		path := fmt.Sprintf("/rest/db/need?folder=%s&page=%d&perpage=%d", url.QueryEscape(folderID), page, perPage)
		if err := g.C.do(ctx, "GET", path, nil, &resp); err != nil {
			return names, truncated, err
		}
		n := 0
		for _, list := range [][]needItem{resp.Progress, resp.Queued, resp.Rest} {
			for _, it := range list {
				n++
				if it.Deleted {
					if len(names) >= maxPendingNames {
						return names, true, nil
					}
					names = append(names, it.Name)
				}
			}
		}
		if n < perPage {
			return names, false, nil
		}
	}
}

// Check — один проход: для каждой папки сравнивает ждущие удаления с порогом. Возвращает сработавшие сейчас.
func (g *Guard) Check(ctx context.Context) ([]Tripped, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	ids, err := g.C.FolderIDs(ctx)
	if err != nil {
		return nil, err
	}
	st, err := g.load()
	if err != nil {
		return nil, err
	}
	var fresh []Tripped
	for _, id := range ids {
		if _, already := st.Folders[id]; already {
			continue // ждёт решения, повторно не срабатываем
		}
		fs, err := g.C.FolderStatus(ctx, id)
		if err != nil {
			continue // папка на паузе, не запущена и т. п.
		}
		if until, settling := st.Settling[id]; settling {
			if fs.NeedDeletes >= int64(g.Cfg.MinFiles) && g.now().Before(until) {
				continue // решение человека применяется, ждём, пока удаления рассосутся
			}
			delete(st.Settling, id)
			if err := g.save(st); err != nil {
				return fresh, err
			}
		}
		if !g.Cfg.Exceeded(fs.NeedDeletes, fs.GlobalFiles) {
			continue
		}
		// Список снимаем ДО паузы; это быстро (десятки миллисекунд), а удаления применяются заметно медленнее.
		names, truncated, listErr := g.pendingDeletes(ctx, id)
		if err := g.C.PauseFolder(ctx, id); err != nil {
			return fresh, fmt.Errorf("пауза папки %s: %w", id, err)
		}
		t := Tripped{FolderID: id, At: g.now().UTC(), NeedDeletes: fs.NeedDeletes, GlobalFiles: fs.GlobalFiles, Pending: names, Truncated: truncated || listErr != nil}
		st.Folders[id] = t
		if err := g.save(st); err != nil {
			return fresh, err
		}
		fresh = append(fresh, t)
		g.entry(core.Entry{Action: "trip", Detail: fmt.Sprintf("папка %s на паузе: пришло удаление %d файлов из %d, нужно решение (envsync guard accept|reject %s)", id, fs.NeedDeletes, fs.GlobalFiles, id)})
		g.logf("ПРЕДОХРАНИТЕЛЬ: папка %q поставлена на паузу: другая машина удаляет %d из %d файлов. Решение: envsync guard accept %s (принять) или envsync guard reject %s (отклонить)", id, fs.NeedDeletes, fs.GlobalFiles, id, id)
	}
	return fresh, nil
}

// Run проверяет папки, пока не отменён ctx.
func (g *Guard) Run(ctx context.Context) {
	every := g.Interval
	if every <= 0 {
		every = 250 * time.Millisecond
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if _, err := g.Check(ctx); err != nil && ctx.Err() == nil {
				g.logf("предохранитель: %v", err)
			}
		}
	}
}

func (g *Guard) tripFor(folderID string) (guardState, Tripped, error) {
	st, err := g.load()
	if err != nil {
		return st, Tripped{}, err
	}
	t, ok := st.Folders[folderID]
	if !ok {
		return st, t, fmt.Errorf("для папки %q предохранитель не срабатывал (ждущих решений нет)", folderID)
	}
	return st, t, nil
}

// Accept принимает удаление: папка возобновляется, ожидающие удаления применяются.
// Удалённое лежит в «корзине» (.stversions) и хранится по сроку, заданному для папки.
func (g *Guard) Accept(ctx context.Context, folderID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	st, t, err := g.tripFor(folderID)
	if err != nil {
		return err
	}
	// Сначала фиксируем решение (с фазой успокоения), потом возобновляем: иначе проверка успеет сработать снова.
	delete(st.Folders, folderID)
	st.Settling[folderID] = g.now().Add(settleFor)
	if err := g.save(st); err != nil {
		return err
	}
	if err := g.C.ResumeFolder(ctx, folderID); err != nil {
		return err
	}
	g.entry(core.Entry{Action: "accept", Detail: fmt.Sprintf("папка %s: удаление %d файлов принято человеком", folderID, t.NeedDeletes)})
	return nil
}

// RejectResult — итог отклонения.
type RejectResult struct {
	Restored int // возвращено из «корзины» (уже были удалены)
	Kept     int // файлы, которые не успели удалить: помечены как изменённые
	Missing  int // не найдены ни на диске, ни в «корзине»
}

// Reject отклоняет удаление: файлы, которые собирались удалить, остаются (уже удалённые возвращаются
// из «корзины»), им обновляется время изменения, чтобы они победили «удаление» другой машины, после чего
// папка возобновляется. Другая машина получит файлы обратно: изменение побеждает удаление.
func (g *Guard) Reject(ctx context.Context, folderID string) (RejectResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	var res RejectResult
	st, t, err := g.tripFor(folderID)
	if err != nil {
		return res, err
	}
	if t.Truncated {
		return res, fmt.Errorf("список ждущих удалений неполон (%d файлов, запомнено %d): отклонить автоматически нельзя; "+
			"верните файлы вручную из .stversions и примите решение accept", t.NeedDeletes, len(t.Pending))
	}
	folder, err := g.C.Folder(ctx, folderID)
	if err != nil || folder == nil {
		return res, fmt.Errorf("папка %q не найдена в Syncthing: %v", folderID, err)
	}
	root, _ := folder["path"].(string)
	if root == "" {
		return res, errors.New("у папки не указан путь")
	}
	now := g.now()
	for _, name := range t.Pending {
		rel := filepath.FromSlash(name)
		if filepath.IsAbs(rel) || strings.HasPrefix(filepath.Clean(rel), "..") {
			return res, fmt.Errorf("недопустимое имя в списке удалений: %q", name)
		}
		local := filepath.Join(root, rel)
		if _, err := os.Stat(local); err == nil {
			res.Kept++
		} else if v := filepath.Join(root, ".stversions", rel); fileExists(v) {
			if err := core.CopyFileAtomic(v, local); err != nil {
				return res, fmt.Errorf("возврат %s из корзины: %w", name, err)
			}
			res.Restored++
		} else {
			res.Missing++
			continue
		}
		if err := os.Chtimes(local, now, now); err != nil {
			return res, fmt.Errorf("обновление времени %s: %w", name, err)
		}
	}
	delete(st.Folders, folderID)
	st.Settling[folderID] = g.now().Add(settleFor)
	if err := g.save(st); err != nil {
		return res, err
	}
	if err := g.C.ResumeFolder(ctx, folderID); err != nil {
		return res, err
	}
	g.entry(core.Entry{Action: "reject", Detail: fmt.Sprintf("папка %s: удаление отклонено, оставлено %d, возвращено из корзины %d, не найдено %d", folderID, res.Kept, res.Restored, res.Missing)})
	return res, nil
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}
