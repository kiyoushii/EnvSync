package core

import (
	"strings"
	"testing"
)

func TestNotSyncedStaticCoversTheAgreedList(t *testing.T) {
	var all strings.Builder
	for _, s := range NotSyncedStatic() {
		for _, it := range s.Items {
			if it.What == "" || it.Why == "" || it.Workaround == "" {
				t.Errorf("пустое поле в %+v", it)
			}
			all.WriteString(it.What + "\n")
		}
	}
	for _, want := range []string{"Драйверы", "Wi-Fi", "Лицензия", "Program Files", "Игры", "Кэши"} {
		if !strings.Contains(all.String(), want) {
			t.Errorf("в списке «не синхронизируется» нет %q (раздел 6 плана)", want)
		}
	}
}

func TestChecklistIsReadyMarkdown(t *testing.T) {
	out := Checklist(NotSyncedStatic(), []ManualItem{{Name: "Photoshop", Note: "с сайта производителя"}})
	for _, want := range []string{"# Чек-лист", "- [ ] Драйверы", "## Программы, которые ставятся вручную", "- [ ] Photoshop с сайта производителя"} {
		if !strings.Contains(out, want) {
			t.Errorf("в чек-листе нет %q", want)
		}
	}
	if strings.Contains(out, "..\n") {
		t.Error("пункты не должны заканчиваться двойной точкой")
	}
}
