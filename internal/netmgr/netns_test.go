package netmgr

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	// No DHCP server here: janusd's client gets its leases from a fake
	// one, which only answers the MACs it's given.
	server := &fakeDHCPServer{leases: map[string]*Lease{}}
	defer func(f func(int, string) dhcpTransport, w time.Duration) { newTransport, DHCPBootWait = f, w }(newTransport, DHCPBootWait)
	newTransport = func(_ int, mac string) dhcpTransport { return &fakeTransport{s: server, mac: mac} }
	DHCPBootWait = 300 * time.Millisecond

	lo, _ := netlink.LinkByName("lo")
	must(t, netlink.LinkSetUp(lo))
	for i, mac := range []string{"52:54:00:00:00:01", "52:54:00:00:00:02"} {
		hw, _ := net.ParseMAC(mac)
		must(t, netlink.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "eth" + string(rune('0'+i)), HardwareAddr: hw}, PeerName: "peer" + string(rune('0'+i))}))
	}

	var changes atomic.Int32 // OnChange also runs from the DHCP clients
	m := New(Options{Manage: true, ResolvConfPath: resolv, OnChange: func() { changes.Add(1) }})
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
	if got := fmt.Sprint(m.MainAddresses()); got != "[192.0.2.10 2001:db8::10]" {
		t.Errorf("main addresses on trial = %s", got)
	}
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
	if got := fmt.Sprint(m.MainAddresses()); got != "[192.0.2.20 2001:db8::10]" {
		t.Errorf("main addresses after the move = %s", got)
	}
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

	// Another daemon's addresses - keepalived's virtual ones, added without
	// netmgr's protocol - stay through applies that change the interface,
	// while netmgr's own go once no longer wanted. And a single IPv6
	// address (an anycast /128) skips DAD: peer0 - eth0's other end,
	// standing for another node on the segment - holds it already, which
	// had eth0's copy "dadfailed".
	withPeer := strings.Replace(mustMarshal(t, cfg), `"interfaces": [`, `"interfaces": [
		{"name": "peer0", "mode": "ADDRESSING_MODE_STATIC", "addresses": ["198.51.100.99/24"]},`, 1)
	if _, _, err := m.ApplyTrial(mustParse(t, withPeer), 5*time.Second); err != nil {
		t.Fatalf("peer0 up: %v", err)
	}
	if _, err := m.Confirm(netip.MustParseAddr("192.0.2.10")); err != nil {
		t.Fatalf("confirm peer0 up: %v", err)
	}
	eth0, _ := netlink.LinkByName("eth0")
	peer0, _ := netlink.LinkByName("peer0")
	foreign := []struct {
		link netlink.Link
		addr *netlink.Addr
	}{
		{eth0, &netlink.Addr{IPNet: mustCIDR(t, "192.0.2.100/24")}},
		{eth0, &netlink.Addr{IPNet: mustCIDR(t, "2001:db8::100/64"), Flags: unix.IFA_F_NODAD}},
		{peer0, &netlink.Addr{IPNet: mustCIDR(t, "2001:db8:7::7/128")}},
	}
	for _, f := range foreign {
		must(t, netlink.AddrAdd(f.link, f.addr))
	}
	waitAddrFlags(t, "peer0", "2001:db8:7::7/128", 0, unix.IFA_F_TENTATIVE|unix.IFA_F_DADFAILED)
	anycast := strings.Replace(withPeer, `"2001:db8::10/64"`, `"2001:db8::10/64", "203.0.113.7/32", "2001:db8:7::7/128"`, 1)
	if _, _, err := m.ApplyTrial(mustParse(t, anycast), 30*time.Second); err != nil {
		t.Fatalf("anycast addresses: %v", err)
	}
	checkAddrs(t, "eth0", "192.0.2.10/24", "192.0.2.100/24", "203.0.113.7/32", "2001:db8::10/64", "2001:db8::100/64", "2001:db8:7::7/128")
	// Tentative while DAD runs, so not passed before DAD would fail.
	waitAddrFlags(t, "eth0", "2001:db8:7::7/128", unix.IFA_F_NODAD|unix.IFA_F_DEPRECATED|unix.IFA_F_PERMANENT, unix.IFA_F_TENTATIVE|unix.IFA_F_DADFAILED)
	waitAddrFlags(t, "eth0", "2001:db8::10/64", 0, unix.IFA_F_NODAD|unix.IFA_F_DEPRECATED|unix.IFA_F_TENTATIVE|unix.IFA_F_DADFAILED)
	// The node's own connections never leave from the anycast /128 -
	// nor, in IPv4, from the /32.
	checkNotSource(t, "2001:db8:99::1", "2001:db8:7::7")
	checkNotSource(t, "198.18.0.1", "203.0.113.7")
	checkAddrs(t, "peer0", "198.51.100.99/24", "2001:db8:7::7/128")
	if _, err := m.Confirm(netip.MustParseAddr("192.0.2.10")); err != nil {
		t.Fatalf("confirm the anycast addresses: %v", err)
	}
	if _, _, err := m.ApplyTrial(cfg, 5*time.Second); err != nil {
		t.Fatalf("anycast addresses away: %v", err)
	}
	checkAddrs(t, "eth0", "192.0.2.10/24", "192.0.2.100/24", "2001:db8::10/64", "2001:db8::100/64")
	checkAddrs(t, "peer0") // down: no IPv6 address outlives that (keep_addr_on_down)
	if _, err := m.Confirm(netip.MustParseAddr("192.0.2.10")); err != nil {
		t.Fatalf("confirm the anycast addresses away: %v", err)
	}
	for _, f := range foreign[:2] {
		must(t, netlink.AddrDel(f.link, f.addr))
	}

	// The kernel's boot DHCP address isn't netmgr's either, but it goes
	// once the interface isn't in DHCP mode.
	m.mu.Lock()
	m.kernelLease = &Lease{Interface: "eth1", MAC: "52:54:00:00:00:02", Address: "10.200.0.9/24"}
	m.mu.Unlock()
	eth1, _ := netlink.LinkByName("eth1")
	must(t, netlink.AddrAdd(eth1, &netlink.Addr{IPNet: mustCIDR(t, "10.200.0.9/24")}))
	dhcp := strings.Replace(mustMarshal(t, cfg), `"interfaces": [`, `"interfaces": [
		{"name": "eth1", "mode": "ADDRESSING_MODE_DHCP"},`, 1)
	if _, _, err := m.ApplyTrial(mustParse(t, dhcp), 5*time.Second); err != nil {
		t.Fatalf("eth1 in DHCP mode: %v", err)
	}
	checkAddrs(t, "eth1", "10.200.0.9/24")
	if _, err := m.Confirm(netip.MustParseAddr("192.0.2.10")); err != nil {
		t.Fatalf("confirm DHCP on eth1: %v", err)
	}
	if _, _, err := m.ApplyTrial(cfg, 5*time.Second); err != nil {
		t.Fatalf("eth1 out of DHCP mode: %v", err)
	}
	checkAddrs(t, "eth1")
	if _, err := m.Confirm(netip.MustParseAddr("192.0.2.10")); err != nil {
		t.Fatalf("confirm eth1 out of DHCP mode: %v", err)
	}
	m.mu.Lock()
	m.kernelLease = nil
	m.mu.Unlock()

	// janusd's own client: DHCP on eth1 again, a server answering now.
	// The lease's address, route and DNS arrive with it, its renewal
	// with new values is applied, and its loss takes them away.
	server.set("52:54:00:00:00:02", &Lease{Address: "10.201.0.9/24", Router: "10.201.0.1", DNS: []string{"10.201.0.53"}})
	changes.Store(0)
	if _, _, err := m.ApplyTrial(mustParse(t, dhcp), 5*time.Second); err != nil {
		t.Fatalf("eth1 in DHCP mode, with a server: %v", err)
	}
	waitFor(t, "eth1's lease", func() bool { return hasAddr("eth1", "10.201.0.9/24") })
	checkDefaultRoutes(t, "eth1", "10.201.0.1@1024")
	if data, _ := os.ReadFile(resolv); !strings.Contains(string(data), "nameserver 192.0.2.53") {
		t.Errorf("the configured DNS wins over the lease's:\n%s", data)
	}
	if st, err := m.Status(); err != nil || !leaseShown(st, "eth1", "10.201.0.9/24") {
		t.Errorf("status doesn't show eth1's lease: %v", err)
	}
	if _, err := os.Stat(leaseFile("52:54:00:00:00:02")); err != nil {
		t.Errorf("the lease isn't recorded for a restarted janusd: %v", err)
	}
	// Renewals that change nothing but its times: Status follows them.
	first := leaseEnd(t, m, "eth1")
	waitFor(t, "a renewal's new end in the status", func() bool { return leaseEnd(t, m, "eth1") > first })
	server.set("52:54:00:00:00:02", &Lease{Address: "10.201.0.10/24", Router: "10.201.0.1"})
	waitFor(t, "the renewal's new address", func() bool { return hasAddr("eth1", "10.201.0.10/24") && !hasAddr("eth1", "10.201.0.9/24") })
	server.set("52:54:00:00:00:02", nil) // refuses
	waitFor(t, "the lease gone", func() bool { return !hasAddr("eth1", "10.201.0.10/24") })
	checkDefaultRoutes(t, "eth1")
	if _, err := m.Confirm(netip.MustParseAddr("192.0.2.10")); err != nil {
		t.Fatalf("confirm DHCP on eth1 with a server: %v", err)
	}
	if changes.Load() == 0 {
		t.Error("a lease change must call OnChange")
	}
	if _, _, err := m.ApplyTrial(cfg, 5*time.Second); err != nil {
		t.Fatalf("eth1 out of DHCP mode again: %v", err)
	}
	if _, err := m.Confirm(netip.MustParseAddr("192.0.2.10")); err != nil {
		t.Fatalf("confirm eth1 out of DHCP mode again: %v", err)
	}

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
	if changes.Load() == 0 {
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
	addrs, err := listAddrs(l.Attrs().Index)
	if err != nil {
		t.Errorf("%s: %v", name, err)
		return
	}
	var got []string
	for _, a := range addrs {
		if managed(a) {
			got = append(got, a.prefix.String())
		}
	}
	slices.Sort(got)
	want = slices.Sorted(slices.Values(want))
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s addresses = %v, want %v", name, got, want)
	}
}

// waitAddrFlags waits up to 5 s for link name's address prefix to have
// every flag in set and none in clear.
func waitAddrFlags(t *testing.T, name, prefix string, set, clear uint32) {
	t.Helper()
	l, err := netlink.LinkByName(name)
	if err != nil {
		t.Errorf("%s: %v", name, err)
		return
	}
	want := netip.MustParsePrefix(prefix)
	var flags uint32
	found := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		addrs, err := listAddrs(l.Attrs().Index)
		if err != nil {
			t.Fatal(err)
		}
		found = false
		for _, a := range addrs {
			if a.prefix == want {
				found, flags = true, a.flags
			}
		}
		if found && flags&set == set && flags&clear == 0 {
			return
		}
	}
	t.Errorf("%s %s: found %v, flags %#x - want %#x set, %#x clear", name, prefix, found, flags, set, clear)
}

// checkNotSource checks the kernel picks a source other than notWant to
// reach dst.
func checkNotSource(t *testing.T, dst, notWant string) {
	t.Helper()
	routes, err := netlink.RouteGet(net.ParseIP(dst))
	if err != nil || len(routes) == 0 {
		t.Errorf("route to %s: %v", dst, err)
		return
	}
	if got := routes[0].Src.String(); got == notWant || routes[0].Src == nil {
		t.Errorf("source towards %s = %s, want any but %s", dst, got, notWant)
	}
}

func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	n, err := netlink.ParseIPNet(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
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
		if r.Protocol != unix.RTPROT_STATIC && r.Protocol != unix.RTPROT_BOOT { // boot: a DHCP lease's
			t.Errorf("%s: route via %s has protocol %d", name, r.Gw, r.Protocol)
		}
		got = append(got, r.Gw.String()+"@"+strconv.Itoa(r.Priority))
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s default routes = %v, want %v", name, got, want)
	}
}

// waitFor polls ok for up to 10 s.
func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func hasAddr(name, prefix string) bool {
	l, err := netlink.LinkByName(name)
	if err != nil {
		return false
	}
	addrs, err := listAddrs(l.Attrs().Index)
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if a.prefix.String() == prefix {
			return true
		}
	}
	return false
}

func leaseEnd(t *testing.T, m *Manager, name string) int64 {
	t.Helper()
	st, err := m.Status()
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range st.GetInterfaces() {
		if i.GetName() == name {
			return i.GetDhcp().GetExpiresUnix()
		}
	}
	return 0
}

func leaseShown(st *janusv1alpha1.NetworkStatusResponse, name, addr string) bool {
	for _, i := range st.GetInterfaces() {
		if i.GetName() == name && i.GetDhcp().GetAddress() == addr && i.GetDhcp().GetExpiresUnix() > time.Now().Unix() {
			return true
		}
	}
	return false
}

// fakeDHCPServer hands out the leases it's given, by MAC: unknown, no
// answer; nil, a refusal. Its leases are renewed every 200 ms.
type fakeDHCPServer struct {
	mu     sync.Mutex
	leases map[string]*Lease
}

func (s *fakeDHCPServer) set(mac string, l *Lease) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.leases[mac] = l
}

type fakeTransport struct {
	s   *fakeDHCPServer
	mac string
}

func (f *fakeTransport) lease() (*Lease, bool) {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	l, ok := f.s.leases[f.mac]
	if !ok || l == nil {
		return nil, ok
	}
	cp := *l
	cp.MAC, cp.Interface = f.mac, "eth?"
	now := sinceBoot()
	cp.Obtained, cp.Renew, cp.Rebind, cp.Expires = now, now+200*time.Millisecond, now+2*time.Hour, now+3*time.Hour
	return &cp, true
}

func (f *fakeTransport) acquire(ctx context.Context, _ netip.Addr) (*Lease, error) {
	for {
		if l, _ := f.lease(); l != nil {
			return l, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (f *fakeTransport) renew(_ context.Context, _ *Lease) (*Lease, error) {
	l, known := f.lease()
	if l == nil && known {
		return nil, errNAK
	}
	if l == nil {
		return nil, errors.New("no answer")
	}
	return l, nil
}
