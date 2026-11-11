package transport

import (
	"encoding/xml"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strings"
)

// Этот файл делает «жёсткую» настройку Syncthing: правит его config.xml ДО запуска,
// чтобы процесс с самого начала слушал только Tailscale и не ходил в интернет.
// Если настраивать через REST уже после старта, Syncthing успеет на несколько секунд
// открыть порты на всех интерфейсах и обратиться к глобальному обнаружению.

// xmlNode — универсальный узел XML. Нужен, чтобы поменять несколько значений в config.xml
// и не потерять остальные настройки Syncthing (мы не знаем и не хотим знать всю его схему).
type xmlNode struct {
	XMLName  xml.Name
	Attrs    []xml.Attr `xml:",any,attr"`
	Text     string     `xml:",chardata"`
	Children []*xmlNode `xml:",any"`
}

func parseXML(data []byte) (*xmlNode, error) {
	var root xmlNode
	if err := xml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("разбор config.xml: %w", err)
	}
	root.trim()
	return &root, nil
}

// trim убирает пробелы-отступы, которые парсер считает текстом, иначе при записи
// они наложатся на новые отступы.
func (n *xmlNode) trim() {
	n.Text = strings.TrimSpace(n.Text)
	for _, c := range n.Children {
		c.trim()
	}
}

func (n *xmlNode) child(name string) *xmlNode {
	for _, c := range n.Children {
		if c.XMLName.Local == name {
			return c
		}
	}
	return nil
}

func (n *xmlNode) childrenNamed(name string) []*xmlNode {
	var out []*xmlNode
	for _, c := range n.Children {
		if c.XMLName.Local == name {
			out = append(out, c)
		}
	}
	return out
}

// setText задаёт значение элемента-листа, создаёт его, если нет.
func (n *xmlNode) setText(name, value string) {
	if c := n.child(name); c != nil {
		c.Text = value
		return
	}
	n.Children = append(n.Children, &xmlNode{XMLName: xml.Name{Local: name}, Text: value})
}

// setList заменяет все элементы с этим именем набором значений
// (так задаётся, например, список <listenAddress>).
// Новые элементы встают на место первого старого, чтобы повторное применение
// не двигало их по файлу и не создавало лишних изменений.
func (n *xmlNode) setList(name string, values []string) {
	fresh := make([]*xmlNode, 0, len(values))
	for _, v := range values {
		fresh = append(fresh, &xmlNode{XMLName: xml.Name{Local: name}, Text: v})
	}

	out := make([]*xmlNode, 0, len(n.Children)+len(fresh))
	inserted := false
	for _, c := range n.Children {
		if c.XMLName.Local != name {
			out = append(out, c)
			continue
		}
		if !inserted {
			out = append(out, fresh...)
			inserted = true
		}
	}
	if !inserted {
		out = append(out, fresh...)
	}
	n.Children = out
}

// HardenParams — что именно разрешено слушать.
type HardenParams struct {
	ListenIP netip.Addr // IP интерфейса Tailscale (или 127.0.0.1 в тестах)
	SyncPort int        // порт синхронизации
	GUIPort  int        // порт REST/GUI, всегда 127.0.0.1
}

// allowedListenIP: слушать можно только на loopback и на адресах Tailscale (100.64.0.0/10).
// Любой другой адрес, включая 0.0.0.0 («все интерфейсы»), считается ошибкой.
func allowedListenIP(ip netip.Addr) bool {
	if !ip.IsValid() || ip.IsUnspecified() {
		return false
	}
	return ip.IsLoopback() || netip.MustParsePrefix("100.64.0.0/10").Contains(ip)
}

// hardenTree применяет настройки к разобранному конфигу. Отдельно от работы с файлом,
// чтобы это можно было тестировать без диска.
func hardenTree(root *xmlNode, p HardenParams) error {
	if !allowedListenIP(p.ListenIP) {
		return fmt.Errorf("адрес %v не подходит для прослушивания: нужен loopback или адрес Tailscale", p.ListenIP)
	}
	if p.SyncPort <= 0 || p.SyncPort > 65535 || p.GUIPort <= 0 || p.GUIPort > 65535 {
		return errors.New("некорректный порт")
	}
	opts := root.child("options")
	gui := root.child("gui")
	if opts == nil || gui == nil {
		return errors.New("в config.xml нет секции options или gui")
	}

	// Только TCP на одном адресе. QUIC не включаем: ему нужен отдельный UDP-порт и STUN.
	opts.setList("listenAddress", []string{"tcp://" + netip.AddrPortFrom(p.ListenIP, uint16(p.SyncPort)).String()})
	opts.setText("globalAnnounceEnabled", "false") // не публикуем себя в глобальном обнаружении
	opts.setText("localAnnounceEnabled", "false")  // не кричим в локальную сеть
	opts.setText("announceLANAddresses", "false")
	opts.setText("relaysEnabled", "false") // никаких внешних релеев
	opts.setText("natEnabled", "false")    // не пробрасываем порты на роутере (UPnP)
	opts.setText("urAccepted", "-1")       // отказ от анонимной статистики
	opts.setText("crashReportingEnabled", "false")
	opts.setText("autoUpgradeIntervalH", "0") // обновляем через winget
	opts.setText("startBrowser", "false")

	gui.setText("address", netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(p.GUIPort)).String())
	return nil
}

// AuditTree проверяет, что конфиг соблюдает правила этапа 1. Возвращает список нарушений
// (пустой = всё в порядке). Используется командой `envsync audit` и тестами.
func AuditTree(root *xmlNode) []string {
	var bad []string
	opts, gui := root.child("options"), root.child("gui")
	if opts == nil || gui == nil {
		return []string{"в config.xml нет секции options или gui"}
	}

	listens := opts.childrenNamed("listenAddress")
	if len(listens) != 1 {
		bad = append(bad, fmt.Sprintf("listenAddress: ожидается ровно один адрес, найдено %d", len(listens)))
	}
	for _, l := range listens {
		ap, err := parseTCPListen(l.Text)
		if err != nil {
			bad = append(bad, fmt.Sprintf("listenAddress %q: %v", l.Text, err))
		} else if !allowedListenIP(ap.Addr()) {
			bad = append(bad, fmt.Sprintf("listenAddress %q: слушает не только Tailscale/loopback", l.Text))
		}
	}

	mustBe := map[string]string{
		"globalAnnounceEnabled": "false",
		"localAnnounceEnabled":  "false",
		"relaysEnabled":         "false",
		"natEnabled":            "false",
		"autoUpgradeIntervalH":  "0",
		"urAccepted":            "-1",
	}
	for name, want := range mustBe {
		if c := opts.child(name); c == nil || c.Text != want {
			got := "<нет>"
			if c != nil {
				got = c.Text
			}
			bad = append(bad, fmt.Sprintf("%s = %s, должно быть %s", name, got, want))
		}
	}

	if a := gui.child("address"); a == nil {
		bad = append(bad, "gui/address не задан")
	} else if ap, err := netip.ParseAddrPort(a.Text); err != nil || !ap.Addr().IsLoopback() {
		bad = append(bad, fmt.Sprintf("gui/address %q: GUI должен слушать только loopback", a.Text))
	}
	return bad
}

func parseTCPListen(s string) (netip.AddrPort, error) {
	rest, ok := strings.CutPrefix(s, "tcp://")
	if !ok {
		return netip.AddrPort{}, errors.New("нужен адрес вида tcp://ip:порт")
	}
	return netip.ParseAddrPort(rest)
}

func marshalXML(root *xmlNode) ([]byte, error) {
	out, err := xml.MarshalIndent(root, "", "    ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// writeFileAtomic пишет во временный файл и переименовывает, чтобы при сбое
// не остался наполовину записанный config.xml.
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// HardenConfigFile применяет HardenParams к config.xml на диске. Syncthing в этот момент
// не должен быть запущен: он перезапишет файл своей копией.
func HardenConfigFile(path string, p HardenParams) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	root, err := parseXML(data)
	if err != nil {
		return err
	}
	if err := hardenTree(root, p); err != nil {
		return err
	}
	out, err := marshalXML(root)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, out)
}

// AuditConfigFile читает config.xml и возвращает список нарушений.
func AuditConfigFile(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	root, err := parseXML(data)
	if err != nil {
		return nil, err
	}
	return AuditTree(root), nil
}

// ReadAPIKey достаёт ключ REST из config.xml. Ключ создаёт сам Syncthing при первой
// генерации, мы его нигде не храним отдельно: единственный источник — config.xml.
func ReadAPIKey(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	root, err := parseXML(data)
	if err != nil {
		return "", err
	}
	if gui := root.child("gui"); gui != nil {
		if k := gui.child("apikey"); k != nil && k.Text != "" {
			return k.Text, nil
		}
	}
	return "", errors.New("в config.xml нет gui/apikey")
}
