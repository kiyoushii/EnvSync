package install

import (
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

func TestTaskXMLIsWellFormedAndSafe(t *testing.T) {
	task := Task{Name: "EnvSync", Exe: `C:\Users\Иван & Co\AppData\Local\EnvSync\bin\envsync.exe`, Args: "run --background", User: `DESKTOP\Иван <test>`}
	x := task.XML()

	var v struct {
		XMLName xml.Name `xml:"Task"`
		Trigger struct {
			UserID string `xml:"UserId"`
		} `xml:"Triggers>LogonTrigger"`
		RunLevel string `xml:"Principals>Principal>RunLevel"`
		Command  string `xml:"Actions>Exec>Command"`
		Args     string `xml:"Actions>Exec>Arguments"`
		Limit    string `xml:"Settings>ExecutionTimeLimit"`
		Multi    string `xml:"Settings>MultipleInstancesPolicy"`
	}
	// Декларация говорит UTF-16, а мы разбираем строку Go: заменим её для проверки структуры.
	body := strings.Replace(x, `encoding="UTF-16"`, `encoding="UTF-8"`, 1)
	if err := xml.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("XML задачи некорректен: %v\n%s", err, x)
	}
	if v.Command != task.Exe || v.Args != "run --background" || v.Trigger.UserID != task.User {
		t.Errorf("значения не сохранились (спецсимволы должны экранироваться): %+v", v)
	}
	if v.RunLevel != "LeastPrivilege" {
		t.Errorf("задача не должна требовать повышенных прав: %q", v.RunLevel)
	}
	if v.Limit != "PT0S" || v.Multi != "IgnoreNew" {
		t.Errorf("limit=%q multi=%q", v.Limit, v.Multi)
	}
	if strings.Contains(x, "HighestAvailable") || strings.Contains(x, "SYSTEM") {
		t.Error("задача не должна запускаться от SYSTEM или с повышенными правами")
	}
}

func TestUTF16FileHasBOMAndRoundTrips(t *testing.T) {
	in := "<?xml?> Привет — мир"
	b := utf16File(in)
	if b[0] != 0xFF || b[1] != 0xFE {
		t.Fatal("нет BOM UTF-16 LE")
	}
	units := make([]uint16, 0, (len(b)-2)/2)
	for i := 2; i+1 < len(b); i += 2 {
		units = append(units, uint16(b[i])|uint16(b[i+1])<<8)
	}
	if got := string(utf16.Decode(units)); got != in {
		t.Fatalf("got %q", got)
	}
}

func TestCreateRequiresAbsoluteExe(t *testing.T) {
	if err := (Task{Name: "x", Exe: "envsync.exe", User: "u"}).Create(context.Background()); err == nil {
		t.Fatal("относительный путь должен быть отклонён")
	}
}

// Настоящий планировщик: создаём безвредную задачу с уникальным именем и сразу удаляем. Запуск явный (ENVSYNC_IT=1).
func TestRealSchedulerCreateQueryDelete(t *testing.T) {
	if os.Getenv("ENVSYNC_IT") != "1" {
		t.Skip("интеграционный тест: задайте ENVSYNC_IT=1")
	}
	ctx := context.Background()
	name := fmt.Sprintf("EnvSyncTest-%d", time.Now().UnixNano())
	task := Task{Name: name, Exe: `C:\Windows\System32\cmd.exe`, Args: "/c exit 0"}
	if err := task.Create(ctx); err != nil {
		t.Skip("планировщик недоступен в этой среде:", err)
	}
	t.Cleanup(func() { _ = Delete(ctx, name) })

	if ok, err := Exists(ctx, name); err != nil || !ok {
		t.Fatalf("задача должна существовать: ok=%v err=%v", ok, err)
	}
	if err := task.Create(ctx); err != nil { // повторное создание заменяет, а не падает
		t.Fatalf("повторное создание: %v", err)
	}
	if err := Delete(ctx, name); err != nil {
		t.Fatal(err)
	}
	if ok, _ := Exists(ctx, name); ok {
		t.Fatal("задача не удалена")
	}
	if err := Delete(ctx, name); err != nil {
		t.Fatalf("удаление несуществующей задачи не должно быть ошибкой: %v", err)
	}
}
