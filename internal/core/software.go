package core

import "strings"

// SoftwareSet — программы для конкретной машины после применения её профиля.
type SoftwareSet struct {
	Install []WingetPkg  // должны быть установлены
	Remove  []string     // явно помеченные на удаление (только с подтверждением)
	Manual  []ManualItem // ставятся вручную
}

// ResolveSoftware применяет профиль машины (пакеты из skip не ставятся и не удаляются на ней).
func (m *Manifest) ResolveSoftware(env Env) SoftwareSet {
	var set SoftwareSet
	if m.Software == nil {
		return set
	}
	skip := map[string]bool{}
	if mc, ok := m.machineProfile(env.Machine); ok {
		for _, id := range mc.Software.Skip {
			skip[strings.ToLower(id)] = true
		}
	}
	for _, p := range m.Software.Winget {
		if !skip[strings.ToLower(p.ID)] {
			set.Install = append(set.Install, p)
		}
	}
	for _, id := range m.Software.Remove {
		if !skip[strings.ToLower(id)] {
			set.Remove = append(set.Remove, id)
		}
	}
	set.Manual = append(set.Manual, m.Software.Manual...)
	return set
}
