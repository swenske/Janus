package netmgr

import (
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/netconfig"
)

// TestInNetns applies configurations to a real kernel - but only inside
// a throwaway network and UTS namespace, with veth interfaces standing
// in for NICs. It refuses to run anywhere else: run it as
//
//	go test -c -o netmgr.test ./internal/netmgr
//	sudo JANUS_NETNS_TEST=1 unshare -nu ./netmgr.test -test.run TestInNetns -test.v
func TestInNetns(t *testing.T) {
	if os.Getenv("JANUS_NETNS_TEST") != "1" {
		t.Skip("set JANUS_NETNS_TEST=1 and run under `unshare -nu` as root")
	}
	links, err := netlink.LinkList()
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 || links[0].Attrs().Name != "lo" {
		t.Fatalf("refusing to run: this isn't a fresh network namespace (%d interfaces)", len(links))
	}
	if h, _ := os.Hostname(); h == "" {
		t.Fatal("no hostname")
	}

	// veth, not dummy: loading the dummy module creates a dummy0 in the
	// *initial* network namespace (numdummies=1) - a side effect on the
	// host this test must not have. veth creates nothing on load.
	physicalTypes["veth"] = true
	defer delete(physicalTypes, "veth")
	netconfig.Dir = t.TempDir()
	RunDir = t.TempDir()
	resolv := filepath.Join(t.TempDir(), "resolv.conf")
	pnpPath = filepath.Join(t.TempDir(), "absent") // no boot DHCP here

	lo, _ := netlink.LinkByName("lo")
	must(t, netlink.LinkSetUp(lo))
	for i, mac := range []string{"52:54:00:00:00:01", "52:54:00:00:00:02"} {
		hw, _ := net.ParseMAC(mac)
		must(t, netlink.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "eth" + string(rune('0'+i)), HardwareAddr: hw}, PeerName: "peer" + string(rune('0'+i))}))
	}

	changes := 0
	m := New(Options{Manage: true, ResolvConfPath: resolv, OnChange: func() { changes++ }})
	if err := m.Start(); err != nil {
		t.Fatalf("start with the default configuration: %v", err)
	}
	// The first physical interface by index names the host (veth peers
	// count as physical here, so compute it rather than assume eth0).
	infos, _ := listLinks()
	if h, _ := os.Hostname(); h != netconfig.DefaultHostname(firstMAC(infos)) || !strings.HasPrefix(h, "janus-") {
		t.Errorf("default hostname = %s, want %s", h, netconfig.DefaultHostname(firstMAC(infos)))
	}

	// A static interface, a VLAN on the other one, DNS, hostname.
	cfg := mustParse(t, `{
		"hostname": "lb1",
		"interfaces": [
			{"name": "eth0", "mode": "ADDRESSING_MODE_STATIC", "addresses": ["192.0.2.10/24", "2001:db8::10/64"], "gateway": "192.0.2.1", "gateway6": "2001:db8::1", "mtu": 1400},
			{"name": "eth1.100", "vlan": {"parent": "eth1", "id": 100}, "mode": "ADDRESSING_MODE_STATIC", "addresses": ["10.100.0.5/24"], "gateway": "10.100.0.1"}
		],
		"dns": {"servers": ["192.0.2.53"], "search": ["example.net"]}
	}`)
	addrs, revertAt, err := m.ApplyTrial(cfg, 5*time.Second)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	t.Logf("on trial, addresses %v, revert at %s", addrs, revertAt.Format(time.TimeOnly))
	checkAddrs(t, "eth0", "192.0.2.10/24", "2001:db8::10/64")
	checkAddrs(t, "eth1.100", "10.100.0.5/24")
	checkAddrs(t, "eth1")
	checkDefaultRoutes(t, "eth0", "192.0.2.1@1024", "2001:db8::1@1024")
	checkDefaultRoutes(t, "eth1.100", "10.100.0.1@1025")
	if l, _ := netlink.LinkByName("eth0"); l.Attrs().MTU != 1400 {
		t.Errorf("eth0 MTU = %d", l.Attrs().MTU)
	}
	if l, _ := netlink.LinkByName("eth1.100"); l == nil || l.Type() != "vlan" || l.(*netlink.Vlan).VlanId != 100 {
		t.Errorf("VLAN eth1.100 missing or wrong: %v", l)
	}
	if l, _ := netlink.LinkByName("eth1"); l.Attrs().Flags&net.FlagUp == 0 {
		t.Error("eth1 carries a VLAN and must be up")
	}
	if h, _ := os.Hostname(); h != "lb1" {
		t.Errorf("hostname = %s", h)
	}
	if data, _ := os.ReadFile(resolv); !strings.Contains(string(data), "nameserver 192.0.2.53") || !strings.Contains(string(data), "search example.net") {
		t.Errorf("resolv.conf:\n%s", data)
	}

	// A second trial is refused; confirmation must come on a node address.
	if _, _, err := m.ApplyTrial(cfg, 5*time.Second); err != ErrTrialPending {
		t.Errorf("second trial: %v", err)
	}
	if _, err := m.Confirm(netip.MustParseAddr("127.0.0.1")); err == nil {
		t.Error("confirmed over loopback")
	}
	if _, err := m.Confirm(netip.MustParseAddr("198.51.100.1")); err == nil {
		t.Error("confirmed over an address the node doesn't have")
	}
	if via, err := m.Confirm(netip.MustParseAddr("192.0.2.10")); err != nil || via != "192.0.2.10" {
		t.Fatalf("confirm: %s %v", via, err)
	}
	if !netconfig.Exists() {
		t.Fatal("a confirmed configuration must be saved")
	}

	// Moving eth0 to another address of the same subnet: removing the old
	// (primary) address takes the new (secondary) one with it when
	// promote_secondaries is off, as it is in a fresh namespace.
	moved := mustParse(t, strings.Replace(mustMarshal(t, cfg), "192.0.2.10/24", "192.0.2.20/24", 1))
	if _, _, err := m.ApplyTrial(moved, 5*time.Second); err != nil {
		t.Fatalf("same-subnet move: %v", err)
	}
	checkAddrs(t, "eth0", "192.0.2.20/24", "2001:db8::10/64")
	checkDefaultRoutes(t, "eth0", "192.0.2.1@1024", "2001:db8::1@1024")
	if _, err := m.Confirm(netip.MustParseAddr("192.0.2.20")); err != nil {
		t.Fatalf("confirm the move: %v", err)
	}
	if _, _, err := m.ApplyTrial(cfg, 5*time.Second); err != nil {
		t.Fatalf("move back: %v", err)
	}
	if _, err := m.Confirm(netip.MustParseAddr("192.0.2.10")); err != nil {
		t.Fatalf("confirm the move back: %v", err)
	}

	// Re-applying the same configuration changes nothing.
	if err := m.Start(); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	checkAddrs(t, "eth0", "192.0.2.10/24", "2001:db8::10/64")
	checkDefaultRoutes(t, "eth0", "192.0.2.1@1024", "2001:db8::1@1024")

	// A new trial that renames eth1 by MAC, drops the VLAN and moves eth0:
	// not confirmed, so it must revert by itself.
	next := mustParse(t, `{"interfaces": [
		{"name": "eth0", "mode": "ADDRESSING_MODE_STATIC", "addresses": ["198.51.100.7/24"]},
		{"name": "lan", "mac": "52:54:00:00:00:02", "mode": "ADDRESSING_MODE_STATIC", "addresses": ["10.9.0.1/16"]}
	]}`)
	if _, _, err := m.ApplyTrial(next, 5*time.Second); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	checkAddrs(t, "eth0", "198.51.100.7/24")
	checkAddrs(t, "lan", "10.9.0.1/16")
	if _, err := netlink.LinkByName("eth1.100"); err == nil {
		t.Error("the VLAN should be gone")
	}
	checkDefaultRoutes(t, "eth0")
	time.Sleep(6 * time.Second)
	if pending, _ := m.Trial(); pending {
		t.Fatal("trial still pending after its timeout")
	}
	// Back to the confirmed configuration, all of it: lan renamed back to
	// eth1 (the confirmed configuration's VLAN hangs off that name), the
	// VLAN recreated, eth0's addresses and routes restored.
	if _, err := netlink.LinkByName("eth1"); err != nil {
		t.Errorf("eth1's name wasn't restored: %v", err)
	}
	checkAddrs(t, "eth0", "192.0.2.10/24", "2001:db8::10/64")
	checkDefaultRoutes(t, "eth0", "192.0.2.1@1024", "2001:db8::1@1024")
	checkAddrs(t, "eth1.100", "10.100.0.5/24")
	if cur, isDefault := m.Config(); isDefault || len(cur.GetInterfaces()) != 2 {
		t.Errorf("configuration after revert: %v (default %v)", cur, isDefault)
	}

	// A configuration naming an interface that doesn't exist is refused
	// before anything changes.
	before, _ := globalAddresses()
	if _, _, err := m.ApplyTrial(mustParse(t, `{"interfaces": [{"name": "eth7", "mode": "ADDRESSING_MODE_NONE"}]}`), 5*time.Second); err == nil {
		t.Error("an absent interface was accepted")
	}
	after, _ := globalAddresses()
	if strings.Join(before, ",") != strings.Join(after, ",") {
		t.Errorf("a refused configuration changed addresses: %v -> %v", before, after)
	}
	if changes == 0 {
		t.Error("OnChange never called")
	}
}

func mustParse(t *testing.T, doc string) *janusv1alpha1.NetworkConfig {
	t.Helper()
	cfg, err := netconfig.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func mustMarshal(t *testing.T, cfg *janusv1alpha1.NetworkConfig) string {
	t.Helper()
	data, err := netconfig.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func checkAddrs(t *testing.T, name string, want ...string) {
	t.Helper()
	l, err := netlink.LinkByName(name)
	if err != nil {
		t.Errorf("%s: %v", name, err)
		return
	}
	addrs, _ := netlink.AddrList(l, netlink.FAMILY_ALL)
	var got []string
	for _, a := range addrs {
		if p, ok := managed(a); ok {
			got = append(got, p.String())
		}
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s addresses = %v, want %v", name, got, want)
	}
}

func checkDefaultRoutes(t *testing.T, name string, want ...string) {
	t.Helper()
	l, err := netlink.LinkByName(name)
	if err != nil {
		t.Errorf("%s: %v", name, err)
		return
	}
	routes, _ := managedRoutes(l)
	var got []string
	for _, r := range routes {
		if r.Protocol != unix.RTPROT_STATIC {
			t.Errorf("%s: route via %s has protocol %d", name, r.Gw, r.Protocol)
		}
		got = append(got, r.Gw.String()+"@"+strconv.Itoa(r.Priority))
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s default routes = %v, want %v", name, got, want)
	}
}
