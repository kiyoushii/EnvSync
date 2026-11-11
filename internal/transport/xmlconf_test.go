package transport

import (
	"net/netip"
	"strings"
	"testing"
)

// Сокращённый config.xml в том виде, в каком его генерирует Syncthing 2.x.
const sampleConfig = `<configuration version="52">
    <device id="IE4XOXX-PR6QRY2-PH243IR-TWYCAJ3-3E47HU6-35ZDQGD-RBWYNO6-6MMNUQY" name="PC" compression="metadata">
        <address>dynamic</address>
    </device>
    <gui enabled="true" tls="false">
        <address>127.0.0.1:8384</address>
        <apikey>SECRETKEY</apikey>
    </gui>
    <options>
        <listenAddress>default</listenAddress>
        <globalAnnounceEnabled>true</globalAnnounceEnabled>
        <localAnnounceEnabled>true</localAnnounceEnabled>
        <relaysEnabled>true</relaysEnabled>
        <natEnabled>true</natEnabled>
        <urAccepted>0</urAccepted>
        <autoUpgradeIntervalH>12</autoUpgradeIntervalH>
        <minHomeDiskFree unit="%">1</minHomeDiskFree>
        <urUniqueID></urUniqueID>
    </options>
</configuration>`

func harden(t *testing.T, ip string) *xmlNode {
	t.Helper()
	root, err := parseXML([]byte(sampleConfig))
	if err != nil {
		t.Fatal(err)
	}
	err = hardenTree(root, HardenParams{ListenIP: netip.MustParseAddr(ip), SyncPort: 22000, GUIPort: 18384})
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestStockConfigFailsAudit(t *testing.T) {
	root, _ := parseXML([]byte(sampleConfig))
	if bad := AuditTree(root); len(bad) < 5 {
		t.Fatalf("стандартный конфиг должен нарушать правила, нарушений: %v", bad)
	}
}

func TestHardenedConfigPassesAudit(t *testing.T) {
	root := harden(t, "100.106.41.73")
	if bad := AuditTree(root); len(bad) != 0 {
		t.Fatalf("после настройки остались нарушения: %v", bad)
	}
	l := root.child("options").childrenNamed("listenAddress")
	if len(l) != 1 || l[0].Text != "tcp://100.106.41.73:22000" {
		t.Fatalf("listenAddress = %+v", l)
	}
	if got := root.child("gui").child("address").Text; got != "127.0.0.1:18384" {
		t.Fatalf("gui address = %s", got)
	}
}

func TestHardenIsIdempotentAndKeepsOtherSettings(t *testing.T) {
	root := harden(t, "100.106.41.73")
	first, _ := marshalXML(root)

	again, _ := parseXML(first)
	if err := hardenTree(again, HardenParams{ListenIP: netip.MustParseAddr("100.106.41.73"), SyncPort: 22000, GUIPort: 18384}); err != nil {
		t.Fatal(err)
	}
	second, _ := marshalXML(again)
	if string(first) != string(second) {
		t.Fatalf("повторное применение изменило файл:\n%s\n---\n%s", first, second)
	}

	// Чужие настройки и атрибуты не потеряны.
	out := string(first)
	for _, want := range []string{`id="IE4XOXX-`, `<apikey>SECRETKEY</apikey>`, `unit="%"`, `<urUniqueID></urUniqueID>`} {
		if !strings.Contains(out, want) {
			t.Errorf("в результате пропало %q", want)
		}
	}
}

func TestAddressChangeReplacesListen(t *testing.T) {
	root := harden(t, "100.106.41.73")
	if err := hardenTree(root, HardenParams{ListenIP: netip.MustParseAddr("100.64.0.9"), SyncPort: 22000, GUIPort: 18384}); err != nil {
		t.Fatal(err)
	}
	l := root.child("options").childrenNamed("listenAddress")
	if len(l) != 1 || l[0].Text != "tcp://100.64.0.9:22000" {
		t.Fatalf("после смены адреса listenAddress = %+v", l)
	}
}

func TestRefusesToListenOutsideTailscale(t *testing.T) {
	for _, ip := range []string{"0.0.0.0", "192.168.1.5", "10.0.0.2", "8.8.8.8", "::"} {
		root, _ := parseXML([]byte(sampleConfig))
		err := hardenTree(root, HardenParams{ListenIP: netip.MustParseAddr(ip), SyncPort: 22000, GUIPort: 18384})
		if err == nil {
			t.Errorf("адрес %s не должен приниматься", ip)
		}
	}
	// Заведомо невалидный адрес.
	root, _ := parseXML([]byte(sampleConfig))
	if err := hardenTree(root, HardenParams{SyncPort: 22000, GUIPort: 18384}); err == nil {
		t.Error("пустой адрес не должен приниматься")
	}
}

func TestAuditCatchesWideListenAndPublicGUI(t *testing.T) {
	root := harden(t, "100.106.41.73")
	root.child("options").child("listenAddress").Text = "tcp://0.0.0.0:22000"
	root.child("gui").child("address").Text = "0.0.0.0:8384"
	bad := strings.Join(AuditTree(root), "\n")
	if !strings.Contains(bad, "listenAddress") || !strings.Contains(bad, "gui/address") {
		t.Fatalf("аудит не заметил подмену: %q", bad)
	}
}
