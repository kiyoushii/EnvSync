package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"os/exec"
	"strings"
)

// TSNode — одна машина в tailnet (мы сами или соседняя).
type TSNode struct {
	Name    string     // короткое имя, например DESKTOP-MGFILO0
	DNSName string     // полное имя в tailnet
	IPv4    netip.Addr // адрес Tailscale (100.x.y.z)
	Online  bool
}

// TSStatus — разобранный вывод `tailscale status --json`.
type TSStatus struct {
	BackendState string // Running, Stopped, NeedsLogin и т. д.
	Self         TSNode
	Peers        []TSNode
}

// Running: Tailscale вошёл в сеть и у нас есть адрес. Только в этом состоянии
// Syncthing разрешено слушать порт.
func (s *TSStatus) Running() bool {
	return s.BackendState == "Running" && s.Self.IPv4.IsValid()
}

// FindPeer ищет соседа по короткому имени или по первой части DNS-имени,
// без учёта регистра.
func (s *TSStatus) FindPeer(name string) (TSNode, bool) {
	want := strings.ToLower(strings.TrimSpace(name))
	for _, p := range s.Peers {
		short, _, _ := strings.Cut(strings.ToLower(p.DNSName), ".")
		if strings.ToLower(p.Name) == want || short == want {
			return p, true
		}
	}
	return TSNode{}, false
}

// PeerNames — имена соседей для сообщений об ошибках.
func (s *TSStatus) PeerNames() []string {
	names := make([]string, 0, len(s.Peers))
	for _, p := range s.Peers {
		names = append(names, p.Name)
	}
	return names
}

type rawNode struct {
	HostName     string
	DNSName      string
	TailscaleIPs []string
	Online       bool
}

func (r rawNode) toNode() TSNode {
	n := TSNode{Name: r.HostName, DNSName: r.DNSName, Online: r.Online}
	for _, s := range r.TailscaleIPs {
		if ip, err := netip.ParseAddr(s); err == nil && ip.Is4() {
			n.IPv4 = ip
			break
		}
	}
	return n
}

// ParseTSStatus разбирает JSON из `tailscale status --json`.
func ParseTSStatus(data []byte) (*TSStatus, error) {
	var raw struct {
		BackendState string
		Self         *rawNode
		Peer         map[string]rawNode
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("разбор вывода tailscale: %w", err)
	}
	st := &TSStatus{BackendState: raw.BackendState}
	if raw.Self != nil {
		st.Self = raw.Self.toNode()
	}
	for _, p := range raw.Peer {
		st.Peers = append(st.Peers, p.toNode())
	}
	return st, nil
}

// Runner запускает внешнюю команду. Вынесен в тип, чтобы в тестах подставлять
// готовый вывод вместо настоящего tailscale.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// ExecRunner — настоящий запуск процесса.
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

// Tailscale — обёртка над tailscale CLI. Мы только читаем статус и ничего не меняем:
// вход и выход из сети остаются за человеком.
type Tailscale struct {
	Bin string
	Run Runner
}

// Status возвращает текущее состояние Tailscale.
func (t *Tailscale) Status(ctx context.Context) (*TSStatus, error) {
	run := t.Run
	if run == nil {
		run = ExecRunner
	}
	out, err := run(ctx, t.Bin, "status", "--json")
	if err != nil {
		// Когда Tailscale разлогинен, CLI может вернуть ненулевой код, но всё же
		// напечатать корректный JSON с BackendState. Такой ответ полезнее ошибки.
		if st, perr := ParseTSStatus(out); perr == nil && st.BackendState != "" {
			return st, nil
		}
		return nil, fmt.Errorf("tailscale status: %w", err)
	}
	return ParseTSStatus(out)
}
