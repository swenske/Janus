// Package neigh reads the kernel's neighbour table - ARP for IPv4, NDP
// for IPv6 - over rtnetlink (RTM_GETNEIGH), for NetworkService's
// NeighborList: what `ip neigh` shows, on a node that has no shell.
package neigh

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"

	"github.com/vishvananda/netlink"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// ErrNoInterface is returned for an interface the node doesn't have.
var ErrNoInterface = errors.New("no such interface")

// msPerTick converts the kernel's NDA_CACHEINFO ages, in clock ticks
// (USER_HZ, 100 a second on every architecture Linux exposes to
// userspace), to milliseconds.
const msPerTick = 10

// List returns the neighbours of every interface, or of iface only.
func List(iface string) ([]*janusv1alpha1.Neighbor, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return nil, fmt.Errorf("list the interfaces: %w", err)
	}
	names := make(map[int]string, len(links))
	index := 0
	for _, l := range links {
		a := l.Attrs()
		names[a.Index] = a.Name
		if a.Name == iface {
			index = a.Index
		}
	}
	if iface != "" && index == 0 {
		return nil, fmt.Errorf("%w %q", ErrNoInterface, iface)
	}
	var all []netlink.Neigh
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		ns, err := netlink.NeighList(index, family)
		if err != nil {
			return nil, fmt.Errorf("read the neighbour table: %w", err)
		}
		all = append(all, ns...)
	}
	return Convert(all, names), nil
}

// Convert turns the kernel's entries into the API's, sorted by
// interface, family (IPv4 first) and address. names maps interface
// indexes to names; an entry whose interface isn't there is named by its
// index.
func Convert(entries []netlink.Neigh, names map[int]string) []*janusv1alpha1.Neighbor {
	out := make([]*janusv1alpha1.Neighbor, 0, len(entries))
	keys := make(map[*janusv1alpha1.Neighbor]netip.Addr, len(entries))
	for _, e := range entries {
		addr, ok := netip.AddrFromSlice(e.IP)
		if !ok {
			continue
		}
		addr = addr.Unmap()
		name, ok := names[e.LinkIndex]
		if !ok {
			name = fmt.Sprintf("if%d", e.LinkIndex)
		}
		n := &janusv1alpha1.Neighbor{
			Interface:      name,
			Family:         family(e.Family, addr),
			Address:        addr.String(),
			State:          State(e.State),
			Router:         e.Flags&netlink.NTF_ROUTER != 0,
			ConfirmedMsAgo: uint64(e.Confirmed) * msPerTick,
			UsedMsAgo:      uint64(e.Used) * msPerTick,
			UpdatedMsAgo:   uint64(e.Updated) * msPerTick,
		}
		if len(e.HardwareAddr) > 0 {
			n.LinkAddress = net.HardwareAddr(e.HardwareAddr).String()
		}
		keys[n] = addr
		out = append(out, n)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Interface != b.Interface {
			return a.Interface < b.Interface
		}
		if a.Family != b.Family {
			return a.Family < b.Family
		}
		return keys[a].Less(keys[b])
	})
	return out
}

func family(f int, addr netip.Addr) janusv1alpha1.NeighborFamily {
	switch {
	case f == netlink.FAMILY_V4, f == 0 && addr.Is4():
		return janusv1alpha1.NeighborFamily_NEIGHBOR_FAMILY_IPV4
	case f == netlink.FAMILY_V6, f == 0 && addr.Is6():
		return janusv1alpha1.NeighborFamily_NEIGHBOR_FAMILY_IPV6
	}
	return janusv1alpha1.NeighborFamily_NEIGHBOR_FAMILY_UNSPECIFIED
}

// State maps the kernel's NUD state. It is one NUD_* bit for every
// entry the kernel reports; the strongest wins otherwise.
func State(nud int) janusv1alpha1.NeighborState {
	for _, s := range []struct {
		bit   int
		state janusv1alpha1.NeighborState
	}{
		{netlink.NUD_PERMANENT, janusv1alpha1.NeighborState_NEIGHBOR_STATE_PERMANENT},
		{netlink.NUD_NOARP, janusv1alpha1.NeighborState_NEIGHBOR_STATE_NOARP},
		{netlink.NUD_REACHABLE, janusv1alpha1.NeighborState_NEIGHBOR_STATE_REACHABLE},
		{netlink.NUD_STALE, janusv1alpha1.NeighborState_NEIGHBOR_STATE_STALE},
		{netlink.NUD_DELAY, janusv1alpha1.NeighborState_NEIGHBOR_STATE_DELAY},
		{netlink.NUD_PROBE, janusv1alpha1.NeighborState_NEIGHBOR_STATE_PROBE},
		{netlink.NUD_FAILED, janusv1alpha1.NeighborState_NEIGHBOR_STATE_FAILED},
		{netlink.NUD_INCOMPLETE, janusv1alpha1.NeighborState_NEIGHBOR_STATE_INCOMPLETE},
	} {
		if nud&s.bit != 0 {
			return s.state
		}
	}
	return janusv1alpha1.NeighborState_NEIGHBOR_STATE_NONE
}
