package clusters

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

func netConfig(t *testing.T, js string) *janusv1alpha1.NetworkConfig {
	t.Helper()
	cfg := &janusv1alpha1.NetworkConfig{}
	if err := protojson.Unmarshal([]byte(js), cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func netStatus(t *testing.T, js string) *janusv1alpha1.NetworkStatusResponse {
	t.Helper()
	st := &janusv1alpha1.NetworkStatusResponse{}
	if err := protojson.Unmarshal([]byte(js), st); err != nil {
		t.Fatal(err)
	}
	return st
}

// What a replacement takes over: the static addressing of the interface
// holding the member's address, that address first.
func TestAddressingOf(t *testing.T) {
	cfg := netConfig(t, `{"interfaces": [
		{"name": "mgmt", "mac": "52:54:00:00:00:01", "mode": "ADDRESSING_MODE_DHCP"},
		{"name": "lan", "mac": "52:54:00:00:00:02", "mode": "ADDRESSING_MODE_STATIC",
		 "addresses": ["2001:db8::161/64", "172.16.1.161/24"], "gateway": "172.16.1.1", "gateway6": "2001:db8::1"}]}`)
	a, err := AddressingOf(cfg, "172.16.1.161")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(a.Addresses, " ") != "172.16.1.161/24 2001:db8::161/64" || a.Gateway != "172.16.1.1" || a.Gateway6 != "2001:db8::1" || a.Address() != "172.16.1.161" {
		t.Errorf("addressing: %+v", a)
	}
	if _, err := AddressingOf(cfg, "172.16.1.99"); err == nil || !strings.Contains(err.Error(), "DHCP") {
		t.Errorf("an address the configuration doesn't hold: %v", err)
	}
}

func TestAddressingCheck(t *testing.T) {
	for _, bad := range []Addressing{
		{},
		{Addresses: []string{"172.16.1.161"}},
		{Addresses: []string{"127.0.0.1/8"}},
		{Addresses: []string{"172.16.1.161/24", "172.16.1.161/24"}},
		{Addresses: []string{"172.16.1.161/24"}, Gateway: "2001:db8::1"},
		{Addresses: []string{"172.16.1.161/24"}, Gateway6: "172.16.1.1"},
	} {
		if bad.Check() == nil {
			t.Errorf("%+v passed", bad)
		}
	}
	if err := (Addressing{Addresses: []string{"172.16.1.161/24"}, Gateway: "172.16.1.1"}).Check(); err != nil {
		t.Error(err)
	}
}

// A replacement's interface gets the old member's addressing - found by
// what holds its own address, whether its configuration lists it or not.
func TestReaddressed(t *testing.T) {
	to := Addressing{Addresses: []string{"172.16.1.161/24", "2001:db8::161/64"}, Gateway: "172.16.1.1", Gateway6: "2001:db8::1"}

	// Static on a temporary address: the interface keeps its MTU and
	// MAC; the other interface, its VLAN, the hostname, DNS stay.
	cfg := netConfig(t, `{"hostname": "edge-1", "interfaces": [
		{"name": "lan", "mac": "52:54:00:00:00:12", "mode": "ADDRESSING_MODE_STATIC", "addresses": ["172.16.1.170/24"], "gateway": "172.16.1.1", "mtu": 9000},
		{"name": "lan.20", "vlan": {"parent": "lan", "id": 20}, "mode": "ADDRESSING_MODE_STATIC", "addresses": ["10.20.0.5/24"]}],
		"dns": {"servers": ["172.16.1.53"]}}`)
	st := netStatus(t, `{"interfaces": [
		{"name": "lan", "kind": "physical", "mac": "52:54:00:00:00:12", "addresses": ["172.16.1.170/24", "fe80::1/64"]},
		{"name": "lan.20", "kind": "vlan", "addresses": ["10.20.0.5/24"], "vlanParent": "lan", "vlanId": 20}],
		"dnsServers": ["192.0.2.53"]}`)
	out, err := Readdressed(cfg, st, "172.16.1.170", to)
	if err != nil {
		t.Fatal(err)
	}
	lan := out.GetInterfaces()[0]
	if lan.GetMode() != janusv1alpha1.AddressingMode_ADDRESSING_MODE_STATIC || strings.Join(lan.GetAddresses(), " ") != "172.16.1.161/24 2001:db8::161/64" ||
		lan.GetGateway() != "172.16.1.1" || lan.GetGateway6() != "2001:db8::1" || lan.GetMtu() != 9000 || lan.GetMac() != "52:54:00:00:00:12" {
		t.Errorf("the interface: %v", lan)
	}
	if len(out.GetInterfaces()) != 2 || out.GetInterfaces()[1].GetAddresses()[0] != "10.20.0.5/24" || out.GetHostname() != "edge-1" || out.GetDns().GetServers()[0] != "172.16.1.53" {
		t.Errorf("the rest changed: %v", out)
	}
	if cfg.GetInterfaces()[0].GetAddresses()[0] != "172.16.1.170/24" {
		t.Error("the configuration given was changed")
	}

	// Left to DHCP - nothing listed: the interface listed by its MAC,
	// and the DNS servers the lease gave kept.
	st = netStatus(t, `{"interfaces": [
		{"name": "eth0", "kind": "physical", "mac": "52:54:00:00:00:13", "addresses": ["172.16.1.200/24"], "mode": "ADDRESSING_MODE_DHCP"}],
		"dnsServers": ["172.16.1.53"], "dnsSearch": ["example.net"]}`)
	out, err = Readdressed(&janusv1alpha1.NetworkConfig{}, st, "172.16.1.200", to)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.GetInterfaces()) != 1 || out.GetInterfaces()[0].GetMac() != "52:54:00:00:00:13" || out.GetInterfaces()[0].GetName() != "eth0" ||
		out.GetInterfaces()[0].GetMode() != janusv1alpha1.AddressingMode_ADDRESSING_MODE_STATIC || out.GetDns().GetServers()[0] != "172.16.1.53" || out.GetDns().GetSearch()[0] != "example.net" {
		t.Errorf("from DHCP: %v", out)
	}

	// An address the node doesn't hold, or one another interface has.
	if _, err := Readdressed(&janusv1alpha1.NetworkConfig{}, st, "172.16.1.99", to); err == nil {
		t.Error("an address no interface holds")
	}
	cfg = netConfig(t, `{"interfaces": [
		{"name": "eth0", "mac": "52:54:00:00:00:13", "mode": "ADDRESSING_MODE_DHCP"},
		{"name": "eth1", "mode": "ADDRESSING_MODE_STATIC", "addresses": ["172.16.1.161/24"]}]}`)
	if _, err := Readdressed(cfg, st, "172.16.1.200", to); err == nil || !strings.Contains(err.Error(), "eth1") {
		t.Errorf("an address on another interface: %v", err)
	}
}
