// Пакет software — синхронизация набора программ через winget.
//
// Желаемый набор описан в манифесте (раздел software). Агент ставит недостающее после подтверждения,
// предлагает добавить в манифест то, что есть на других машинах, и ничего не удаляет сам:
// удаление возможно только для пакетов, явно перечисленных в software.remove, только с флагом
// и только после отдельного подтверждения по каждому пакету.
package software

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kiyoushii/EnvSync/internal/core"
)

// Package — установленный пакет winget.
type Package struct {
	ID      string
	Version string
}

// Winget — доступ к winget. В тестах подменяется, в работе — RealWinget.
type Winget interface {
	// Installed возвращает установленные пакеты; ключ — id в нижнем регистре.
	Installed(ctx context.Context) (map[string]Package, error)
	Install(ctx context.Context, p core.WingetPkg) error
	Uninstall(ctx context.Context, id string) error
}

// Manager строит план по манифесту и применяет его.
type Manager struct {
	W       Winget
	Machine string
	State   core.StateDir
	Snaps   *core.SnapshotStore
	Journal *core.Journal
	Now     func() time.Time
}

// Suggestion — пакет, которого нет в манифесте, но он стоит на какой-то машине.
type Suggestion struct {
	ID      string
	Version string
	Where   []string // имена машин
}

// Plan — что агент предлагает сделать на этой машине.
type Plan struct {
	Install   []core.WingetPkg // в манифесте, но не установлено
	Remove    []string         // явно помечено на удаление и сейчас установлено
	Manual    []core.ManualItem
	Mismatch  []string // установлена не та версия, что закреплена в манифесте (только сообщение, не меняется)
	Suggest   []Suggestion
	AlreadyOK int // сколько пакетов из манифеста уже стоят
}

// HasWork — есть ли что устанавливать или удалять.
func (p *Plan) HasWork() bool { return len(p.Install) > 0 || len(p.Remove) > 0 }

// noisePrefixes — системные компоненты, которые не стоит предлагать как «программы».
var noisePrefixes = []string{
	"microsoft.vcredist", "microsoft.vclibs", "microsoft.ui.xaml", "microsoft.dotnet.desktopruntime",
	"microsoft.dotnet.runtime", "microsoft.dotnet.aspnetcore", "microsoft.edgewebview2runtime", "microsoft.appinstaller",
}

func isNoise(id string) bool {
	l := strings.ToLower(id)
	for _, p := range noisePrefixes {
		if strings.HasPrefix(l, p) {
			return true
		}
	}
	return false
}

type machineState struct {
	Packages map[string]string `json:"packages"` // id → версия
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) log(e core.Entry) {
	if m.Journal == nil {
		return
	}
	e.Time, e.Machine, e.Module = m.now().UTC(), m.Machine, "software"
	_ = m.Journal.Append(e)
}

// Scan читает установленные пакеты и записывает их в состояние этой машины, чтобы другие машины
// могли предложить добавить их в манифест.
func (m *Manager) Scan(ctx context.Context) (map[string]Package, error) {
	installed, err := m.W.Installed(ctx)
	if err != nil {
		return nil, err
	}
	st := machineState{Packages: map[string]string{}}
	for _, p := range installed {
		st.Packages[p.ID] = p.Version
	}
	data, _ := json.Marshal(st)
	if changed, err := m.State.Write(m.Machine, "software", data, m.now()); err != nil {
		return nil, err
	} else if changed {
		m.log(core.Entry{Action: "export", Detail: fmt.Sprintf("установлено пакетов winget: %d", len(st.Packages))})
	}
	return installed, nil
}

// Plan сравнивает манифест с тем, что установлено.
func (m *Manager) Plan(ctx context.Context, set core.SoftwareSet) (*Plan, error) {
	installed, err := m.Scan(ctx)
	if err != nil {
		return nil, err
	}
	plan := &Plan{Manual: set.Manual}
	inManifest := map[string]bool{}
	for _, want := range set.Install {
		id := strings.ToLower(want.ID)
		inManifest[id] = true
		have, ok := installed[id]
		switch {
		case !ok:
			plan.Install = append(plan.Install, want)
		case want.Version != "" && have.Version != want.Version:
			plan.Mismatch = append(plan.Mismatch, fmt.Sprintf("%s: установлена %s, в манифесте %s", want.ID, have.Version, want.Version))
			plan.AlreadyOK++
		default:
			plan.AlreadyOK++
		}
	}
	for _, id := range set.Remove {
		inManifest[strings.ToLower(id)] = true // не предлагать вернуть то, что пометили на удаление
		if _, ok := installed[strings.ToLower(id)]; ok {
			plan.Remove = append(plan.Remove, id)
		}
	}

	// Предложения: пакеты на этой и на других машинах, которых нет в манифесте.
	where := map[string]*Suggestion{}
	add := func(id, version, machine string) {
		l := strings.ToLower(id)
		if inManifest[l] || isNoise(id) {
			return
		}
		s := where[l]
		if s == nil {
			s = &Suggestion{ID: id, Version: version}
			where[l] = s
		}
		s.Where = append(s.Where, machine)
	}
	for _, p := range installed {
		add(p.ID, p.Version, m.Machine)
	}
	machines, err := m.State.Machines()
	if err != nil {
		return nil, err
	}
	for _, other := range machines {
		if other == m.Machine {
			continue
		}
		sf, err := m.State.Read(other, "software")
		if err != nil || sf == nil {
			continue
		}
		var st machineState
		if json.Unmarshal(sf.Data, &st) == nil {
			for id, v := range st.Packages {
				add(id, v, other)
			}
		}
	}
	for _, s := range where {
		sort.Strings(s.Where)
		plan.Suggest = append(plan.Suggest, *s)
	}
	sort.Slice(plan.Suggest, func(i, j int) bool { return strings.ToLower(plan.Suggest[i].ID) < strings.ToLower(plan.Suggest[j].ID) })
	return plan, nil
}

// ApplyOptions — параметры применения.
type ApplyOptions struct {
	Uninstall bool                     // разрешить удаление пакетов из remove (всё равно с подтверждением)
	Confirm   func(prompt string) bool // подтверждение каждого удаления; nil = отказ
}

// Result — итог применения.
type Result struct {
	RunID     string
	Installed []string
	Removed   []string
	Failed    map[string]error // id → ошибка
}

type runRecord struct {
	Installed []core.WingetPkg `json:"installed"` // поставлены этим запуском (не стояли до него)
	Removed   []Package        `json:"removed"`   // удалены этим запуском
}

// Apply ставит недостающее. Сбой одного пакета не останавливает остальные.
// Удаляет только при Uninstall и подтверждении каждого пакета.
func (m *Manager) Apply(ctx context.Context, p *Plan, o ApplyOptions) (*Result, error) {
	res := &Result{Failed: map[string]error{}}
	if !p.HasWork() {
		return res, nil
	}
	run, err := m.Snaps.NewRun(m.Machine, "", m.now())
	if err != nil {
		return nil, err
	}
	res.RunID = run.ID
	snap, err := run.Snapshot("software")
	if err != nil {
		return nil, err
	}
	var rec runRecord
	save := func() error { return snap.PutJSON("run.json", rec) }
	if err := save(); err != nil {
		return nil, err
	}

	for _, want := range p.Install {
		if err := m.W.Install(ctx, want); err != nil {
			res.Failed[want.ID] = err
			m.log(core.Entry{Action: "error", Run: run.ID, Detail: fmt.Sprintf("установка %s: %v", want.ID, err)})
			continue
		}
		rec.Installed = append(rec.Installed, want)
		res.Installed = append(res.Installed, want.ID)
		m.log(core.Entry{Action: "apply", Run: run.ID, Changes: []string{"установлено " + want.ID}})
		if err := save(); err != nil {
			return res, err
		}
	}

	if o.Uninstall {
		installed, err := m.W.Installed(ctx)
		if err != nil {
			return res, err
		}
		for _, id := range p.Remove {
			if o.Confirm == nil || !o.Confirm(fmt.Sprintf("Удалить %s?", id)) {
				m.log(core.Entry{Action: "skip", Run: run.ID, Detail: "удаление " + id + " не подтверждено"})
				continue
			}
			before := installed[strings.ToLower(id)]
			if err := m.W.Uninstall(ctx, id); err != nil {
				res.Failed[id] = err
				m.log(core.Entry{Action: "error", Run: run.ID, Detail: fmt.Sprintf("удаление %s: %v", id, err)})
				continue
			}
			rec.Removed = append(rec.Removed, before)
			res.Removed = append(res.Removed, id)
			m.log(core.Entry{Action: "apply", Run: run.ID, Changes: []string{"удалено " + id}})
			if err := save(); err != nil {
				return res, err
			}
		}
	}

	if err := run.MarkDone(); err != nil {
		return res, err
	}
	_, scanErr := m.Scan(ctx) // новое состояние фиксируем как своё
	return res, errors.Join(scanErr)
}

// Rollback отменяет запуск: удаляет поставленное им (с подтверждением каждого пакета) и возвращает удалённое.
func (m *Manager) Rollback(ctx context.Context, runID string, confirm func(prompt string) bool) (*Result, error) {
	run, err := m.Snaps.OpenRun(runID)
	if err != nil {
		return nil, err
	}
	if run.Rolled {
		return nil, fmt.Errorf("запуск %s уже откатан", run.ID)
	}
	snap, err := run.Snapshot("software")
	if err != nil {
		return nil, err
	}
	var rec runRecord
	if err := snap.GetJSON("run.json", &rec); err != nil {
		return nil, fmt.Errorf("запись запуска %s не найдена (это не запуск программ?): %w", run.ID, err)
	}
	res := &Result{RunID: run.ID, Failed: map[string]error{}}

	for _, p := range rec.Removed { // вернуть удалённое
		if err := m.W.Install(ctx, core.WingetPkg{ID: p.ID, Version: p.Version}); err != nil {
			res.Failed[p.ID] = err
			continue
		}
		res.Installed = append(res.Installed, p.ID)
		m.log(core.Entry{Action: "rollback", Run: run.ID, Changes: []string{"возвращено " + p.ID}})
	}
	for i := len(rec.Installed) - 1; i >= 0; i-- { // удалить поставленное, в обратном порядке
		p := rec.Installed[i]
		if confirm == nil || !confirm(fmt.Sprintf("Откат: удалить %s, поставленный этим запуском?", p.ID)) {
			m.log(core.Entry{Action: "skip", Run: run.ID, Detail: "откат: удаление " + p.ID + " не подтверждено"})
			res.Failed[p.ID] = errors.New("удаление не подтверждено")
			continue
		}
		if err := m.W.Uninstall(ctx, p.ID); err != nil {
			res.Failed[p.ID] = err
			continue
		}
		res.Removed = append(res.Removed, p.ID)
		m.log(core.Entry{Action: "rollback", Run: run.ID, Changes: []string{"удалено " + p.ID}})
	}
	if len(res.Failed) == 0 {
		if err := run.MarkRolled(); err != nil {
			return res, err
		}
	}
	_, scanErr := m.Scan(ctx)
	return res, scanErr
}
