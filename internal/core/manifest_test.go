package core

import (
	"os"
	"strings"
	"testing"
)

const goodManifest = `
schema: 1
vars:
  work: "{{sync_root}}\\Work"
folders:
  - id: projects
    path: "{{work}}\\Projects"
    keep_days: 60
    ignore: ["(?i)*.log"]
  - id: docs
    path: "{{home}}\\Documents"
  - id: envsync-test
    path: "{{sync_root}}\\EnvSyncTest"
    test: true
machines:
  DESKTOP-PC:
    vars:
      sync_root: "D:\\Sync"
  laptop:
    vars:
      sync_root: "C:\\Users\\{{user}}\\Sync"
    folders:
      docs:
        skip: true
      projects:
        path: "E:\\Other\\Projects"
`

func mustParse(t *testing.T, s string) *Manifest {
	t.Helper()
	m, err := ParseManifest([]byte(s))
	if err != nil {
		t.Fatalf("манифест должен быть корректным: %v", err)
	}
	return m
}

func byID(fs []ResolvedFolder) map[string]ResolvedFolder {
	out := map[string]ResolvedFolder{}
	for _, f := range fs {
		out[f.ID] = f
	}
	return out
}

// Критерий этапа 3: один манифест корректно работает на машинах с разными буквами дисков.
func TestOneManifestTwoMachinesDifferentDrives(t *testing.T) {
	m := mustParse(t, goodManifest)

	pc := Env{Machine: "DESKTOP-PC", User: "kiyoushii", Home: `C:\Users\kiyoushii`, AppData: `C:\Users\kiyoushii\AppData\Roaming`}
	laptop := Env{Machine: "LAPTOP", User: "kiyoushii", Home: `F:\Users\kiyoushii`, AppData: `F:\Users\kiyoushii\AppData\Roaming`}

	a, err := m.Resolve(pc)
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.Resolve(laptop)
	if err != nil {
		t.Fatal(err)
	}
	pa, pb := byID(a), byID(b)

	// ПК: sync_root на D:, ноутбук: sync_root внутри профиля на F:, один и тот же манифест.
	want := map[string]string{
		"projects":     `D:\Sync\Work\Projects`,
		"docs":         `C:\Users\kiyoushii\Documents`,
		"envsync-test": `D:\Sync\EnvSyncTest`,
	}
	for id, path := range want {
		if pa[id].Path != path {
			t.Errorf("ПК %s: %q, ожидалось %q", id, pa[id].Path, path)
		}
	}
	// Ноутбук: docs пропущен, projects лежит по особому пути, остальное через переменные.
	if _, has := pb["docs"]; has {
		t.Error("docs на ноутбуке должен быть пропущен")
	}
	if pb["projects"].Path != `E:\Other\Projects` {
		t.Errorf("ноутбук projects: %q", pb["projects"].Path)
	}
	if pb["envsync-test"].Path != `C:\Users\kiyoushii\Sync\EnvSyncTest` {
		t.Errorf("ноутбук envsync-test: %q", pb["envsync-test"].Path)
	}
	// Идентификаторы совпадают там, где папка синкается на обеих: именно они связывают машины.
	for _, id := range []string{"projects", "envsync-test"} {
		if _, ok := pa[id]; !ok {
			t.Errorf("папки %s нет на ПК", id)
		}
		if _, ok := pb[id]; !ok {
			t.Errorf("папки %s нет на ноутбуке", id)
		}
	}
}

func TestVariablesAreResolvedChainedAndCaseInsensitive(t *testing.T) {
	m := mustParse(t, `
schema: 1
vars:
  a: "{{b}}\\a"
  b: "{{ HOME }}\\b"
folders:
  - id: x
    path: "{{A}}\\x"
`)
	fs, err := m.Resolve(Env{Machine: "M", User: "u", Home: `C:\Users\u`})
	if err != nil {
		t.Fatal(err)
	}
	if fs[0].Path != `C:\Users\u\b\a\x` {
		t.Fatalf("path = %q", fs[0].Path)
	}
}

func TestMachineProfileMatchesNameCaseInsensitively(t *testing.T) {
	m := mustParse(t, goodManifest)
	fs, err := m.Resolve(Env{Machine: "desktop-pc", User: "u", Home: `C:\Users\u`})
	if err != nil {
		t.Fatal(err)
	}
	if byID(fs)["projects"].Path != `D:\Sync\Work\Projects` {
		t.Fatalf("профиль не подхватился: %+v", fs)
	}
}

func TestUnknownMachineGetsDefaults(t *testing.T) {
	m := mustParse(t, goodManifest)
	fs, err := m.Resolve(Env{Machine: "SOMETHING-ELSE", User: "u", Home: `C:\Users\u`})
	if err != nil {
		t.Fatal(err)
	}
	if byID(fs)["envsync-test"].Path != `C:\Users\u\EnvSyncTest` { // sync_root по умолчанию = home
		t.Fatalf("path = %q", byID(fs)["envsync-test"].Path)
	}
}

func TestIgnoresAndKeepDays(t *testing.T) {
	m := mustParse(t, goodManifest)
	fs, _ := m.Resolve(Env{Machine: "DESKTOP-PC", User: "u", Home: `C:\Users\u`})
	p, d := byID(fs)["projects"], byID(fs)["docs"]

	if p.KeepDays != 60 || d.KeepDays != 30 {
		t.Errorf("keep_days: projects=%d docs=%d", p.KeepDays, d.KeepDays)
	}
	join := strings.Join(p.Ignores, "\n")
	if !strings.Contains(join, "(?i).git") || !strings.Contains(join, "(?i)*.log") {
		t.Errorf("у projects должны быть исключения по умолчанию и свои: %v", p.Ignores)
	}
	if strings.Contains(strings.Join(d.Ignores, "\n"), "*.log") {
		t.Error("исключение projects попало в docs")
	}

	// Свой общий список заменяет список по умолчанию целиком.
	m2 := mustParse(t, "schema: 1\nignore: [\"(?i)only-this\"]\nfolders:\n  - id: a\n    path: 'C:\\A'\n")
	fs2, _ := m2.Resolve(Env{Machine: "M", User: "u", Home: `C:\Users\u`})
	if len(fs2[0].Ignores) != 1 || fs2[0].Ignores[0] != "(?i)only-this" {
		t.Errorf("ignores = %v", fs2[0].Ignores)
	}
}

func TestParseRejectsBadManifests(t *testing.T) {
	cases := map[string]string{
		"пустой":                  "",
		"нет схемы":               "folders: []",
		"схема новее агента":      "schema: 99",
		"неизвестное поле":        "schema: 1\nfoldres: []",
		"опечатка во вложенном":   "schema: 1\nfolders:\n  - id: a\n    pth: 'C:\\A'",
		"повтор id":               "schema: 1\nfolders:\n  - {id: a, path: 'C:\\A'}\n  - {id: a, path: 'C:\\B'}",
		"плохой id":               "schema: 1\nfolders:\n  - {id: 'My Folder', path: 'C:\\A'}",
		"нет path":                "schema: 1\nfolders:\n  - {id: a}",
		"отрицательные keep_days": "schema: 1\nfolders:\n  - {id: a, path: 'C:\\A', keep_days: -1}",
		"встроенная переменная":   "schema: 1\nvars: {home: 'D:\\x'}",
		"плохое имя переменной":   "schema: 1\nvars: {Bad-Name: 'x'}",
		"профиль на чужую папку":  "schema: 1\nfolders:\n  - {id: a, path: 'C:\\A'}\nmachines:\n  PC:\n    folders:\n      zzz: {skip: true}",
		"имена машин по регистру": "schema: 1\nmachines:\n  PC: {}\n  pc: {}",
		"пустой шаблон":           "schema: 1\nignore: ['  ']",
	}
	for name, src := range cases {
		if _, err := ParseManifest([]byte(src)); err == nil {
			t.Errorf("%s: манифест должен быть отклонён", name)
		}
	}
}

func TestResolveRejectsDangerousOrBrokenPaths(t *testing.T) {
	env := Env{Machine: "M", User: "u", Home: `C:\Users\u`}
	cases := map[string]string{
		"неизвестная переменная": "schema: 1\nfolders:\n  - {id: a, path: '{{nope}}\\x'}",
		"цикл переменных":        "schema: 1\nvars: {a: '{{b}}', b: '{{a}}'}\nfolders:\n  - {id: x, path: 'C:\\x'}",
		"относительный путь":     "schema: 1\nfolders:\n  - {id: a, path: 'Documents\\x'}",
		"корень диска":           "schema: 1\nfolders:\n  - {id: a, path: 'D:\\'}",
		"корень диска без слэша": "schema: 1\nfolders:\n  - {id: a, path: 'D:'}",
		"папка Windows":          "schema: 1\nfolders:\n  - {id: a, path: 'C:\\Windows\\System32'}",
		"Program Files":          "schema: 1\nfolders:\n  - {id: a, path: 'C:\\Program Files\\App'}",
		"вложенные папки":        "schema: 1\nfolders:\n  - {id: a, path: 'C:\\Data'}\n  - {id: b, path: 'C:\\data\\Sub'}",
		"одинаковые пути":        "schema: 1\nfolders:\n  - {id: a, path: 'C:\\Data'}\n  - {id: b, path: 'c:\\DATA\\'}",
	}
	for name, src := range cases {
		m, err := ParseManifest([]byte(src))
		if err != nil {
			t.Errorf("%s: манифест разобрался с ошибкой раньше времени: %v", name, err)
			continue
		}
		if _, err := m.Resolve(env); err == nil {
			t.Errorf("%s: раскрытие должно быть отклонено", name)
		}
	}
}

func TestSiblingFoldersWithCommonPrefixAreNotOverlapping(t *testing.T) {
	m := mustParse(t, "schema: 1\nfolders:\n  - {id: a, path: 'C:\\Data'}\n  - {id: b, path: 'C:\\Data2'}")
	if _, err := m.Resolve(Env{Machine: "M", User: "u", Home: `C:\Users\u`}); err != nil {
		t.Fatalf("C:\\Data и C:\\Data2 не пересекаются: %v", err)
	}
}

// Файл-пример из репозитория должен оставаться корректным манифестом.
func TestExampleManifestIsValid(t *testing.T) {
	data, err := os.ReadFile("../../envsync.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	m, err := ParseManifest(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, mc := range []string{"DESKTOP-EXAMPLE", "LAPTOP-EXAMPLE", "OTHER"} {
		if _, err := m.Resolve(Env{Machine: mc, User: "me", Home: `C:\Users\me`}); err != nil {
			t.Errorf("пример не раскрывается для %s: %v", mc, err)
		}
	}
}
