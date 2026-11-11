package core

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Journal — журнал аудита: что, когда, с какой машины и почему было изменено.
// Только добавление, по одной JSON-записи в строке; читается человеком через Entry.String().
type Journal struct {
	Path string
}

// Entry — одна запись журнала.
type Entry struct {
	Time     time.Time `json:"time"`
	Machine  string    `json:"machine"`
	Module   string    `json:"module"`
	Action   string    `json:"action"` // export, apply, rollback, skip, error
	Run      string    `json:"run,omitempty"`
	Source   string    `json:"source,omitempty"` // с какой машины пришло состояние
	Detail   string    `json:"detail,omitempty"`
	Changes  []string  `json:"changes,omitempty"`
	Snapshot string    `json:"snapshot,omitempty"`
}

// Append добавляет запись в конец журнала.
func (j *Journal) Append(e Entry) error {
	if err := os.MkdirAll(filepath.Dir(j.Path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(j.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	return err
}

// Recent возвращает последние n записей (старые первыми).
func (j *Journal) Recent(n int) ([]Entry, error) {
	f, err := os.Open(j.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var all []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var e Entry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			all = append(all, e)
		}
	}
	if n > 0 && len(all) > n {
		all = all[len(all)-n:]
	}
	return all, sc.Err()
}

// String — запись в виде строки для человека.
func (e Entry) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %-10s %-10s %s", e.Time.Local().Format("2006-01-02 15:04:05"), e.Module, e.Action, e.Machine)
	if e.Source != "" {
		fmt.Fprintf(&b, " ← %s", e.Source)
	}
	if e.Detail != "" {
		fmt.Fprintf(&b, ": %s", e.Detail)
	}
	for _, c := range e.Changes {
		fmt.Fprintf(&b, "\n    • %s", c)
	}
	if e.Run != "" {
		fmt.Fprintf(&b, "\n    запуск %s", e.Run)
	}
	return b.String()
}
