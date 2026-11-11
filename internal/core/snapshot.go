package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// SnapshotStore хранит «снимки до»: перед каждым применением настроек модуль складывает сюда
// всё, что нужно для отката. Структура на диске: <Dir>/<id запуска>/<модуль>/..., рядом run.json.
type SnapshotStore struct {
	Dir string
}

// Run — один запуск применения: объединяет снимки всех модулей, которые изменились за раз.
// Откат запуска возвращает все его модули в обратном порядке.
type Run struct {
	ID      string    `json:"id"`
	Created time.Time `json:"created"`
	Machine string    `json:"machine"`
	Source  string    `json:"source,omitempty"` // с какой машины пришло состояние
	Modules []string  `json:"modules"`          // в порядке применения
	Done    bool      `json:"done"`             // применение завершено без ошибки
	Rolled  bool      `json:"rolled"`           // запуск уже откатан

	dir string
}

// NewRun создаёт новый запуск.
func (s *SnapshotStore) NewRun(machine, source string, now time.Time) (*Run, error) {
	id := now.UTC().Format("20060102T150405.000")
	r := &Run{ID: id, Created: now.UTC(), Machine: machine, Source: source, dir: filepath.Join(s.Dir, id)}
	if err := os.MkdirAll(r.dir, 0o700); err != nil {
		return nil, err
	}
	return r, r.save()
}

// OpenRun открывает существующий запуск; id "" означает последний.
func (s *SnapshotStore) OpenRun(id string) (*Run, error) {
	if id == "" {
		runs, err := s.ListRuns()
		if err != nil {
			return nil, err
		}
		if len(runs) == 0 {
			return nil, errors.New("снимков нет: откатывать нечего")
		}
		return runs[len(runs)-1], nil
	}
	data, err := os.ReadFile(filepath.Join(s.Dir, id, "run.json"))
	if err != nil {
		return nil, fmt.Errorf("запуск %q: %w", id, err)
	}
	var r Run
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	r.dir = filepath.Join(s.Dir, id)
	return &r, nil
}

// ListRuns возвращает все запуски от старых к новым.
func (s *SnapshotStore) ListRuns() ([]*Run, error) {
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var runs []*Run
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if r, err := s.OpenRun(e.Name()); err == nil {
			runs = append(runs, r)
		}
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].ID < runs[j].ID })
	return runs, nil
}

func (r *Run) save() error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(r.dir, "run.json"), data)
}

// MarkDone и MarkRolled отмечают итог запуска.
func (r *Run) MarkDone() error   { r.Done = true; return r.save() }
func (r *Run) MarkRolled() error { r.Rolled = true; return r.save() }

// Snapshot открывает (создавая) снимок модуля внутри запуска.
func (r *Run) Snapshot(module string) (*Snapshot, error) {
	dir := filepath.Join(r.dir, module)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	for _, m := range r.Modules {
		if m == module {
			return &Snapshot{Module: module, dir: dir}, nil
		}
	}
	r.Modules = append(r.Modules, module)
	return &Snapshot{Module: module, dir: dir}, r.save()
}

// Snapshot — место для данных отката одного модуля.
type Snapshot struct {
	Module string
	dir    string
}

// PutJSON сохраняет произвольные данные под именем.
func (s *Snapshot) PutJSON(name string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(s.dir, name), data)
}

// GetJSON читает данные, сохранённые PutJSON.
func (s *Snapshot) GetJSON(name string, v any) error {
	data, err := os.ReadFile(filepath.Join(s.dir, name))
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// PutFile копирует файл в снимок под именем (имя может содержать подпапки).
func (s *Snapshot) PutFile(name, src string) error {
	dst := filepath.Join(s.dir, "files", name)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	return copyFile(src, dst)
}

// FilePath возвращает путь к файлу, сохранённому PutFile.
func (s *Snapshot) FilePath(name string) string { return filepath.Join(s.dir, "files", name) }

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// CopyFileAtomic копирует файл так, чтобы получатель никогда не оказался наполовину записанным.
func CopyFileAtomic(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	return copyFile(src, dst)
}

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
