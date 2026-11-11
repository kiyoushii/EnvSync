// Пакет winreg — модуль синхронизации настроек, которые живут в реестре Windows (HKCU).
//
// Работает только по белому списку ключей (Spec): что не перечислено, агент не читает и не меняет.
// Перед изменением любого ключа сохраняется его прежнее значение, поэтому откат возможен всегда.
package winreg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/kiyoushii/EnvSync/internal/core"
)

// Value — значение реестра в переносимом виде. Data: число (dword) в десятичной записи,
// строка (string, expand) как есть, байты (binary) в hex.
type Value struct {
	Type string `json:"type"` // dword, string, expand, binary
	Data string `json:"data"`
}

func (v *Value) String() string {
	if v == nil {
		return "<нет значения>"
	}
	if v.Type == "binary" && len(v.Data) > 24 {
		return "binary:" + v.Data[:24] + "…"
	}
	return v.Type + ":" + v.Data
}

func equal(a, b *Value) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// ApplyMode — когда изменение вступит в силу.
type ApplyMode string

const (
	Immediate ApplyMode = ""         // сразу (после уведомления системы)
	Explorer  ApplyMode = "explorer" // после перезапуска проводника
	Logoff    ApplyMode = "logoff"   // после выхода из системы
)

// Spec — одна разрешённая настройка. Key задаётся относительно HKCU.
type Spec struct {
	ID    string // стабильный идентификатор, по нему состояние переносится между машинами
	Key   string
	Name  string
	Type  string // тип, который агент записывает
	Apply ApplyMode
}

func (s Spec) target() string { return `HKCU\` + s.Key + `\` + s.Name }

// Backend — доступ к реестру. В тестах подменяется, в работе — RealBackend.
type Backend interface {
	// Get возвращает значение или nil, если его нет.
	Get(key, name string) (*Value, error)
	Set(key, name string, v Value) error
	// Delete удаляет значение; отсутствие значения не ошибка.
	Delete(key, name string) error
}

// Notifier сообщает системе об изменении (например, чтобы окна перерисовали тему).
type Notifier interface {
	Notify(ctx context.Context) error
}

// Module — модуль настроек реестра с заданным белым списком.
type Module struct {
	name  string
	specs []Spec
	be    Backend
	nt    Notifier
}

// New создаёт модуль. nt может быть nil.
func New(name string, specs []Spec, be Backend, nt Notifier) *Module {
	return &Module{name: name, specs: specs, be: be, nt: nt}
}

func (m *Module) Name() string { return m.name }

type state struct {
	Values map[string]*Value `json:"values"`
}

// Export читает текущие значения всех ключей из белого списка.
func (m *Module) Export(context.Context, string) (json.RawMessage, error) {
	st := state{Values: map[string]*Value{}}
	for _, s := range m.specs {
		v, err := m.be.Get(s.Key, s.Name)
		if err != nil {
			return nil, fmt.Errorf("чтение %s: %w", s.target(), err)
		}
		st.Values[s.ID] = v
	}
	return json.Marshal(st)
}

type pending struct {
	spec    Spec
	current *Value
	want    *Value
}

// diff сравнивает желаемое состояние с текущим. Настройки, которых нет в белом списке
// или которые источник не описал, не затрагиваются.
func (m *Module) diff(desired json.RawMessage) ([]pending, error) {
	var st state
	if err := json.Unmarshal(desired, &st); err != nil {
		return nil, fmt.Errorf("состояние %s: %w", m.name, err)
	}
	var out []pending
	for _, s := range m.specs {
		want, described := st.Values[s.ID]
		if !described {
			continue
		}
		if want != nil && want.Type != s.Type {
			return nil, fmt.Errorf("%s: тип значения %q не совпадает с ожидаемым %q", s.ID, want.Type, s.Type)
		}
		cur, err := m.be.Get(s.Key, s.Name)
		if err != nil {
			return nil, fmt.Errorf("чтение %s: %w", s.target(), err)
		}
		if !equal(cur, want) {
			out = append(out, pending{spec: s, current: cur, want: want})
		}
	}
	return out, nil
}

// Plan — dry-run.
func (m *Module) Plan(_ context.Context, desired json.RawMessage, _ string) ([]core.Change, error) {
	ps, err := m.diff(desired)
	if err != nil {
		return nil, err
	}
	changes := make([]core.Change, 0, len(ps))
	for _, p := range ps {
		changes = append(changes, core.Change{
			Target:  p.spec.target(),
			Before:  p.current.String(),
			After:   p.want.String(),
			Restart: string(p.spec.Apply),
		})
	}
	return changes, nil
}

// Apply сначала целиком сохраняет прежние значения, потом меняет.
func (m *Module) Apply(ctx context.Context, desired json.RawMessage, _ string, snap *core.Snapshot) error {
	ps, err := m.diff(desired)
	if err != nil {
		return err
	}
	before := state{Values: map[string]*Value{}}
	for _, p := range ps {
		before.Values[p.spec.ID] = p.current
	}
	if err := snap.PutJSON("before.json", before); err != nil {
		return fmt.Errorf("снимок перед изменением: %w", err)
	}
	for _, p := range ps {
		if p.want == nil {
			err = m.be.Delete(p.spec.Key, p.spec.Name)
		} else {
			err = m.be.Set(p.spec.Key, p.spec.Name, *p.want)
		}
		if err != nil {
			return fmt.Errorf("запись %s: %w", p.spec.target(), err)
		}
	}
	return m.notify(ctx)
}

// Rollback возвращает значения из снимка: бывшие — на место, не существовавшие — удаляет.
func (m *Module) Rollback(ctx context.Context, snap *core.Snapshot) error {
	var before state
	if err := snap.GetJSON("before.json", &before); err != nil {
		return fmt.Errorf("снимок не найден: %w", err)
	}
	var errs []error
	for _, s := range m.specs {
		v, had := before.Values[s.ID]
		if !had {
			continue
		}
		var err error
		if v == nil {
			err = m.be.Delete(s.Key, s.Name)
		} else {
			err = m.be.Set(s.Key, s.Name, *v)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("возврат %s: %w", s.target(), err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	return m.notify(ctx)
}

func (m *Module) notify(ctx context.Context) error {
	if m.nt == nil {
		return nil
	}
	return m.nt.Notify(ctx)
}

// ParseUint32 разбирает десятичную запись dword.
func ParseUint32(s string) (uint32, error) {
	n, err := strconv.ParseUint(s, 10, 32)
	return uint32(n), err
}
