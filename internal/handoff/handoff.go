// Package handoff hands a cluster member's open connections over
// (docs/guide/clusters.md "Handing connections over"). In an anycast
// cluster every member holds the service addresses and the router in
// front spreads the connections over them by a hash: when a member's
// routes are withdrawn - drained - the router sends its connections'
// packets to another member, which has no such connection and resets
// it; when it comes back, the hash sends part of the connections opened
// meanwhile to it, reset in turn. A member that hands another's
// connections over copies each packet of one of them to that member (nft's
// dup statement, to the member's own address on the link) and drops its
// own copy: the member that has the connection answers, straight to the
// client.
//
// The connections are read from their member (ClusterFlows), every few
// seconds while a handoff lasts; janusd keeps its nftables tables -
// "janus_handoff", ip and ip6 - beside the firewall's ruleset, which
// adds them back each time it replaces itself (firewall.Reserved).
package handoff

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

// Table is the nftables tables' name, in the ip and ip6 families.
const Table = "janus_handoff"

// Flow is a TCP connection as the member that has it sees it.
type Flow struct {
	Remote, Local netip.AddrPort
}

// Is6 reports whether f is an IPv6 connection.
func (f Flow) Is6() bool { return f.Local.Addr().Is6() && !f.Local.Addr().Is4In6() }

func (f Flow) String() string { return f.Remote.String() + " > " + f.Local.String() }

// Compare orders flows: by family, then local, then remote.
func Compare(a, b Flow) int {
	if a.Is6() != b.Is6() {
		if a.Is6() {
			return 1
		}
		return -1
	}
	if c := a.Local.Compare(b.Local); c != 0 {
		return c
	}
	return a.Remote.Compare(b.Remote)
}

// Pack encodes flows, back to back, big endian - the remote address and
// port, the local address and port: 12 bytes each for IPv4, 36 for IPv6
// (ClusterFlowsChunk).
func Pack(flows []Flow) (v4, v6 []byte) {
	for _, f := range flows {
		if f.Is6() {
			v6 = appendAddrPort(v6, f.Remote, true)
			v6 = appendAddrPort(v6, f.Local, true)
		} else {
			v4 = appendAddrPort(v4, f.Remote, false)
			v4 = appendAddrPort(v4, f.Local, false)
		}
	}
	return v4, v6
}

func appendAddrPort(b []byte, ap netip.AddrPort, v6 bool) []byte {
	a := ap.Addr().Unmap()
	if v6 {
		s := a.As16()
		b = append(b, s[:]...)
	} else {
		s := a.As4()
		b = append(b, s[:]...)
	}
	return binary.BigEndian.AppendUint16(b, ap.Port())
}

// Unpack decodes what Pack encoded, one family at a time.
func Unpack(b []byte, v6 bool) ([]Flow, error) {
	size := 12
	if v6 {
		size = 36
	}
	if len(b)%size != 0 {
		return nil, fmt.Errorf("%d bytes isn't a whole number of %d-byte connections", len(b), size)
	}
	out := make([]Flow, 0, len(b)/size)
	for i := 0; i < len(b); i += size {
		half := size / 2
		out = append(out, Flow{Remote: addrPort(b[i:i+half], v6), Local: addrPort(b[i+half:i+size], v6)})
	}
	return out, nil
}

func addrPort(b []byte, v6 bool) netip.AddrPort {
	var a netip.Addr
	if v6 {
		a = netip.AddrFrom16([16]byte(b[:16]))
	} else {
		a = netip.AddrFrom4([4]byte(b[:4]))
	}
	return netip.AddrPortFrom(a, binary.BigEndian.Uint16(b[len(b)-2:]))
}

// Owner is a member whose connections are copied to it: Reach4/Reach6
// its address on the link in each family, Dev4/Dev6 this node's
// interface toward it.
type Owner struct {
	Name           string
	Reach4, Reach6 netip.Addr
	Dev4, Dev6     string
	Flows          []Flow
}

// Guards are what a member does with the connections it can't hand
// over yet: Refuse - a member about to be drained - drops new
// connections' SYNs on those addresses, so it takes none the others'
// reading of its connections would miss; Hold - a member back from a
// reboot - drops, rather than resets, the packets of connections on those
// addresses it has no trace of (conntrack has followed its own from their
// SYN since it booted), until a reading copies them.
type Guards struct {
	Refuse, Hold []netip.Addr
}

// guardSet is the anonymous set of addrs in family v6, "" for none.
func guardSet(addrs []netip.Addr, v6 bool) string {
	var in []string
	for _, a := range addrs {
		if a.Is6() == v6 {
			in = append(in, a.WithZone("").String())
		}
	}
	if len(in) == 0 {
		return ""
	}
	return "{ " + strings.Join(in, ", ") + " }"
}

// The guards' rule comments: no member name has a colon.
const (
	refuseComment = "janus:refuse"
	holdComment   = "janus:hold"
)

// Document is the nft document that replaces janusd's handoff tables
// with owners' and the guards - none: the tables go. The tables are
// destroyed first, in the same transaction: nothing is ever half there.
// The copies are made before conntrack (priority raw): a connection
// copied away leaves no trace here; the hold comes after it (mangle).
func Document(owners []Owner, g Guards) string {
	var b strings.Builder
	for _, fam := range []string{"ip", "ip6"} {
		fmt.Fprintf(&b, "destroy table %s %s\n", fam, Table)
	}
	owners = slices.Clone(owners)
	slices.SortFunc(owners, func(a, b Owner) int { return strings.Compare(a.Name, b.Name) })
	for _, fam := range []string{"ip", "ip6"} {
		v6 := fam == "ip6"
		var sets, rules strings.Builder
		if set := guardSet(g.Refuse, v6); set != "" {
			fmt.Fprintf(&rules, "\t\t%s daddr %s tcp flags & (syn | ack) == syn counter drop comment %q\n", fam, set, refuseComment)
		}
		for i, o := range owners {
			reach, dev := o.Reach4, o.Dev4
			if v6 {
				reach, dev = o.Reach6, o.Dev6
			}
			var flows []Flow
			for _, f := range o.Flows {
				if f.Is6() == v6 {
					flows = append(flows, f)
				}
			}
			if len(flows) == 0 || !reach.IsValid() || dev == "" {
				continue
			}
			slices.SortFunc(flows, Compare)
			key, addr := "ipv4_addr", "ip"
			if v6 {
				key, addr = "ipv6_addr", "ip6"
			}
			fmt.Fprintf(&sets, "\tset o%d {\n\t\ttype %s . inet_service . %s . inet_service\n\t\telements = {\n", i, key, key)
			for j, f := range flows {
				sep := ","
				if j == len(flows)-1 {
					sep = ""
				}
				fmt.Fprintf(&sets, "\t\t\t%s . %d . %s . %d%s\n", f.Remote.Addr().Unmap(), f.Remote.Port(), f.Local.Addr().Unmap(), f.Local.Port(), sep)
			}
			sets.WriteString("\t\t}\n\t}\n")
			fmt.Fprintf(&rules, "\t\t%s saddr . tcp sport . %s daddr . tcp dport @o%d counter dup to %s device %q drop comment %q\n", addr, addr, i, reach.Unmap().WithZone(""), dev, o.Name)
		}
		hold := guardSet(g.Hold, v6)
		if rules.Len() == 0 && hold == "" {
			continue
		}
		fmt.Fprintf(&b, "table %s %s {\n%s", fam, Table, sets.String())
		if rules.Len() > 0 {
			fmt.Fprintf(&b, "\tchain prerouting {\n\t\ttype filter hook prerouting priority raw; policy accept;\n\t\tmeta l4proto != tcp accept\n%s\t}\n", rules.String())
		}
		if hold != "" {
			fmt.Fprintf(&b, "\tchain hold {\n\t\ttype filter hook prerouting priority mangle; policy accept;\n\t\t%s daddr %s tcp flags & (syn | rst) == 0 ct state { new, invalid } counter drop comment %q\n\t}\n", fam, hold, holdComment)
		}
		b.WriteString("}\n")
	}
	return b.String()
}

// ErrUnavailable is why a node can't hand connections over.
var ErrUnavailable = errors.New("this node can't hand connections over")
