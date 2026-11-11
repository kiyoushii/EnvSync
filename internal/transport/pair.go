package transport

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// Device ID Syncthing: 8 групп по 7 символов (A–Z и 2–7), разделённых дефисами.
var deviceIDRe = regexp.MustCompile(`^([A-Z2-7]{7}-){7}[A-Z2-7]{7}$`)

// NormalizeDeviceID приводит ID к верхнему регистру и проверяет формат.
func NormalizeDeviceID(s string) (string, error) {
	id := strings.ToUpper(strings.TrimSpace(s))
	if !deviceIDRe.MatchString(id) {
		return "", fmt.Errorf("%q не похож на Device ID Syncthing (8 групп по 7 символов через дефис)", s)
	}
	return id, nil
}

// PairPlan — что именно сделает `envsync pair`. Строится без побочных эффектов,
// показывается человеку (dry-run), и только после подтверждения применяется.
type PairPlan struct {
	PeerName   string // имя машины в tailnet
	PeerID     string // Device ID Syncthing соседа
	Address    string // tcp://<tailscale-ip>:порт, единственный адрес, по которому к нему ходим
	FolderID   string
	FolderPath string

	// Параметры папки (этап 2). Нулевые значения означают «оставить как есть».
	Ignores       []string // шаблоны исключений (.stignore)
	WatcherDelayS int      // задержка отклика на изменения файлов
	MaxConflicts  int      // сколько конфликтных копий хранить
}

// BuildPairPlan проверяет, что сосед действительно в нашей tailnet, и собирает план.
// Устройство вне tailnet не добавляется никогда.
func BuildPairPlan(s Settings, ts *TSStatus, peerName, peerID, selfID string) (*PairPlan, error) {
	if !ts.Running() {
		return nil, fmt.Errorf("Tailscale не подключён (состояние %q): сначала войдите в сеть", ts.BackendState)
	}
	id, err := NormalizeDeviceID(peerID)
	if err != nil {
		return nil, err
	}
	if id == selfID {
		return nil, errors.New("это Device ID самой этой машины, а нужен ID второй")
	}
	peer, ok := ts.FindPeer(peerName)
	if !ok {
		return nil, fmt.Errorf("машина %q не найдена в tailnet; есть: %s", peerName, strings.Join(ts.PeerNames(), ", "))
	}
	if !peer.IPv4.IsValid() {
		return nil, fmt.Errorf("у машины %q нет адреса Tailscale", peerName)
	}
	return &PairPlan{
		PeerName:   peer.Name,
		PeerID:     id,
		Address:    "tcp://" + netip.AddrPortFrom(peer.IPv4, uint16(s.SyncPort)).String(),
		FolderID:   s.TestFolderID,
		FolderPath: s.TestFolderPath,

		Ignores:       s.IgnoresOrDefault(),
		WatcherDelayS: s.WatcherDelayS,
		MaxConflicts:  s.MaxConflicts,
	}, nil
}

// Describe — текст dry-run для человека.
func (p *PairPlan) Describe() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Будет сделано в Syncthing этой машины:\n")
	fmt.Fprintf(&b, "  1. Добавить устройство %s (%s)\n", p.PeerName, p.PeerID)
	fmt.Fprintf(&b, "     единственный адрес: %s; автопринятие папок и режим introducer выключены\n", p.Address)
	fmt.Fprintf(&b, "  2. Создать (если нет) тестовую папку %q: %s\n", p.FolderID, p.FolderPath)
	fmt.Fprintf(&b, "     версионирование: корзина, 30 дней; общий доступ только для этого устройства\n")
	if p.WatcherDelayS > 0 || p.MaxConflicts != 0 {
		fmt.Fprintf(&b, "     отклик на изменения: %d с; конфликтных копий на файл: до %d\n", p.WatcherDelayS, p.MaxConflicts)
	}
	if n := countPatterns(p.Ignores); n > 0 {
		fmt.Fprintf(&b, "  3. Задать исключения папки (%d шаблонов): .git, node_modules, кэши, незавершённые загрузки и т. п.\n", n)
	}
	fmt.Fprintf(&b, "Ничего другого не затрагивается.\n")
	return b.String()
}

// ApplyPair применяет план через REST API запущенного Syncthing.
// Повторный запуск безопасен: устройство и папка просто приводятся к тому же состоянию.
func ApplyPair(ctx context.Context, c *Client, selfID string, p *PairPlan) error {
	// --- устройство ---
	dev, err := c.DeviceDefaults(ctx)
	if err != nil {
		return fmt.Errorf("шаблон устройства: %w", err)
	}
	dev["deviceID"] = p.PeerID
	dev["name"] = p.PeerName
	dev["addresses"] = []string{p.Address} // без "dynamic": обнаружение выключено
	dev["autoAcceptFolders"] = false
	dev["introducer"] = false
	if err := c.PutDevice(ctx, p.PeerID, dev); err != nil {
		return fmt.Errorf("добавление устройства: %w", err)
	}

	// --- тестовая папка ---
	if err := os.MkdirAll(p.FolderPath, 0o700); err != nil {
		return fmt.Errorf("создание папки %s: %w", p.FolderPath, err)
	}
	folder, err := c.Folder(ctx, p.FolderID)
	if err != nil {
		return fmt.Errorf("чтение папки: %w", err)
	}
	if folder == nil {
		if folder, err = c.FolderDefaults(ctx); err != nil {
			return fmt.Errorf("шаблон папки: %w", err)
		}
		folder["id"] = p.FolderID
		folder["label"] = p.FolderID
		folder["path"] = p.FolderPath
		folder["type"] = "sendreceive"
		folder["devices"] = []any{cfgObject{"deviceID": selfID}}
		folder["versioning"] = cfgObject{
			"type":   "trashcan",
			"params": cfgObject{"cleanoutDays": strconv.Itoa(30)},
		}
	}
	if !folderHasDevice(folder, p.PeerID) {
		devs, _ := folder["devices"].([]any)
		folder["devices"] = append(devs, cfgObject{"deviceID": p.PeerID})
	}
	folder["fsWatcherEnabled"] = true
	if p.WatcherDelayS > 0 {
		folder["fsWatcherDelayS"] = p.WatcherDelayS
	}
	if p.MaxConflicts != 0 {
		folder["maxConflicts"] = p.MaxConflicts
	}
	if err := c.PutFolder(ctx, p.FolderID, folder); err != nil {
		return fmt.Errorf("настройка папки: %w", err)
	}
	if len(p.Ignores) > 0 {
		if err := c.SetIgnores(ctx, p.FolderID, p.Ignores); err != nil {
			return fmt.Errorf("исключения папки: %w", err)
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
