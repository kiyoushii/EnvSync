package core

import "testing"

func TestSoftwareSectionRequiresSchema2AndIsValidated(t *testing.T) {
	if _, err := ParseManifest([]byte("schema: 1\nsoftware:\n  winget: [{id: Git.Git}]")); err == nil {
		t.Error("раздел software в манифесте схемы 1 должен быть отклонён")
	}
	bad := map[string]string{
		"плохой id":           "schema: 2\nsoftware:\n  winget: [{id: 'Git Git'}]",
		"повтор":              "schema: 2\nsoftware:\n  winget: [{id: Git.Git}, {id: git.git}]",
		"плохой scope":        "schema: 2\nsoftware:\n  winget: [{id: Git.Git, scope: root}]",
		"опасная версия":      "schema: 2\nsoftware:\n  winget: [{id: Git.Git, version: '1.0 & calc'}]",
		"и ставим и удаляем":  "schema: 2\nsoftware:\n  winget: [{id: Git.Git}]\n  remove: [Git.Git]",
		"плохой id в remove":  "schema: 2\nsoftware:\n  remove: ['a;b']",
		"manual без имени":    "schema: 2\nsoftware:\n  manual: [{note: x}]",
		"плохой skip":         "schema: 2\nmachines:\n  PC:\n    software:\n      skip: ['x y']",
		"неизвестное в софте": "schema: 2\nsoftware:\n  wingget: []",
	}
	for name, src := range bad {
		if _, err := ParseManifest([]byte(src)); err == nil {
			t.Errorf("%s: манифест должен быть отклонён", name)
		}
	}
	// Манифест схемы 1 без software по-прежнему корректен.
	if _, err := ParseManifest([]byte("schema: 1\nfolders:\n  - {id: a, path: 'C:\\A'}")); err != nil {
		t.Errorf("схема 1 должна оставаться допустимой: %v", err)
	}
}

func TestResolveSoftwareAppliesMachineSkip(t *testing.T) {
	m := mustParse(t, `
schema: 2
software:
  winget:
    - {id: Git.Git}
    - {id: Valve.Steam}
    - {id: GoLang.Go, version: "1.27.4", scope: user}
  remove: [Old.App, Skipped.Old]
  manual:
    - {name: Photoshop, note: "вручную"}
machines:
  LAPTOP:
    software:
      skip: [valve.steam, skipped.old]
`)
	pc := m.ResolveSoftware(Env{Machine: "PC"})
	if len(pc.Install) != 3 || len(pc.Remove) != 2 || len(pc.Manual) != 1 {
		t.Fatalf("pc = %+v", pc)
	}
	laptop := m.ResolveSoftware(Env{Machine: "laptop"})
	if len(laptop.Install) != 2 || laptop.Install[0].ID != "Git.Git" || laptop.Install[1].Version != "1.27.4" {
		t.Fatalf("laptop.Install = %+v", laptop.Install)
	}
	if len(laptop.Remove) != 1 || laptop.Remove[0] != "Old.App" {
		t.Fatalf("laptop.Remove = %+v", laptop.Remove)
	}
	if (&Manifest{Schema: 1}).ResolveSoftware(Env{}).Install != nil {
		t.Error("без раздела software набор пуст")
	}
}
