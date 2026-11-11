// Пакет appconfig — модуль синхронизации конфигов приложений (git, VS Code, терминал, Claude...).
//
// Для каждого приложения заранее перечислены файлы и папки, которые можно брать (белый список).
// Всё, что похоже на секреты (токены, пароли, закрытые ключи), в состояние не попадает.
// Модуль только создаёт и заменяет файлы: ничего не удаляет, даже если файла нет на другой машине.
package appconfig

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/kiyoushii/EnvSync/internal/core"
)

// Root — от какой папки считается путь файла. Значения раскрываются на каждой машине по-своему.
type Root string

const (
	Home         Root = "home"
	AppData      Root = "appdata"
	LocalAppData Root = "localappdata"
)

// FileSpec — файл или папка (рекурсивно). Path относительный, с прямыми слэшами.
type FileSpec struct {
	Root     Root
	Path     string
	Dir      bool
	SkipDirs []string // имена подпапок, которые не берём на любой глубине (без учёта регистра)
}

// App — одно приложение со своим белым списком файлов.
type App struct {
	ID            string
	Files         []FileSpec
	BusyProcesses []string // если хоть один из этих процессов запущен, конфиг сейчас не трогаем
}

// ProcessChecker возвращает имена запущенных процессов в нижнем регистре (например "code.exe").
type ProcessChecker func() (map[string]bool, error)

// MaxFileSize — файлы больше этого размера не синкаются (конфиги маленькие; большое — это уже данные).
const MaxFileSize = 1 << 20

// Module — модуль конфигов приложений.
type Module struct {
	apps    []App
	roots   map[Root]string
	running ProcessChecker
}

// New создаёт модуль. roots — реальные папки этой машины для home/appdata/localappdata.
// running может быть nil (тогда приложения считаются незапущенными).
func New(apps []App, roots map[Root]string, running ProcessChecker) *Module {
	return &Module{apps: apps, roots: roots, running: running}
}

func (m *Module) Name() string { return "appconfig" }

type fileInfo struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type appState struct {
	Files   map[string]fileInfo `json:"files"`
	Skipped map[string]string   `json:"skipped,omitempty"` // что не взято и почему
}

type state struct {
	Apps map[string]appState `json:"apps"`
}

// Признаки секретов. Лучше пропустить лишний файл, чем разослать пароль.
var secretRe = regexp.MustCompile(`(?im)` +
	`("[^"\n]*(api[_-]?key|token|secret|passw(or)?d|credential)[^"\n]*"\s*:\s*"[^"\n]{8,}")` +
	`|(^\s*(password|token|secret|api[_-]?key)\s*=\s*\S{6,})` +
	`|(gh[pousr]_[A-Za-z0-9]{20,})` +
	`|(sk-[A-Za-z0-9_-]{20,})` +
	`|(AKIA[0-9A-Z]{16})` +
	`|(-----BEGIN [A-Z ]*PRIVATE KEY-----)`)

// LooksSecret сообщает, похоже ли содержимое на секреты.
func LooksSecret(content []byte) bool { return secretRe.Match(content) }

type found struct {
	path string
	info fileInfo
}

// collect находит файлы приложения на этой машине.
func (m *Module) collect(app App) (files map[string]found, skipped map[string]string, err error) {
	files, skipped = map[string]found{}, map[string]string{}
	add := func(root Root, rel, full string) {
		key := string(root) + "/" + filepath.ToSlash(rel)
		fi, err := os.Stat(full)
		if err != nil {
			return
		}
		if fi.Size() > MaxFileSize {
			skipped[key] = "файл больше 1 МБ"
			return
		}
		data, err := os.ReadFile(full)
		if err != nil {
			skipped[key] = "не прочитан: " + err.Error()
			return
		}
		if LooksSecret(data) {
			skipped[key] = "похоже на секрет (токен, пароль или ключ)"
			return
		}
		sum := sha256.Sum256(data)
		files[key] = found{path: full, info: fileInfo{SHA256: hex.EncodeToString(sum[:]), Size: fi.Size()}}
	}
	for _, spec := range app.Files {
		base := m.roots[spec.Root]
		if base == "" {
			continue
		}
		full := filepath.Join(base, filepath.FromSlash(spec.Path))
		if !spec.Dir {
			if fi, err := os.Lstat(full); err == nil && fi.Mode().IsRegular() {
				add(spec.Root, spec.Path, full)
			}
			continue
		}
		_ = filepath.WalkDir(full, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				for _, skip := range spec.SkipDirs {
					if strings.EqualFold(d.Name(), skip) && p != full {
						return filepath.SkipDir
					}
				}
				return nil
			}
			if !d.Type().IsRegular() { // ссылки пропускаем
				return nil
			}
			rel, _ := filepath.Rel(base, p)
			add(spec.Root, rel, p)
			return nil
		})
	}
	return files, skipped, nil
}

func assetPath(assets, appID, key string) string {
	return filepath.Join(assets, appID, filepath.FromSlash(key))
}

// Export снимает конфиги и копирует файлы в assets. Лишние файлы в assets убираются.
func (m *Module) Export(_ context.Context, assets string) (json.RawMessage, error) {
	st := state{Apps: map[string]appState{}}
	var errs []error
	for _, app := range m.apps {
		files, skipped, _ := m.collect(app)
		as := appState{Files: map[string]fileInfo{}}
		if len(skipped) > 0 {
			as.Skipped = skipped
		}
		for key, f := range files {
			dst := assetPath(assets, app.ID, key)
			if old, err := fileSHA(dst); err != nil || old != f.info.SHA256 {
				if err := core.CopyFileAtomic(f.path, dst); err != nil {
					errs = append(errs, fmt.Errorf("%s: %w", key, err))
					continue
				}
			}
			as.Files[key] = f.info
		}
		removeStale(filepath.Join(assets, app.ID), as.Files)
		st.Apps[app.ID] = as
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return json.Marshal(st)
}

func removeStale(dir string, keep map[string]fileInfo) {
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		if _, ok := keep[filepath.ToSlash(rel)]; !ok {
			_ = os.Remove(p)
		}
		return nil
	})
}

func fileSHA(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// resolveKey превращает ключ "root/путь" в путь на этой машине. Ключи с выходом за пределы
// корня (..) и неизвестными корнями отклоняются: состояние пришло с другой машины и ему нельзя доверять слепо.
func (m *Module) resolveKey(key string) (string, error) {
	root, rel, ok := strings.Cut(key, "/")
	base := m.roots[Root(root)]
	if !ok || base == "" || rel == "" {
		return "", fmt.Errorf("недопустимый ключ файла %q", key)
	}
	full := filepath.Join(base, filepath.FromSlash(rel))
	if r, err := filepath.Rel(base, full); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("ключ файла %q выходит за пределы папки", key)
	}
	return full, nil
}

type job struct {
	app      string
	key      string
	src      string // файл в assets
	dst      string // файл на этой машине
	before   string // как сейчас
	after    string
	existing bool
}

// plan строит список изменений. Занятые приложения дают Change с Skip и не попадают в работу.
func (m *Module) plan(desired json.RawMessage, assets string) (jobs []job, changes []core.Change, err error) {
	var st state
	if err := json.Unmarshal(desired, &st); err != nil {
		return nil, nil, fmt.Errorf("состояние appconfig: %w", err)
	}
	var running map[string]bool
	if m.running != nil {
		if running, err = m.running(); err != nil {
			return nil, nil, fmt.Errorf("список процессов: %w", err)
		}
	}
	for _, app := range m.apps {
		as, ok := st.Apps[app.ID]
		if !ok {
			continue
		}
		var busy []string
		for _, p := range app.BusyProcesses {
			if running[strings.ToLower(p)] {
				busy = append(busy, p)
			}
		}
		keys := make([]string, 0, len(as.Files))
		for k := range as.Files {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		var appJobs []job
		for _, key := range keys {
			want := as.Files[key]
			dst, err := m.resolveKey(key)
			if err != nil {
				return nil, nil, fmt.Errorf("%s: %w", app.ID, err)
			}
			src := assetPath(assets, app.ID, key)
			sha, err := fileSHA(src)
			if err != nil {
				return nil, nil, fmt.Errorf("%s: файл %s ещё не доехал: %w", app.ID, key, err)
			}
			if sha != want.SHA256 {
				return nil, nil, fmt.Errorf("%s: файл %s повреждён или ещё докачивается (хеш не совпал)", app.ID, key)
			}
			cur, curErr := fileSHA(dst)
			if curErr == nil && cur == want.SHA256 {
				continue
			}
			j := job{app: app.ID, key: key, src: src, dst: dst, existing: curErr == nil, after: short(want.SHA256, want.Size)}
			j.before = "<нет файла>"
			if curErr == nil {
				fi, _ := os.Stat(dst)
				j.before = short(cur, fi.Size())
			}
			appJobs = append(appJobs, j)
		}
		for _, j := range appJobs {
			c := core.Change{Target: j.dst, Before: j.before, After: j.after}
			if len(busy) > 0 {
				c.After, c.Skip = "приложение запущено ("+strings.Join(busy, ", ")+"), будет применено после закрытия", true
			} else {
				jobs = append(jobs, j)
			}
			changes = append(changes, c)
		}
	}
	return jobs, changes, nil
}

func short(sha string, size int64) string { return fmt.Sprintf("%s…, %d байт", sha[:8], size) }

// Plan — dry-run.
func (m *Module) Plan(_ context.Context, desired json.RawMessage, assets string) ([]core.Change, error) {
	_, changes, err := m.plan(desired, assets)
	return changes, err
}

type backup struct {
	Key     string `json:"key"`
	Path    string `json:"path"`
	Existed bool   `json:"existed"`
	File    string `json:"file,omitempty"` // имя копии в снимке
}

// Apply сначала целиком сохраняет прежние файлы, потом записывает новые.
func (m *Module) Apply(_ context.Context, desired json.RawMessage, assets string, snap *core.Snapshot) error {
	jobs, _, err := m.plan(desired, assets)
	if err != nil {
		return err
	}
	backups := make([]backup, 0, len(jobs))
	for i, j := range jobs {
		b := backup{Key: j.app + "/" + j.key, Path: j.dst, Existed: j.existing}
		if j.existing {
			b.File = fmt.Sprintf("%04d", i)
			if err := snap.PutFile(b.File, j.dst); err != nil {
				return fmt.Errorf("резервная копия %s: %w", j.dst, err)
			}
		}
		backups = append(backups, b)
	}
	if err := snap.PutJSON("before.json", backups); err != nil {
		return fmt.Errorf("снимок перед изменением: %w", err)
	}
	for _, j := range jobs {
		if err := core.CopyFileAtomic(j.src, j.dst); err != nil {
			return fmt.Errorf("запись %s: %w", j.dst, err)
		}
	}
	return nil
}

// Rollback возвращает прежние файлы, а созданные при применении удаляет.
func (m *Module) Rollback(_ context.Context, snap *core.Snapshot) error {
	var backups []backup
	if err := snap.GetJSON("before.json", &backups); err != nil {
		return fmt.Errorf("снимок не найден: %w", err)
	}
	var errs []error
	for _, b := range backups {
		if b.Existed {
			if err := core.CopyFileAtomic(snap.FilePath(b.File), b.Path); err != nil {
				errs = append(errs, fmt.Errorf("возврат %s: %w", b.Path, err))
			}
		} else if err := os.Remove(b.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("удаление созданного %s: %w", b.Path, err))
		}
	}
	return errors.Join(errs...)
}
