package transport

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kiyoushii/EnvSync/internal/core"
)

// FolderSpec — что должно быть настроено в Syncthing для одной папки.
type FolderSpec struct {
	ID            string
	Path          string
	Ignores       []string
	KeepDays      int // срок хранения удалённого в «корзине»; 0 = 30
	WatcherDelayS int // 0 = не менять
	MaxConflicts  int // 0 = не менять
}

// FolderSpecFrom собирает спецификацию из раскрытой папки манифеста и настроек агента.
func FolderSpecFrom(f core.ResolvedFolder, s Settings) FolderSpec {
	return FolderSpec{
		ID:            f.ID,
		Path:          f.Path,
		Ignores:       f.Ignores,
		KeepDays:      f.KeepDays,
		WatcherDelayS: s.WatcherDelayS,
		MaxConflicts:  s.MaxConflicts,
	}
}

// ApplyFolder создаёт или приводит в порядок папку в Syncthing и открывает её для перечисленных
// устройств. Повторный вызов безопасен.
//
// Защита данных: если папка с таким ID уже есть, но смотрит на ДРУГОЙ путь, функция отказывается.
// Молча перенацелить папку нельзя: Syncthing увидел бы пустое место и мог бы разослать «удаление»
// всех файлов второй машине.
func ApplyFolder(ctx context.Context, c *Client, selfID string, peerIDs []string, f FolderSpec) error {
	if err := os.MkdirAll(f.Path, 0o700); err != nil {
		return fmt.Errorf("создание папки %s: %w", f.Path, err)
	}
	folder, err := c.Folder(ctx, f.ID)
	if err != nil {
		return fmt.Errorf("чтение папки %s: %w", f.ID, err)
	}
	if folder == nil {
		if folder, err = c.FolderDefaults(ctx); err != nil {
			return fmt.Errorf("шаблон папки: %w", err)
		}
		folder["id"] = f.ID
		folder["label"] = f.ID
		folder["path"] = f.Path
		folder["type"] = "sendreceive"
		folder["devices"] = []any{cfgObject{"deviceID": selfID}}
	} else if cur, _ := folder["path"].(string); !samePath(cur, f.Path) {
		return fmt.Errorf("папка %q в Syncthing уже настроена на путь %q, а манифест требует %q: "+
			"перенаправлять папку автоматически небезопасно, удалите её в Syncthing вручную или верните путь", f.ID, cur, f.Path)
	}

	for _, id := range peerIDs {
		if !folderHasDevice(folder, id) {
			devs, _ := folder["devices"].([]any)
			folder["devices"] = append(devs, cfgObject{"deviceID": id})
		}
	}

	keep := f.KeepDays
	if keep == 0 {
		keep = 30
	}
	folder["versioning"] = cfgObject{
		"type":   "trashcan",
		"params": cfgObject{"cleanoutDays": strconv.Itoa(keep)},
	}
	folder["fsWatcherEnabled"] = true
	if f.WatcherDelayS > 0 {
		folder["fsWatcherDelayS"] = f.WatcherDelayS
	}
	if f.MaxConflicts != 0 {
		folder["maxConflicts"] = f.MaxConflicts
	}

	if err := c.PutFolder(ctx, f.ID, folder); err != nil {
		return fmt.Errorf("настройка папки %s: %w", f.ID, err)
	}
	if len(f.Ignores) > 0 {
		if err := c.SetIgnores(ctx, f.ID, f.Ignores); err != nil {
			return fmt.Errorf("исключения папки %s: %w", f.ID, err)
		}
	}
	return nil
}

func folderHasDevice(folder cfgObject, id string) bool {
	devs, _ := folder["devices"].([]any)
	for _, d := range devs {
		if m, ok := d.(map[string]any); ok && m["deviceID"] == id {
			return true
		}
	}
	return false
}

// samePath сравнивает пути так, как это делает Windows: без учёта регистра и лишних разделителей.
func samePath(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}
