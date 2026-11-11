package core

import "testing"

func TestMassDeleteThresholds(t *testing.T) {
	d := (&Manifest{}).MassDelete() // значения по умолчанию: 1000 файлов, 5 %, минимум 10
	if d.MaxFiles != 1000 || d.MaxPercent != 5 || d.MinFiles != 10 {
		t.Fatalf("defaults = %+v", d)
	}
	cases := []struct {
		need, global int64
		want         bool
		why          string
	}{
		{0, 1000, false, "ничего не удаляется"},
		{3, 5, false, "папка из 5 файлов: меньше минимума, хотя 60 %"},
		{9, 10, false, "всё ещё меньше минимума"},
		{10, 100000, false, "10 из 100000: меньше процента"},
		{10, 100, true, "10 % и не меньше минимума"},
		{12, 300, false, "4 % ниже порога"},
		{16, 300, true, "5,3 % выше порога"},
		{1001, 100000, true, "больше 1000 файлов, даже если это 1 %"},
		{1000, 100000, false, "ровно 1000 при малой доле"},
		{50, 0, false, "неизвестный размер папки: по проценту не судим, по числу ниже порога"},
	}
	for _, c := range cases {
		if got := d.Exceeded(c.need, c.global); got != c.want {
			t.Errorf("Exceeded(%d из %d) = %v, ожидалось %v: %s", c.need, c.global, got, c.want, c.why)
		}
	}
}

func TestSafetySectionFromManifest(t *testing.T) {
	m := mustParse(t, "schema: 2\nsafety:\n  mass_delete: {max_files: 500, max_percent: 2.5, min_files: 4}")
	d := m.MassDelete()
	if d.MaxFiles != 500 || d.MaxPercent != 2.5 || d.MinFiles != 4 {
		t.Fatalf("%+v", d)
	}
	if !d.Exceeded(5, 100) || d.Exceeded(3, 10) {
		t.Error("пороги из манифеста не применяются")
	}
	for name, src := range map[string]string{
		"схема 1":            "schema: 1\nsafety:\n  mass_delete: {max_files: 5}",
		"отрицательное":      "schema: 2\nsafety:\n  mass_delete: {max_files: -1}",
		"процент больше 100": "schema: 2\nsafety:\n  mass_delete: {max_percent: 120}",
		"опечатка":           "schema: 2\nsafety:\n  mass_delet: {}",
	} {
		if _, err := ParseManifest([]byte(src)); err == nil {
			t.Errorf("%s: манифест должен быть отклонён", name)
		}
	}
}
