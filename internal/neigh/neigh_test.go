package neigh

import (
	"errors"
	"net"
	"os"
	"testing"

	"github.com/vishvananda/netlink"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

func TestConvert(t *testing.T) {
	mac := func(s string) net.HardwareAddr { m, _ := net.ParseMAC(s); return m }
	entries := []netlink.Neigh{
		{LinkIndex: 3, Family: netlink.FAMILY_V6, IP: net.ParseIP("fe80::1"), HardwareAddr: mac("52:54:00:00:00:fe"), State: netlink.NUD_STALE, Flags: netlink.NTF_ROUTER, Confirmed: 1234, Used: 5, Updated: 77},
		{LinkIndex: 2, Family: netlink.FAMILY_V4, IP: net.ParseIP("10.0.2.10"), State: netlink.NUD_FAILED},
		{LinkIndex: 2, Family: netlink.FAMILY_V4, IP: net.ParseIP("10.0.2.2"), HardwareAddr: mac("52:55:0a:00:02:02"), State: netlink.NUD_REACHABLE, Confirmed: 3},
		{LinkIndex: 2, Family: netlink.FAMILY_V6, IP: net.ParseIP("fe80::2"), State: netlink.NUD_INCOMPLETE},
		{LinkIndex: 9, Family: netlink.FAMILY_V4, IP: net.ParseIP("192.0.2.1"), State: netlink.NUD_PERMANENT},
		{LinkIndex: 2, Family: netlink.FAMILY_V4, IP: nil}, // no address: skipped
	}
	got := Convert(entries, map[int]string{2: "eth0", 3: "eth1"})
	want := []struct {
		iface, addr, mac string
		state            janusv1alpha1.NeighborState
	}{
		{"eth0", "10.0.2.2", "52:55:0a:00:02:02", janusv1alpha1.NeighborState_NEIGHBOR_STATE_REACHABLE},
		{"eth0", "10.0.2.10", "", janusv1alpha1.NeighborState_NEIGHBOR_STATE_FAILED},
		{"eth0", "fe80::2", "", janusv1alpha1.NeighborState_NEIGHBOR_STATE_INCOMPLETE},
		{"eth1", "fe80::1", "52:54:00:00:00:fe", janusv1alpha1.NeighborState_NEIGHBOR_STATE_STALE},
		{"if9", "192.0.2.1", "", janusv1alpha1.NeighborState_NEIGHBOR_STATE_PERMANENT},
	}
	if len(got) != len(want) {
		t.Fatalf("%d entries, want %d: %v", len(got), len(want), got)
	}
	for i, w := range want {
		g := got[i]
		if g.GetInterface() != w.iface || g.GetAddress() != w.addr || g.GetLinkAddress() != w.mac || g.GetState() != w.state {
			t.Errorf("entry %d = %s %s %s %s, want %s %s %s %s", i, g.GetInterface(), g.GetAddress(), g.GetLinkAddress(), g.GetState(), w.iface, w.addr, w.mac, w.state)
		}
	}
	// The IPv4 entry reads as IPv4 (not ::ffff:10.0.2.2), its family set.
	if got[0].GetFamily() != janusv1alpha1.NeighborFamily_NEIGHBOR_FAMILY_IPV4 || got[3].GetFamily() != janusv1alpha1.NeighborFamily_NEIGHBOR_FAMILY_IPV6 {
		t.Errorf("families: %s, %s", got[0].GetFamily(), got[3].GetFamily())
	}
	r := got[3]
	if !r.GetRouter() || r.GetConfirmedMsAgo() != 12340 || r.GetUsedMsAgo() != 50 || r.GetUpdatedMsAgo() != 770 {
		t.Errorf("router %v, ages %d/%d/%d ms, want true, 12340/50/770", r.GetRouter(), r.GetConfirmedMsAgo(), r.GetUsedMsAgo(), r.GetUpdatedMsAgo())
	}
	if got[0].GetRouter() {
		t.Error("an IPv4 entry without NTF_ROUTER reads as a router")
	}
}

func TestState(t *testing.T) {
	for nud, want := range map[int]janusv1alpha1.NeighborState{
		netlink.NUD_NONE:                          janusv1alpha1.NeighborState_NEIGHBOR_STATE_NONE,
		netlink.NUD_INCOMPLETE:                    janusv1alpha1.NeighborState_NEIGHBOR_STATE_INCOMPLETE,
		netlink.NUD_REACHABLE:                     janusv1alpha1.NeighborState_NEIGHBOR_STATE_REACHABLE,
		netlink.NUD_STALE:                         janusv1alpha1.NeighborState_NEIGHBOR_STATE_STALE,
		netlink.NUD_DELAY:                         janusv1alpha1.NeighborState_NEIGHBOR_STATE_DELAY,
		netlink.NUD_PROBE:                         janusv1alpha1.NeighborState_NEIGHBOR_STATE_PROBE,
		netlink.NUD_FAILED:                        janusv1alpha1.NeighborState_NEIGHBOR_STATE_FAILED,
		netlink.NUD_NOARP:                         janusv1alpha1.NeighborState_NEIGHBOR_STATE_NOARP,
		netlink.NUD_PERMANENT:                     janusv1alpha1.NeighborState_NEIGHBOR_STATE_PERMANENT,
		netlink.NUD_PERMANENT | netlink.NUD_NOARP: janusv1alpha1.NeighborState_NEIGHBOR_STATE_PERMANENT,
	} {
		if got := State(nud); got != want {
			t.Errorf("State(%#x) = %s, want %s", nud, got, want)
		}
	}
}

// TestInNetns reads a real table: a veth pair in a fresh network
// namespace, one neighbour set by hand and one the kernel resolved.
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
	// veth, never dummy: loading the dummy module creates a dummy0 in the
	// host's own namespace.
	if err := netlink.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "eth0"}, PeerName: "peer0"}); err != nil {
		t.Fatal(err)
	}
	eth0, _ := netlink.LinkByName("eth0")
	peer, _ := netlink.LinkByName("peer0")
	for _, l := range []netlink.Link{eth0, peer} {
		if err := netlink.LinkSetUp(l); err != nil {
			t.Fatal(err)
		}
	}
	for l, cidr := range map[netlink.Link]string{eth0: "192.0.2.1/24", peer: "192.0.2.2/24"} {
		a, _ := netlink.ParseAddr(cidr)
		if err := netlink.AddrAdd(l, a); err != nil {
			t.Fatal(err)
		}
	}
	hw, _ := net.ParseMAC("02:00:00:00:00:99")
	if err := netlink.NeighAdd(&netlink.Neigh{LinkIndex: eth0.Attrs().Index, Family: netlink.FAMILY_V4, State: netlink.NUD_PERMANENT, IP: net.ParseIP("192.0.2.99"), HardwareAddr: hw}); err != nil {
		t.Fatal(err)
	}
	// Both ends are local addresses, so traffic between them never leaves
	// through eth0: ask the kernel to resolve the peer on eth0 itself.
	if err := netlink.NeighAdd(&netlink.Neigh{LinkIndex: eth0.Attrs().Index, Family: netlink.FAMILY_V4, State: netlink.NUD_REACHABLE, IP: net.ParseIP("192.0.2.2"), HardwareAddr: peer.Attrs().HardwareAddr}); err != nil {
		t.Fatal(err)
	}

	got, err := List("eth0")
	if err != nil {
		t.Fatal(err)
	}
	byAddr := map[string]*janusv1alpha1.Neighbor{}
	for _, n := range got {
		if n.GetInterface() != "eth0" {
			t.Errorf("List(eth0) returned %s's %s", n.GetInterface(), n.GetAddress())
		}
		byAddr[n.GetAddress()] = n
	}
	if n := byAddr["192.0.2.99"]; n == nil || n.GetState() != janusv1alpha1.NeighborState_NEIGHBOR_STATE_PERMANENT || n.GetLinkAddress() != "02:00:00:00:00:99" {
		t.Errorf("192.0.2.99: %v", n)
	}
	if n := byAddr["192.0.2.2"]; n == nil || n.GetState() != janusv1alpha1.NeighborState_NEIGHBOR_STATE_REACHABLE || n.GetLinkAddress() != peer.Attrs().HardwareAddr.String() {
		t.Errorf("192.0.2.2: %v (peer0 is %s)", n, peer.Attrs().HardwareAddr)
	}
	if all, err := List(""); err != nil || len(all) < len(got) {
		t.Errorf("List(\"\") = %d entries, %v", len(all), err)
	}
	if _, err := List("eth9"); !errors.Is(err, ErrNoInterface) {
		t.Errorf("List(eth9): %v, want ErrNoInterface", err)
	}
}
