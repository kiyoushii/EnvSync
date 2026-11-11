// Пакет core — ядро EnvSync, не зависящее от Syncthing: манифест, переменные путей, профили машин.
package core

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// SupportedSchema — какую версию схемы манифеста понимает этот агент.
// Манифест новее агента не применяется: агент старой версии не должен «понимать наполовину»
// и портить данные (принцип из раздела 7 исходного плана).
//
// Версии схемы: 1 — папки и профили машин; 2 — добавлен раздел software (программы через winget).
// Манифест версии 1 по-прежнему корректен.
const SupportedSchema = 2

// Manifest — содержимое envsync.yaml: что синкается и чем отличаются машины.
// Что в манифесте не написано, агент не трогает.
type Manifest struct {
	Schema   int                `yaml:"schema"`
	Vars     map[string]string  `yaml:"vars"`     // свои переменные путей, доступны как {{имя}}
	Ignore   []string           `yaml:"ignore"`   // общие исключения; если не задано, берутся DefaultIgnores
	Folders  []FolderDef        `yaml:"folders"`  // синкаемые папки
	Software *SoftwareDef       `yaml:"software"` // программы (схема 2)
	Machines map[string]Machine `yaml:"machines"` // профили машин по имени компьютера
}

// SoftwareDef — желаемый набор программ.
type SoftwareDef struct {
	Winget []WingetPkg  `yaml:"winget"` // ставятся через winget
	Remove []string     `yaml:"remove"` // удалять только явно перечисленное и только с подтверждением
	Manual []ManualItem `yaml:"manual"` // программы, которых нет в winget: показываются списком «вручную»
}

// WingetPkg — пакет winget.
type WingetPkg struct {
	ID      string `yaml:"id"`
	Version string `yaml:"version"` // пусто = последняя
	Scope   string `yaml:"scope"`   // user или machine; пусто = по умолчанию пакета
}

// ManualItem — программа, которую нужно поставить вручную.
type ManualItem struct {
	Name string `yaml:"name"`
	Note string `yaml:"note"`
}

// SoftwareOverride — переопределения программ для машины.
type SoftwareOverride struct {
	Skip []string `yaml:"skip"` // эти пакеты на этой машине не ставить и не удалять
}

// FolderDef — одна синкаемая папка. ID одинаков на всех машинах, путь у каждой свой (через переменные).
type FolderDef struct {
	ID       string   `yaml:"id"`
	Path     string   `yaml:"path"`
	Ignore   []string `yaml:"ignore"`    // добавляется к общим исключениям
	KeepDays int      `yaml:"keep_days"` // сколько дней хранить удалённое в «корзине»; 0 = 30
	Test     bool     `yaml:"test"`      // тестовая папка: разрешена, пока синк реальных данных выключен
}

// Machine — переопределения для конкретной машины.
type Machine struct {
	Vars     map[string]string         `yaml:"vars"`     // переопределяют переменные (например sync_root на другом диске)
	Folders  map[string]FolderOverride `yaml:"folders"`  // переопределения по ID папки
	Software SoftwareOverride          `yaml:"software"` // переопределения программ
}

// FolderOverride — особый путь папки на этой машине или её пропуск.
type FolderOverride struct {
	Path string `yaml:"path"`
	Skip bool   `yaml:"skip"`
}

var (
	folderIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	varNameRe  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	wingetIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$`)
)

func (s *SoftwareDef) validate() []error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	seen := map[string]bool{}
	for i, p := range s.Winget {
		key := strings.ToLower(p.ID)
		switch {
		case !wingetIDRe.MatchString(p.ID):
			add("software.winget[%d]: недопустимый id пакета %q", i, p.ID)
		case seen[key]:
			add("software.winget[%d]: пакет %q повторяется", i, p.ID)
		}
		seen[key] = true
		if p.Scope != "" && p.Scope != "user" && p.Scope != "machine" {
			add("software.winget[%d] (%s): scope должен быть user или machine", i, p.ID)
		}
		if strings.ContainsAny(p.Version, " \t\"'&|;<>") {
			add("software.winget[%d] (%s): недопустимая версия %q", i, p.ID, p.Version)
		}
	}
	for i, id := range s.Remove {
		if !wingetIDRe.MatchString(id) {
			add("software.remove[%d]: недопустимый id пакета %q", i, id)
		}
		if seen[strings.ToLower(id)] {
			add("software.remove[%d]: пакет %q одновременно в winget и в remove", i, id)
		}
	}
	for i, it := range s.Manual {
		if strings.TrimSpace(it.Name) == "" {
			add("software.manual[%d]: не указано name", i)
		}
	}
	return errs
}

// reservedVars — встроенные переменные, их нельзя объявлять в манифесте заново, кроме sync_root.
var reservedVars = map[string]bool{"user": true, "home": true, "appdata": true, "localappdata": true, "machine": true}

// ParseManifest разбирает и проверяет манифест. Неизвестные поля — ошибка: опечатка
// не должна молча превращаться в «настройка не применилась».
func ParseManifest(data []byte) (*Manifest, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("манифест пуст")
		}
		return nil, fmt.Errorf("разбор манифеста: %w", err)
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// LoadManifest читает манифест из файла.
func LoadManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m, err := ParseManifest(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

// validate проверяет то, что можно проверить без знания о конкретной машине.
func (m *Manifest) validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	switch {
	case m.Schema == 0:
		add("не указана версия схемы (schema: %d)", SupportedSchema)
	case m.Schema > SupportedSchema:
		add("манифест версии %d новее агента (поддерживается %d): обновите агент", m.Schema, SupportedSchema)
	case m.Schema < 0:
		add("версия схемы не может быть отрицательной")
	}

	for name := range m.Vars {
		if !varNameRe.MatchString(name) {
			add("vars: недопустимое имя переменной %q (строчные буквы, цифры, _)", name)
		}
		if reservedVars[name] {
			add("vars: %q — встроенная переменная, переопределять нельзя", name)
		}
	}
	for _, l := range m.Ignore {
		if strings.TrimSpace(l) == "" {
			add("ignore: пустой шаблон")
		}
	}

	seen := map[string]bool{}
	for i, f := range m.Folders {
		switch {
		case !folderIDRe.MatchString(f.ID):
			add("folders[%d]: недопустимый id %q (строчные латинские буквы, цифры, . _ -)", i, f.ID)
		case seen[f.ID]:
			add("folders[%d]: id %q повторяется", i, f.ID)
		}
		seen[f.ID] = true
		if strings.TrimSpace(f.Path) == "" {
			add("folders[%d] (%s): не указан path", i, f.ID)
		}
		if f.KeepDays < 0 {
			add("folders[%d] (%s): keep_days не может быть отрицательным", i, f.ID)
		}
	}

	if m.Software != nil {
		if m.Schema < 2 {
			add("раздел software требует schema: 2")
		}
		errs = append(errs, m.Software.validate()...)
	}

	lower := map[string]string{}
	for name, mc := range m.Machines {
		for _, id := range mc.Software.Skip {
			if !wingetIDRe.MatchString(id) {
				add("machines.%s.software.skip: недопустимый id пакета %q", name, id)
			}
		}
		key := strings.ToLower(name)
		if prev, dup := lower[key]; dup {
			add("machines: имена %q и %q различаются только регистром", prev, name)
		}
		lower[key] = name
		for v := range mc.Vars {
			if !varNameRe.MatchString(v) || (reservedVars[v]) {
				add("machines.%s.vars: недопустимая или встроенная переменная %q", name, v)
			}
		}
		for id := range mc.Folders {
			if !seen[id] {
				add("machines.%s.folders: папки %q нет в списке folders", name, id)
			}
		}
	}
	return errors.Join(errs...)
}
