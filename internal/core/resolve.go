package core

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Env — описание машины, для которой раскрываются переменные. Всё, что зависит от конкретного
// компьютера (имя, пользователь, диски), приходит сюда, а не зашито в код. Благодаря этому
// один манифест можно проверить «как на другой машине» (команда manifest check --machine ...).
type Env struct {
	Machine      string // имя компьютера, по нему выбирается профиль
	User         string
	Home         string // профиль пользователя
	AppData      string
	LocalAppData string
}

// CurrentEnv собирает Env для этой машины из окружения Windows.
func CurrentEnv() (Env, error) {
	host, err := os.Hostname()
	if err != nil {
		return Env{}, fmt.Errorf("имя компьютера: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return Env{}, fmt.Errorf("профиль пользователя: %w", err)
	}
	user := os.Getenv("USERNAME")
	if user == "" {
		user = filepath.Base(home)
	}
	return Env{
		Machine:      strings.ToUpper(host),
		User:         user,
		Home:         home,
		AppData:      os.Getenv("APPDATA"),
		LocalAppData: os.Getenv("LOCALAPPDATA"),
	}, nil
}

// ResolvedFolder — папка с путём и настройками для конкретной машины.
type ResolvedFolder struct {
	ID       string
	Path     string
	Ignores  []string
	KeepDays int
	Test     bool
}

var placeholderRe = regexp.MustCompile(`\{\{\s*([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)

// expand подставляет {{переменные}}. Переменные могут ссылаться друг на друга,
// циклы и неизвестные имена — ошибка.
func expand(s string, raw map[string]string, resolved map[string]string, stack []string) (string, error) {
	var firstErr error
	out := placeholderRe.ReplaceAllStringFunc(s, func(m string) string {
		name := strings.ToLower(placeholderRe.FindStringSubmatch(m)[1])
		if v, ok := resolved[name]; ok {
			return v
		}
		def, ok := raw[name]
		if !ok {
			if firstErr == nil {
				firstErr = fmt.Errorf("неизвестная переменная {{%s}}", name)
			}
			return m
		}
		for _, s := range stack {
			if s == name {
				if firstErr == nil {
					firstErr = fmt.Errorf("переменные ссылаются друг на друга по кругу: %s → %s", strings.Join(stack, " → "), name)
				}
				return m
			}
		}
		v, err := expand(def, raw, resolved, append(stack, name))
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			return m
		}
		resolved[name] = v
		return v
	})
	return out, firstErr
}

// machineProfile ищет профиль по имени компьютера без учёта регистра.
func (m *Manifest) machineProfile(name string) (Machine, bool) {
	for k, v := range m.Machines {
		if strings.EqualFold(k, name) {
			return v, true
		}
	}
	return Machine{}, false
}

// ResolveVars возвращает все переменные для машины: встроенные, затем манифеста, затем профиля машины.
// sync_root по умолчанию равен {{home}}.
func (m *Manifest) ResolveVars(env Env) (map[string]string, error) {
	raw := map[string]string{
		"user":         env.User,
		"home":         env.Home,
		"appdata":      env.AppData,
		"localappdata": env.LocalAppData,
		"machine":      env.Machine,
		"sync_root":    "{{home}}",
	}
	for k, v := range m.Vars {
		raw[k] = v
	}
	if mc, ok := m.machineProfile(env.Machine); ok {
		for k, v := range mc.Vars {
			raw[k] = v
		}
	}
	resolved := map[string]string{}
	var errs []error
	names := make([]string, 0, len(raw))
	for k := range raw {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		if _, done := resolved[k]; done {
			continue
		}
		v, err := expand("{{"+k+"}}", raw, resolved, nil)
		if err != nil {
			errs = append(errs, fmt.Errorf("переменная %s: %w", k, err))
			continue
		}
		resolved[k] = v
	}
	return resolved, errors.Join(errs...)
}

// Resolve раскрывает манифест для машины: пути, исключения, пропуск папок.
// Результат проверяется: пути абсолютные, не корень диска, не системные папки, не вложены друг в друга.
func (m *Manifest) Resolve(env Env) ([]ResolvedFolder, error) {
	vars, err := m.ResolveVars(env)
	if err != nil {
		return nil, err
	}
	mc, _ := m.machineProfile(env.Machine)

	base := m.Ignore
	if len(base) == 0 {
		base = DefaultIgnores
	}

	var out []ResolvedFolder
	var errs []error
	for _, f := range m.Folders {
		raw := f.Path
		if ov, ok := mc.Folders[f.ID]; ok {
			if ov.Skip {
				continue
			}
			if ov.Path != "" {
				raw = ov.Path
			}
		}
		p, err := expand(raw, nil, vars, nil)
		if err != nil {
			errs = append(errs, fmt.Errorf("папка %s: %w", f.ID, err))
			continue
		}
		p = filepath.Clean(p)
		if err := checkFolderPath(p); err != nil {
			errs = append(errs, fmt.Errorf("папка %s: %w", f.ID, err))
			continue
		}
		ign := append(append([]string{}, base...), f.Ignore...)
		keep := f.KeepDays
		if keep == 0 {
			keep = 30
		}
		out = append(out, ResolvedFolder{ID: f.ID, Path: p, Ignores: ign, KeepDays: keep, Test: f.Test})
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	if err := checkOverlaps(out); err != nil {
		return nil, err
	}
	return out, nil
}

// checkFolderPath защищает от опасных путей: относительных, корня диска, системных папок Windows.
func checkFolderPath(p string) error {
	if !filepath.IsAbs(p) {
		return fmt.Errorf("путь %q не абсолютный (после подстановки переменных)", p)
	}
	if vol := filepath.VolumeName(p); strings.EqualFold(p, vol+`\`) || strings.EqualFold(p, vol) {
		return fmt.Errorf("путь %q — корень диска, синкать его целиком нельзя", p)
	}
	for _, bad := range []string{`\windows`, `\program files`, `\program files (x86)`, `\programdata`} {
		rest := strings.ToLower(strings.TrimPrefix(p, filepath.VolumeName(p)))
		if rest == bad || strings.HasPrefix(rest, bad+`\`) {
			return fmt.Errorf("путь %q лежит в системной папке Windows", p)
		}
	}
	return nil
}

// checkOverlaps запрещает вложенные папки: Syncthing не любит папку внутри другой папки,
// а данные при этом делятся неоднозначно.
func checkOverlaps(fs []ResolvedFolder) error {
	var errs []error
	for i := range fs {
		for j := i + 1; j < len(fs); j++ {
			a, b := strings.ToLower(fs[i].Path), strings.ToLower(fs[j].Path)
			if a == b || strings.HasPrefix(a, b+`\`) || strings.HasPrefix(b, a+`\`) {
				errs = append(errs, fmt.Errorf("папки %s (%s) и %s (%s) пересекаются", fs[i].ID, fs[i].Path, fs[j].ID, fs[j].Path))
			}
		}
	}
	return errors.Join(errs...)
}

// Describe — текст для человека: как манифест раскрылся на этой машине.
func Describe(env Env, folders []ResolvedFolder) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Машина %s, пользователь %s, профиль %s\n", env.Machine, env.User, env.Home)
	if len(folders) == 0 {
		b.WriteString("  папок для синхронизации нет\n")
	}
	for _, f := range folders {
		test := ""
		if f.Test {
			test = " [тестовая]"
		}
		fmt.Fprintf(&b, "  %s%s\n    путь: %s\n    исключений: %d, корзина: %d дн.\n", f.ID, test, f.Path, CountPatterns(f.Ignores), f.KeepDays)
	}
	return b.String()
}
