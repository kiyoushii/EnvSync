// Пакет install — автозапуск агента задачей планировщика Windows при входе пользователя.
//
// Почему задача планировщика, а не служба (решение этапа 0): HKCU, обои и тема живут в пользовательской сессии,
// агент должен работать от имени пользователя, а не SYSTEM. Права администратора не нужны.
package install

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"unicode/utf16"
)

// DefaultTaskName — имя задачи.
const DefaultTaskName = "EnvSync"

// Task описывает задачу «запускать агента при входе пользователя».
type Task struct {
	Name string
	Exe  string // полный путь к envsync.exe
	Args string // например "run --background"
	User string // DOMAIN\user; пусто = текущий пользователь
}

func esc(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// XML возвращает описание задачи для планировщика.
//
// Запуск при входе только этого пользователя, с обычными правами (не повышенными), без ограничения по времени,
// перезапуск при сбое каждую минуту, запуск и на батарее, второй экземпляр не создаётся.
func (t Task) XML() string {
	user := esc(t.User)
	return `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>EnvSync: синхронизация среды Windows между машинами</Description>
  </RegistrationInfo>
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
      <UserId>` + user + `</UserId>
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>` + user + `</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>true</StartWhenAvailable>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>false</Hidden>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <RestartOnFailure>
      <Interval>PT1M</Interval>
      <Count>999</Count>
    </RestartOnFailure>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>` + esc(t.Exe) + `</Command>
      <Arguments>` + esc(t.Args) + `</Arguments>
    </Exec>
  </Actions>
</Task>
`
}

// CurrentUser возвращает DOMAIN\user текущего пользователя.
func CurrentUser() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	return u.Username, nil
}

// utf16File кодирует XML так, как требует планировщик: UTF-16 LE с BOM.
func utf16File(s string) []byte {
	units := utf16.Encode([]rune(s))
	out := make([]byte, 0, 2+len(units)*2)
	out = append(out, 0xFF, 0xFE)
	for _, u := range units {
		out = append(out, byte(u), byte(u>>8))
	}
	return out
}

func schtasks(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "schtasks", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// Create создаёт или заменяет задачу.
func (t Task) Create(ctx context.Context) error {
	if t.Name == "" {
		t.Name = DefaultTaskName
	}
	if t.User == "" {
		u, err := CurrentUser()
		if err != nil {
			return err
		}
		t.User = u
	}
	if !filepath.IsAbs(t.Exe) {
		return fmt.Errorf("нужен полный путь к программе, а не %q", t.Exe)
	}
	f, err := os.CreateTemp("", "envsync-task-*.xml")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(utf16File(t.XML())); err != nil {
		f.Close()
		return err
	}
	f.Close()
	if out, err := schtasks(ctx, "/Create", "/TN", t.Name, "/XML", f.Name(), "/F"); err != nil {
		return fmt.Errorf("schtasks /Create: %w: %s", err, out)
	}
	return nil
}

// Exists проверяет, есть ли задача.
func Exists(ctx context.Context, name string) (bool, error) {
	out, err := schtasks(ctx, "/Query", "/TN", name)
	if err == nil {
		return true, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return false, nil // код 1: задачи нет
	}
	return false, fmt.Errorf("schtasks /Query: %w: %s", err, out)
}

// Delete удаляет задачу; отсутствие задачи не ошибка.
func Delete(ctx context.Context, name string) error {
	if ok, err := Exists(ctx, name); err != nil || !ok {
		return err
	}
	if out, err := schtasks(ctx, "/Delete", "/TN", name, "/F"); err != nil {
		return fmt.Errorf("schtasks /Delete: %w: %s", err, out)
	}
	return nil
}
