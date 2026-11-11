// Пакет wallpaper — модуль синхронизации обоев рабочего стола.
//
// Синкается сам файл картинки (кладётся рядом с состоянием машины и едет через Syncthing) и режим
// подгонки. Картинки из папки Windows (стандартные обои) не копируются: передаётся только путь.
package wallpaper

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kiyoushii/EnvSync/internal/core"
)

// Setting — обои так, как их видит система.
type Setting struct {
	Path  string // полный путь к картинке, пусто = нет обоев
	Style string // WallpaperStyle: 0 по центру, 2 растянуть, 6 по размеру, 10 заполнить, 22 расширить
	Tile  string // TileWallpaper: "1" мозаика
}

// Backend — доступ к обоям. В тестах подменяется, в работе — RealBackend.
type Backend interface {
	Get() (Setting, error)
	// Set записывает режим и путь и просит систему перерисовать рабочий стол.
	Set(Setting) error
}

// Module — модуль обоев.
type Module struct {
	be            Backend
	dataDir       string // куда складываются принятые картинки на этой машине
	systemRoot    string // папка Windows: картинки из неё считаются стандартными
	StyleOverride string // режим подгонки этой машины (из профиля); пусто = как у источника
}

// New создаёт модуль. dataDir — постоянная папка для принятых картинок (не папка состояния:
// та может обновляться синхронизацией).
func New(be Backend, dataDir, systemRoot string) *Module {
	return &Module{be: be, dataDir: dataDir, systemRoot: systemRoot}
}

func (m *Module) Name() string { return "wallpaper" }

type state struct {
	Has    bool   `json:"has"`
	System bool   `json:"system,omitempty"` // стандартная картинка Windows: файл не передаётся
	Path   string `json:"path,omitempty"`   // только для System
	File   string `json:"file,omitempty"`   // имя файла в папке assets
	SHA256 string `json:"sha256,omitempty"`
	Style  string `json:"style"`
	Tile   string `json:"tile"`
}

func (m *Module) isSystem(path string) bool {
	if m.systemRoot == "" {
		return false
	}
	root := strings.ToLower(filepath.Clean(m.systemRoot)) + `\`
	return strings.HasPrefix(strings.ToLower(filepath.Clean(path))+`\`, root)
}

func fileSHA(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Export снимает текущие обои. Нестандартную картинку копирует в assets.
func (m *Module) Export(_ context.Context, assets string) (json.RawMessage, error) {
	cur, err := m.be.Get()
	if err != nil {
		return nil, err
	}
	st := state{Style: cur.Style, Tile: cur.Tile}
	switch {
	case cur.Path == "":
		// обоев нет
	case m.isSystem(cur.Path):
		st.Has, st.System, st.Path = true, true, cur.Path
	default:
		sha, err := fileSHA(cur.Path)
		if err != nil {
			// Картинка недоступна (например, диск отключён): состояние «обоев нет» было бы ложным,
			// а ошибка не должна останавливать экспорт остальных модулей.
			return nil, fmt.Errorf("файл обоев %s: %w", cur.Path, err)
		}
		name := "wallpaper" + strings.ToLower(filepath.Ext(cur.Path))
		dst := filepath.Join(assets, name)
		if old, err := fileSHA(dst); err != nil || old != sha {
			if err := core.CopyFileAtomic(cur.Path, dst); err != nil {
				return nil, err
			}
		}
		m.removeStaleAssets(assets, name)
		st.Has, st.File, st.SHA256 = true, name, sha
	}
	return json.Marshal(st)
}

// removeStaleAssets оставляет в assets только текущую картинку.
func (m *Module) removeStaleAssets(assets, keep string) {
	entries, _ := os.ReadDir(assets)
	for _, e := range entries {
		if !e.IsDir() && e.Name() != keep {
			_ = os.Remove(filepath.Join(assets, e.Name()))
		}
	}
}

// target определяет, какой путь и режим должны получиться на этой машине, и проверяет, что картинка доступна.
func (m *Module) target(desired json.RawMessage, assets string) (want Setting, src string, st state, err error) {
	if err = json.Unmarshal(desired, &st); err != nil {
		return
	}
	want.Style, want.Tile = st.Style, st.Tile
	if m.StyleOverride != "" {
		want.Style = m.StyleOverride
	}
	switch {
	case !st.Has:
	case st.System:
		if _, e := os.Stat(st.Path); e != nil {
			err = fmt.Errorf("на этой машине нет стандартной картинки %s", st.Path)
			return
		}
		want.Path = st.Path
	default:
		src = filepath.Join(assets, st.File)
		sha, e := fileSHA(src)
		if e != nil {
			err = fmt.Errorf("картинка обоев ещё не доехала: %w", e)
			return
		}
		if sha != st.SHA256 {
			err = errors.New("картинка обоев повреждена или ещё докачивается (хеш не совпал)")
			return
		}
		want.Path = filepath.Join(m.dataDir, st.SHA256[:16]+strings.ToLower(filepath.Ext(st.File)))
	}
	return
}

func describe(s Setting) string {
	if s.Path == "" {
		return "нет обоев"
	}
	return fmt.Sprintf("%s (режим %s)", s.Path, s.Style)
}

// Plan — dry-run.
func (m *Module) Plan(_ context.Context, desired json.RawMessage, assets string) ([]core.Change, error) {
	want, _, st, err := m.target(desired, assets)
	if err != nil {
		return nil, err
	}
	cur, err := m.be.Get()
	if err != nil {
		return nil, err
	}
	if m.same(cur, want, st) {
		return nil, nil
	}
	return []core.Change{{Target: "обои рабочего стола", Before: describe(cur), After: describe(want)}}, nil
}

// same сравнивает не пути, а содержимое: одна и та же картинка в другом месте — не изменение.
func (m *Module) same(cur, want Setting, st state) bool {
	if cur.Style != want.Style || cur.Tile != want.Tile {
		return false
	}
	if cur.Path == want.Path {
		return true
	}
	if st.Has && !st.System && st.SHA256 != "" && cur.Path != "" {
		if sha, err := fileSHA(cur.Path); err == nil && sha == st.SHA256 {
			return true
		}
	}
	return false
}

// Apply запоминает прежние обои, принимает картинку к себе и ставит её.
func (m *Module) Apply(_ context.Context, desired json.RawMessage, assets string, snap *core.Snapshot) error {
	want, src, _, err := m.target(desired, assets)
	if err != nil {
		return err
	}
	cur, err := m.be.Get()
	if err != nil {
		return err
	}
	if err := snap.PutJSON("before.json", cur); err != nil {
		return fmt.Errorf("снимок перед изменением: %w", err)
	}
	if src != "" {
		if err := core.CopyFileAtomic(src, want.Path); err != nil {
			return fmt.Errorf("сохранение картинки: %w", err)
		}
	}
	return m.be.Set(want)
}

// Rollback возвращает прежние обои. Прежняя картинка не копировалась и не удалялась, поэтому она на месте.
func (m *Module) Rollback(_ context.Context, snap *core.Snapshot) error {
	var before Setting
	if err := snap.GetJSON("before.json", &before); err != nil {
		return fmt.Errorf("снимок не найден: %w", err)
	}
	return m.be.Set(before)
}
