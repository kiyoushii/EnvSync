// Пакет app — «живой» бэкенд интерфейса: собирает статус из Tailscale, Syncthing и состояния машин
// и выполняет решения человека (предохранитель, пауза).
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kiyoushii/EnvSync/internal/core"
	"github.com/kiyoushii/EnvSync/internal/transport"
	"github.com/kiyoushii/EnvSync/internal/ui"
)

// Backend реализует ui.Backend на настоящих данных.
type Backend struct {
	S       transport.Settings
	TS      *transport.Tailscale
	Machine string
	Version string
	Guard   *transport.Guard
	Engine  func() (*core.Engine, error) // для просмотра плана настроек; nil = раздел недоступен
	Audit   *core.Journal                // журнал аудита

	mu           sync.Mutex
	conflictsAt  time.Time
	conflictsVal []ui.Conflict
}

func (b *Backend) client() (*transport.Client, error) {
	key, err := transport.ReadAPIKey(b.S.ConfigXMLPath())
	if err != nil {
		return nil, errors.New("конфига Syncthing ещё нет: выполните envsync init")
	}
	return transport.NewClient(b.S.GUIPort, key), nil
}

// Self снимает состояние этой машины вживую.
func (b *Backend) Self(ctx context.Context) (ui.Status, error) {
	st := ui.Status{Machine: b.Machine, At: time.Now().UTC(), Version: b.Version, Guard: []ui.GuardTrip{}}
	st.Syncthing.Devices, st.Syncthing.Folders = []ui.DeviceStatus{}, []ui.FolderStatus{}
	st.Tailscale.Peers = []ui.Peer{}

	if ts, err := b.TS.Status(ctx); err != nil {
		st.Tailscale.State = "недоступен: " + err.Error()
	} else {
		st.Tailscale.State = ts.BackendState
		st.Tailscale.Name, st.Tailscale.IP = ts.Self.Name, ts.Self.IPv4.String()
		if !ts.Self.IPv4.IsValid() {
			st.Tailscale.IP = ""
		}
		for _, p := range ts.Peers {
			st.Tailscale.Peers = append(st.Tailscale.Peers, ui.Peer{Name: p.Name, IP: p.IPv4.String(), Online: p.Online})
		}
	}

	if trips, err := b.Guard.Tripped(); err == nil {
		for _, t := range trips {
			st.Guard = append(st.Guard, ui.GuardTrip{Folder: t.FolderID, At: t.At, NeedDeletes: t.NeedDeletes, GlobalFiles: t.GlobalFiles})
		}
	}

	c, err := b.client()
	if err != nil {
		return st, nil
	}
	rctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	selfID, err := c.MyID(rctx)
	if err != nil {
		return st, nil // Syncthing не запущен
	}
	st.Syncthing.Running, st.Syncthing.DeviceID = true, selfID

	conns, _ := c.Connections(rctx)
	devs, _ := c.DeviceList(rctx, selfID)
	allPaused := len(devs) > 0
	for _, d := range devs {
		cn := conns[d.ID]
		addr := cn.Address
		if addr == "" && len(d.Addresses) > 0 {
			addr = d.Addresses[0]
		}
		st.Syncthing.Devices = append(st.Syncthing.Devices, ui.DeviceStatus{ID: d.ID, Name: d.Name, Connected: cn.Connected, Paused: d.Paused || cn.Paused, Address: addr})
		allPaused = allPaused && (d.Paused || cn.Paused)
	}
	st.Paused = allPaused

	folders, _ := c.FolderList(rctx)
	for _, f := range folders {
		fs := ui.FolderStatus{ID: f.ID, Path: f.Path, Paused: f.Paused, State: "paused"}
		if !f.Paused {
			if s, err := c.FolderStatus(rctx, f.ID); err == nil {
				fs.State, fs.LocalFiles, fs.GlobalFiles = s.State, s.LocalFiles, s.GlobalFiles
				fs.NeedFiles, fs.NeedDeletes, fs.NeedBytes, fs.Errors = s.NeedFiles, s.NeedDeletes, s.NeedBytes, s.PullErrors
			}
		}
		st.Syncthing.Folders = append(st.Syncthing.Folders, fs)
	}
	return st, nil
}

// ExportStatus записывает статус этой машины в папку состояний, чтобы вторая машина его видела.
func (b *Backend) ExportStatus(ctx context.Context) error {
	st, err := b.Self(ctx)
	if err != nil {
		return err
	}
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	_, err = core.StateDir{Root: b.S.StateDir()}.Write(b.Machine, "status", data, time.Now())
	return err
}

// StatusLoop раз в every записывает статус, пока не отменён ctx.
func (b *Backend) StatusLoop(ctx context.Context, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	_ = b.ExportStatus(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			_ = b.ExportStatus(ctx)
		}
	}
}

// Others читает последние известные статусы других машин из папки состояний.
func (b *Backend) Others(ctx context.Context) ([]ui.Status, error) {
	dir := core.StateDir{Root: b.S.StateDir()}
	machines, err := dir.Machines()
	if err != nil {
		return nil, err
	}
	var out []ui.Status
	for _, m := range machines {
		if strings.EqualFold(m, b.Machine) {
			continue
		}
		sf, err := dir.Read(m, "status")
		if err != nil || sf == nil {
			continue
		}
		var st ui.Status
		if json.Unmarshal(sf.Data, &st) == nil {
			out = append(out, st)
		}
	}
	return out, nil
}

// Journal возвращает последние записи журнала.
func (b *Backend) Journal(n int) ([]core.Entry, error) {
	if b.Audit == nil {
		return nil, nil
	}
	return b.Audit.Recent(n)
}

var conflictRe = regexp.MustCompile(`^(.*)\.sync-conflict-\d{8}-\d{6}(?:-[A-Z0-9]+)?(\.[^.]*)?$`)

// Conflicts ищет конфликтные копии во всех папках (результат кэшируется на 30 секунд: обход большой папки не бесплатен).
func (b *Backend) Conflicts(ctx context.Context) ([]ui.Conflict, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if time.Since(b.conflictsAt) < 30*time.Second && b.conflictsVal != nil {
		return b.conflictsVal, nil
	}
	c, err := b.client()
	if err != nil {
		return nil, err
	}
	folders, err := c.FolderList(ctx)
	if err != nil {
		return nil, nil // Syncthing не запущен: конфликтов не показываем
	}
	out := []ui.Conflict{}
	for _, f := range folders {
		if f.ID == transport.StateFolderID || f.Path == "" {
			continue
		}
		visited := 0
		_ = filepath.WalkDir(f.Path, func(p string, d fs.DirEntry, err error) error {
			if err != nil || ctx.Err() != nil {
				return nil
			}
			name := d.Name()
			if d.IsDir() {
				if name == ".stfolder" || name == ".stversions" || strings.EqualFold(name, "node_modules") || strings.EqualFold(name, ".git") {
					return filepath.SkipDir
				}
				return nil
			}
			if visited++; visited > 500000 {
				return filepath.SkipAll
			}
			m := conflictRe.FindStringSubmatch(name)
			if m == nil {
				return nil
			}
			info, _ := d.Info()
			rel, _ := filepath.Rel(f.Path, p)
			cf := ui.Conflict{Folder: f.ID, Path: filepath.ToSlash(rel), Original: m[1] + m[2]}
			if info != nil {
				cf.Modified, cf.Size = info.ModTime(), info.Size()
			}
			out = append(out, cf)
			return nil
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
	b.conflictsVal, b.conflictsAt = out, time.Now()
	return out, nil
}

// NotSynced собирает экран «что не синхронизируется»: постоянное и то, что зависит от манифеста и состояния.
func (b *Backend) NotSynced(ctx context.Context) ([]ui.NotSyncedSection, error) {
	secs := core.NotSyncedStatic()

	ignores := core.DefaultIgnores
	var manual []core.ManualItem
	if m, err := core.LoadManifest(b.S.ManifestFile()); err == nil {
		if len(m.Ignore) > 0 {
			ignores = m.Ignore
		}
		if m.Software != nil {
			manual = m.Software.Manual
		}
	}
	var items []core.NotSyncedItem
	for _, l := range ignores {
		l = strings.TrimSpace(l)
		if l != "" && !strings.HasPrefix(l, "//") {
			items = append(items, core.NotSyncedItem{What: strings.TrimPrefix(l, "(?i)"), Why: "исключено правилами синхронизации папок", Workaround: "изменить список ignore в манифесте"})
		}
	}
	secs = append(secs, core.NotSyncedSection{Title: "Исключено правилами папок (действует сейчас)", Items: items})

	// Файлы конфигов, которые модуль appconfig не взял как секреты или как слишком большие.
	if sf, err := (core.StateDir{Root: b.S.StateDir()}).Read(b.Machine, "appconfig"); err == nil && sf != nil {
		var st struct {
			Apps map[string]struct {
				Skipped map[string]string `json:"skipped"`
			} `json:"apps"`
		}
		if json.Unmarshal(sf.Data, &st) == nil {
			var skipped []core.NotSyncedItem
			for app, a := range st.Apps {
				for file, why := range a.Skipped {
					skipped = append(skipped, core.NotSyncedItem{What: app + ": " + file, Why: why, Workaround: "перенести вручную, если это не секрет"})
				}
			}
			sort.Slice(skipped, func(i, j int) bool { return skipped[i].What < skipped[j].What })
			if len(skipped) > 0 {
				secs = append(secs, core.NotSyncedSection{Title: "Конфиги, которые не взяты на этой машине", Items: skipped})
			}
		}
	}
	for _, m := range manual {
		secs = append(secs, core.NotSyncedSection{Title: "Программы вручную", Items: []core.NotSyncedItem{{What: m.Name, Why: "нет в winget", Workaround: m.Note}}})
	}
	return secs, nil
}

// SettingsPlan показывает, что изменилось бы на этой машине по настройкам другой (только просмотр).
func (b *Backend) SettingsPlan(ctx context.Context) ([]ui.PlanItem, error) {
	if b.Engine == nil {
		return nil, errors.New("модули настроек недоступны")
	}
	eng, err := b.Engine()
	if err != nil {
		return nil, err
	}
	items, err := eng.Plan(ctx, nil)
	if err != nil {
		return nil, err
	}
	var out []ui.PlanItem
	for _, it := range items {
		pi := ui.PlanItem{Module: it.Module, Source: it.Source}
		if it.Err != nil {
			pi.Error = it.Err.Error()
		}
		for _, c := range it.Changes {
			pi.Changes = append(pi.Changes, c.String())
		}
		out = append(out, pi)
	}
	return out, nil
}

// GuardAccept и GuardReject выполняют решение человека по сработавшему предохранителю.
func (b *Backend) GuardAccept(ctx context.Context, folder string) error {
	return b.Guard.Accept(ctx, folder)
}

func (b *Backend) GuardReject(ctx context.Context, folder string) (string, error) {
	res, err := b.Guard.Reject(ctx, folder)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("удаление отклонено: оставлено %d, возвращено из корзины %d, не найдено %d; папка возобновлена", res.Kept, res.Restored, res.Missing), nil
}

// SetPaused — «пауза всего» и возобновление.
func (b *Backend) SetPaused(ctx context.Context, paused bool) error {
	c, err := b.client()
	if err != nil {
		return err
	}
	action, detail := "resume-all", "синхронизация возобновлена из интерфейса"
	if paused {
		err, action, detail = c.PauseAll(ctx), "pause-all", "синхронизация приостановлена из интерфейса"
	} else {
		err = c.ResumeAll(ctx)
	}
	if err != nil {
		return err
	}
	if b.Audit != nil {
		_ = b.Audit.Append(core.Entry{Time: time.Now().UTC(), Machine: b.Machine, Module: "guard", Action: action, Detail: detail})
	}
	return nil
}
