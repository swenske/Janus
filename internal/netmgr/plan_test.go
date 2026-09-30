package netmgr

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/netconfig"
)

func testLinks() []linkInfo {
	return []linkInfo{
		{name: "lo", index: 1, kind: kindLoopback},
		{name: "eth0", index: 2, mac: "52:54:00:00:00:01", kind: kindPhysical, up: true},
		{name: "eth1", index: 3, mac: "52:54:00:00:00:02", kind: kindPhysical},
		{name: "eth2", index: 4, mac: "52:54:00:00:00:03", kind: kindPhysical},
	}
}

var testLease = &BootLease{Interface: "eth0", MAC: "52:54:00:00:00:01", Address: "10.0.2.15/24", Router: "10.0.2.2", DNS: []string{"10.0.2.3"}}

func mustCfg(t *testing.T, doc string) *janusv1alpha1.NetworkConfig {
	t.Helper()
	cfg, err := netconfig.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func names(ifs []ifPlan) string {
	var n []string
	for _, i := range ifs {
		n = append(n, i.name+":"+strings.TrimPrefix(i.mode.String(), "ADDRESSING_MODE_"))
	}
	return strings.Join(n, ",")
}

func TestPlanDefault(t *testing.T) {
	links := append(testLinks(), linkInfo{name: "old.10", index: 9, kind: kindVLAN, parent: 2, vlanID: 10})
	p, err := buildPlan(&janusv1alpha1.NetworkConfig{}, links, testLease)
	if err != nil {
		t.Fatal(err)
	}
	if names(p.ifaces) != "eth0:DHCP" || p.dhcp != "eth0" || len(p.down) != 0 || len(p.renames) != 0 {
		t.Fatalf("plan: %s dhcp=%s down=%v renames=%v", names(p.ifaces), p.dhcp, p.down, p.renames)
	}
	eth0 := p.ifaces[0]
	if eth0.addrs[0].String() != "10.0.2.15/24" || eth0.gw4.String() != "10.0.2.2" || eth0.metric != 1024 {
		t.Errorf("eth0: %+v", eth0)
	}
	if len(p.deleteVLANs) != 1 || p.deleteVLANs[0].name != "old.10" {
		t.Errorf("VLANs left from a trial should go: %v", p.deleteVLANs)
	}

	p, err = buildPlan(&janusv1alpha1.NetworkConfig{}, testLinks(), nil)
	if err != nil || len(p.ifaces) != 0 || len(p.down) != 0 {
		t.Fatalf("no lease: %v %v", p, err)
	}
}

func TestPlanStaticAndVLAN(t *testing.T) {
	cfg := mustCfg(t, `{"interfaces": [
		{"name": "eth0", "mode": "ADDRESSING_MODE_STATIC", "addresses": ["192.0.2.10/24"], "gateway": "192.0.2.1"},
		{"name": "eth1.100", "vlan": {"parent": "eth1", "id": 100}, "mode": "ADDRESSING_MODE_STATIC", "addresses": ["10.100.0.5/24"], "route_metric": 50}
	]}`)
	links := append(testLinks(),
		linkInfo{name: "eth1.100", index: 10, kind: kindVLAN, parent: 3, vlanID: 100},
		linkInfo{name: "eth1.200", index: 11, kind: kindVLAN, parent: 3, vlanID: 200})
	p, err := buildPlan(cfg, links, testLease)
	if err != nil {
		t.Fatal(err)
	}
	// eth1 carries the VLAN: up, no address; eth2 isn't listed: down.
	if names(p.ifaces) != "eth1:NONE,eth0:STATIC,eth1.100:STATIC" {
		t.Errorf("ifaces: %s", names(p.ifaces))
	}
	if len(p.down) != 1 || p.down[0].name != "eth2" {
		t.Errorf("down: %v", p.down)
	}
	if p.dhcp != "" {
		t.Errorf("no DHCP interface in effect, got %s", p.dhcp)
	}
	vlan := p.ifaces[2]
	if vlan.existing == nil || vlan.existing.index != 10 || vlan.metric != 50 || vlan.vlanParent != "eth1" || vlan.vlanID != 100 {
		t.Errorf("existing matching VLAN should be kept: %+v", vlan)
	}
	if p.ifaces[1].metric != 1024 || p.ifaces[1].gw4.String() != "192.0.2.1" {
		t.Errorf("eth0: %+v", p.ifaces[1])
	}
	if len(p.deleteVLANs) != 1 || p.deleteVLANs[0].name != "eth1.200" {
		t.Errorf("unlisted VLAN should go: %v", p.deleteVLANs)
	}
}

func TestPlanRecreatesChangedVLAN(t *testing.T) {
	cfg := mustCfg(t, `{"interfaces": [{"name": "v", "vlan": {"parent": "eth0", "id": 30}, "mode": "ADDRESSING_MODE_NONE"}]}`)
	links := append(testLinks(), linkInfo{name: "v", index: 12, kind: kindVLAN, parent: 2, vlanID: 20})
	p, err := buildPlan(cfg, links, nil)
	if err != nil {
		t.Fatal(err)
	}
	last := p.ifaces[len(p.ifaces)-1]
	if last.name != "v" || last.existing != nil || len(p.deleteVLANs) != 1 || p.deleteVLANs[0].name != "v" {
		t.Errorf("a VLAN whose id changed must be deleted and recreated: %+v %v", last, p.deleteVLANs)
	}
}

func TestPlanRenameByMAC(t *testing.T) {
	cfg := mustCfg(t, `{"interfaces": [
		{"name": "wan", "mac": "52:54:00:00:00:01"},
		{"name": "eth0", "mac": "52:54:00:00:00:02", "mode": "ADDRESSING_MODE_STATIC", "addresses": ["192.0.2.10/24"]}
	]}`)
	p, err := buildPlan(cfg, testLinks(), testLease)
	if err != nil {
		t.Fatal(err)
	}
	// eth0 -> wan and eth1 -> eth0: a swap, done through temporary names.
	if len(p.renames) != 2 || p.renames[0] != (rename{"eth0", "wan"}) || p.renames[1] != (rename{"eth1", "eth0"}) {
		t.Fatalf("renames: %v", p.renames)
	}
	if p.dhcp != "wan" {
		t.Errorf("the lease follows its MAC through the rename: dhcp=%s", p.dhcp)
	}
}

func TestPlanRefuses(t *testing.T) {
	cases := map[string]struct {
		doc   string
		lease *BootLease
		want  string
	}{
		"DHCP elsewhere":  {`{"interfaces": [{"name": "eth1"}]}`, testLease, "only available on the interface the kernel configured at boot (eth0"},
		"DHCP no lease":   {`{"interfaces": [{"name": "eth0"}]}`, nil, "none was obtained this boot"},
		"missing":         {`{"interfaces": [{"name": "eth9", "mode": "ADDRESSING_MODE_NONE"}]}`, testLease, "no such physical interface (physical interfaces: eth0 52:54:00:00:00:01"},
		"unknown MAC":     {`{"interfaces": [{"name": "x", "mac": "52:54:00:00:00:99", "mode": "ADDRESSING_MODE_NONE"}]}`, testLease, "no physical interface has MAC"},
		"name collision":  {`{"interfaces": [{"name": "eth2", "mac": "52:54:00:00:00:02", "mode": "ADDRESSING_MODE_NONE"}]}`, testLease, "taken by eth2, which the configuration doesn't list"},
		"bad VLAN parent": {`{"interfaces": [{"name": "v", "vlan": {"parent": "lo", "id": 5}, "mode": "ADDRESSING_MODE_NONE"}]}`, testLease, "isn't a physical interface"},
		"same link twice": {`{"interfaces": [{"name": "eth0", "mode": "ADDRESSING_MODE_NONE"}, {"name": "x", "mac": "52:54:00:00:00:01", "mode": "ADDRESSING_MODE_NONE"}]}`, testLease, "already configured as eth0"},
		"invalid":         {`{"interfaces": [{"name": "eth0", "mode": "ADDRESSING_MODE_STATIC"}]}`, testLease, "static mode needs at least one address"},
	}
	for name, c := range cases {
		cfg := &janusv1alpha1.NetworkConfig{}
		if err := protojsonUnmarshal(c.doc, cfg); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		_, err := buildPlan(cfg, testLinks(), c.lease)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to contain %q", name, err, c.want)
		}
	}
}

// protojsonUnmarshal decodes without validating, so buildPlan's own
// validation is what's exercised.
func protojsonUnmarshal(doc string, cfg *janusv1alpha1.NetworkConfig) error {
	return protojson.Unmarshal([]byte(doc), cfg)
}
