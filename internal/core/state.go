package core

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// StateSchema — версия формата файлов состояния.
const StateSchema = 1

// StateFile — декларативное состояние одного модуля на одной машине.
// Каждая машина пишет только свою папку <Root>/<машина>/, поэтому циклов «A → B → A» не бывает:
// чужие состояния машина только читает.
type StateFile struct {
	Schema    int             `json:"schema"`
	Machine   string          `json:"machine"`
	Module    string          `json:"module"`
	ChangedAt time.Time       `json:"changedAt"` // когда данные в последний раз изменились (а не когда их в последний раз снимали)
	Data      json.RawMessage `json:"data"`
}

// StateDir — папка состояний, которая синкается между машинами через Syncthing.
type StateDir struct {
	Root string
}

func (d StateDir) file(machine, module string) string {
	return filepath.Join(d.Root, machine, module+".json")
}

// AssetDir — папка для файлов модуля (например, картинка обоев) рядом с его состоянием.
func (d StateDir) AssetDir(machine, module string) string {
	return filepath.Join(d.Root, machine, "assets", module)
}

// canonical приводит JSON к стабильному виду (ключи по алфавиту), чтобы сравнивать данные, а не форматирование.
func canonical(data json.RawMessage) ([]byte, error) {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// Read читает состояние; nil, если файла нет.
func (d StateDir) Read(machine, module string) (*StateFile, error) {
	data, err := os.ReadFile(d.file(machine, module))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var sf StateFile
	if err := json.Unmarshal(data, &sf); err != nil {
		return nil, fmt.Errorf("%s: %w", d.file(machine, module), err)
	}
	if sf.Schema > StateSchema {
		return nil, fmt.Errorf("%s: схема состояния %d новее агента (%d)", d.file(machine, module), sf.Schema, StateSchema)
	}
	return &sf, nil
}

// Write записывает состояние. Если данные не изменились с прошлой записи, файл и ChangedAt остаются
// как были (changed=false): иначе каждый снимок состояния выглядел бы «новым изменением» и машины
// гоняли бы одно и то же друг другу.
func (d StateDir) Write(machine, module string, data json.RawMessage, now time.Time) (changed bool, err error) {
	newCanon, err := canonical(data)
	if err != nil {
		return false, fmt.Errorf("состояние %s: %w", module, err)
	}
	prev, err := d.Read(machine, module)
	if err != nil {
		return false, err
	}
	if prev != nil {
		if oldCanon, err := canonical(prev.Data); err == nil && bytes.Equal(oldCanon, newCanon) {
			return false, nil
		}
	}
	// Самый первый снимок — «база», а не правка: ChangedAt нулевой. Иначе машина, которая запустила агент позже,
	// выглядела бы «новее» и навязала бы свои настройки остальным. Первичное выравнивание делается явно (PlanFrom).
	changedAt := now.UTC()
	if prev == nil {
		changedAt = time.Time{}
	}
	sf := StateFile{Schema: StateSchema, Machine: machine, Module: module, ChangedAt: changedAt, Data: newCanon}
	out, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return false, err
	}
	return true, writeFileAtomic(d.file(machine, module), out)
}

// Touch делает текущее состояние модуля машины самым свежим (ChangedAt = now), не меняя данных.
// Это решение «оставить как здесь»: другие машины увидят его как последнюю правку и подтянутся к нему.
func (d StateDir) Touch(machine, module string, now time.Time) error {
	sf, err := d.Read(machine, module)
	if err != nil {
		return err
	}
	if sf == nil {
		return fmt.Errorf("состояния модуля %s на машине %s нет", module, machine)
	}
	sf.ChangedAt = now.UTC()
	out, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(d.file(machine, module), out)
}

// Machines возвращает имена машин, у которых есть состояние.
func (d StateDir) Machines() ([]string, error) {
	entries, err := os.ReadDir(d.Root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		// Папки, начинающиеся с точки, служебные (.stfolder, .stversions Syncthing), а не машины.
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}
