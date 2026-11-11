package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Change — одно изменение, которое Apply внесёт в систему. Показывается человеку в dry-run.
type Change struct {
	Target  string // что меняется: ключ реестра, файл и т. п.
	Before  string // как сейчас
	After   string // как станет
	Restart string // "" — применится сразу; иначе подсказка: "explorer" (перезапуск проводника) или "logoff" (выход из системы)
	Skip    bool   // изменение не будет применено (например, приложение запущено); показывается, чтобы было понятно почему
}

func (c Change) String() string {
	s := fmt.Sprintf("%s: %s → %s", c.Target, c.Before, c.After)
	if c.Skip {
		s += " [пропущено]"
	}
	switch c.Restart {
	case "explorer":
		s += " (вступит в силу после перезапуска проводника)"
	case "logoff":
		s += " (вступит в силу после выхода из системы)"
	}
	return s
}

// Module — один тип синхронизируемых настроек (оформление, проводник, конфиги приложений...).
// Новый вид синка добавляется новым модулем, ядро не переписывается.
type Module interface {
	Name() string
	// Export снимает текущее состояние этой машины в декларативном виде. Файлы (например, обои)
	// модуль складывает в assets, эта папка синкается вместе с состоянием.
	Export(ctx context.Context, assets string) (json.RawMessage, error)
	// Plan — dry-run: сравнивает желаемое состояние (с другой машины) с текущим и возвращает
	// список изменений. Ничего не меняет.
	Plan(ctx context.Context, desired json.RawMessage, assets string) ([]Change, error)
	// Apply применяет желаемое состояние. Перед любым изменением складывает в snap всё нужное для отката.
	Apply(ctx context.Context, desired json.RawMessage, assets string, snap *Snapshot) error
	// Rollback возвращает состояние, каким оно было до Apply.
	Rollback(ctx context.Context, snap *Snapshot) error
}

// PlanItem — запланированные изменения одного модуля и откуда они пришли.
type PlanItem struct {
	Module  string
	Source  string // имя машины-источника
	Changes []Change
	Err     error // модуль не смог построить план (например, нужный файл ещё не доехал); остальные модули это не останавливает

	desired json.RawMessage
	assets  string
}

// HasWork — есть ли в плане хоть одно изменение, которое будет применено.
func (p PlanItem) HasWork() bool {
	if p.Err != nil {
		return false
	}
	for _, c := range p.Changes {
		if !c.Skip {
			return true
		}
	}
	return false
}

// Engine связывает модули, состояние машин, снимки и журнал.
type Engine struct {
	Machine string
	State   StateDir
	Snaps   *SnapshotStore
	Journal *Journal
	Modules []Module
	Now     func() time.Time
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Engine) module(name string) Module {
	for _, m := range e.Modules {
		if m.Name() == name {
			return m
		}
	}
	return nil
}

func (e *Engine) selected(only []string) []Module {
	if len(only) == 0 {
		return e.Modules
	}
	var out []Module
	for _, m := range e.Modules {
		for _, o := range only {
			if m.Name() == o {
				out = append(out, m)
			}
		}
	}
	return out
}

func (e *Engine) log(en Entry) {
	if e.Journal == nil {
		return
	}
	en.Time, en.Machine = e.now().UTC(), e.Machine
	_ = e.Journal.Append(en) // сбой журнала не должен ломать применение настроек
}

// Export снимает состояние всех модулей и записывает его в папку состояний этой машины.
// Возвращает имена модулей, чьё состояние изменилось с прошлого раза.
func (e *Engine) Export(ctx context.Context) ([]string, error) {
	var changed []string
	var errs []error
	for _, m := range e.Modules {
		data, err := m.Export(ctx, e.State.AssetDir(e.Machine, m.Name()))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", m.Name(), err))
			continue
		}
		ch, err := e.State.Write(e.Machine, m.Name(), data, e.now())
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", m.Name(), err))
			continue
		}
		if ch {
			changed = append(changed, m.Name())
			e.log(Entry{Module: m.Name(), Action: "export", Detail: "состояние изменилось"})
		}
	}
	return changed, errors.Join(errs...)
}

// Plan строит dry-run: для каждого модуля берёт самое свежее состояние другой машины и,
// если оно новее нашего, считает изменения. «Побеждает последняя правка».
func (e *Engine) Plan(ctx context.Context, only []string) ([]PlanItem, error) {
	// Сначала фиксируем собственное состояние: если пользователь менял настройку локально позже,
	// чем другая машина, его правка новее и не должна затираться.
	if _, err := e.Export(ctx); err != nil {
		return nil, err
	}
	machines, err := e.State.Machines()
	if err != nil {
		return nil, err
	}
	var items []PlanItem
	for _, m := range e.selected(only) {
		self, err := e.State.Read(e.Machine, m.Name())
		if err != nil {
			return nil, err
		}
		var best *StateFile
		for _, other := range machines {
			if other == e.Machine {
				continue
			}
			sf, err := e.State.Read(other, m.Name())
			if err != nil {
				return nil, err
			}
			if sf != nil && (best == nil || sf.ChangedAt.After(best.ChangedAt)) {
				best = sf
			}
		}
		if best == nil || (self != nil && !best.ChangedAt.After(self.ChangedAt)) {
			continue
		}
		assets := e.State.AssetDir(best.Machine, m.Name())
		changes, err := m.Plan(ctx, best.Data, assets)
		if err != nil {
			// Проблема одного модуля не должна останавливать остальные: показываем её в плане.
			items = append(items, PlanItem{Module: m.Name(), Source: best.Machine, Err: err})
			continue
		}
		if len(changes) > 0 {
			items = append(items, PlanItem{Module: m.Name(), Source: best.Machine, Changes: changes, desired: best.Data, assets: assets})
		}
	}
	return items, nil
}

// Apply применяет запланированное. Перед изменением каждого модуля делается снимок; если какой-то модуль
// упал, все уже применённые в этом запуске модули откатываются, система остаётся как была.
// Возвращает запуск (nil, если менять нечего) и применённый план.
func (e *Engine) Apply(ctx context.Context, only []string) (*Run, []PlanItem, error) {
	all, err := e.Plan(ctx, only)
	if err != nil {
		return nil, nil, err
	}
	var items []PlanItem
	for _, it := range all {
		if it.HasWork() {
			items = append(items, it)
		} else if it.Err != nil {
			e.log(Entry{Module: it.Module, Action: "skip", Source: it.Source, Detail: it.Err.Error()})
		}
	}
	if len(items) == 0 {
		return nil, nil, nil
	}
	run, err := e.Snaps.NewRun(e.Machine, items[0].Source, e.now())
	if err != nil {
		return nil, nil, err
	}
	for _, it := range items {
		m := e.module(it.Module)
		snap, err := run.Snapshot(it.Module)
		if err == nil {
			err = m.Apply(ctx, it.desired, it.assets, snap)
		}
		if err != nil {
			e.log(Entry{Module: it.Module, Action: "error", Run: run.ID, Source: it.Source, Detail: err.Error()})
			rbErr := e.rollbackRun(ctx, run)
			return run, items, errors.Join(fmt.Errorf("%s: %w (все изменения этого запуска откатаны)", it.Module, err), rbErr)
		}
		var ch []string
		for _, c := range it.Changes {
			ch = append(ch, c.String())
		}
		e.log(Entry{Module: it.Module, Action: "apply", Run: run.ID, Source: it.Source, Changes: ch})
	}
	if err := run.MarkDone(); err != nil {
		return run, items, err
	}
	// Новое состояние машины фиксируем сразу: теперь оно «наше», и другая машина не будет его пересылать обратно.
	_, err = e.Export(ctx)
	return run, items, err
}

// Rollback откатывает запуск (id "" — последний) во всех его модулях.
func (e *Engine) Rollback(ctx context.Context, id string) (*Run, error) {
	run, err := e.Snaps.OpenRun(id)
	if err != nil {
		return nil, err
	}
	if run.Rolled {
		return run, fmt.Errorf("запуск %s уже откатан", run.ID)
	}
	if err := e.rollbackRun(ctx, run); err != nil {
		return run, err
	}
	_, err = e.Export(ctx)
	return run, err
}

// rollbackRun откатывает модули запуска в обратном порядке и помечает запуск откатанным.
func (e *Engine) rollbackRun(ctx context.Context, run *Run) error {
	var errs []error
	for i := len(run.Modules) - 1; i >= 0; i-- {
		name := run.Modules[i]
		m := e.module(name)
		if m == nil {
			errs = append(errs, fmt.Errorf("модуль %q не найден", name))
			continue
		}
		snap, err := run.Snapshot(name)
		if err == nil {
			err = m.Rollback(ctx, snap)
		}
		if err != nil {
			e.log(Entry{Module: name, Action: "error", Run: run.ID, Detail: "откат: " + err.Error()})
			errs = append(errs, fmt.Errorf("откат %s: %w", name, err))
			continue
		}
		e.log(Entry{Module: name, Action: "rollback", Run: run.ID})
	}
	if len(errs) == 0 {
		errs = append(errs, run.MarkRolled())
	}
	return errors.Join(errs...)
}
