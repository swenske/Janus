package netmgr

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"time"

	"golang.org/x/sys/unix"
)

// Announcing an address (#515's follow-up): when netmgr adds an address
// to an interface, it tells the segment - a gratuitous ARP for IPv4 (an
// ARP request from the address for the address, RFC 5227's
// announcement) and an unsolicited Neighbour Advertisement with the
// override flag for IPv6 (RFC 4861 7.2.6). A neighbour that already has
// an entry for the address - a router, the cluster's other members -
// takes this node's MAC for it at once, instead of keeping the MAC of
// whatever held the address before until the entry fails: an address
// taken over from a node gone (a recycle keeping a member's address), a
// trial reverting. Linux updates an existing entry on either; neither
// creates one (arp_accept, accept_untracked_na stay off).
//
// Every address netmgr adds is announced, DHCP's included, except a
// single address (/32, /128): an anycast or service address several
// nodes may hold at once (see addAddr) - one node's announcement would
// pull a whole segment's traffic for it to that node. An IPv6 address is
// announced once duplicate address detection is over, never if it
// failed. Three times, a second apart, in the background: an apply
// doesn't wait for it, a failure is logged.

// announceRepeats and announceEvery: how many announcements, how far
// apart (a lost frame, a switch port still learning).
var (
	announceRepeats = 3
	announceEvery   = time.Second
)

// announceable reports whether address p is announced when added.
func announceable(p netip.Prefix) bool {
	return p.Addr().IsGlobalUnicast() && !p.IsSingleIP()
}

// announce tells link index's segment, in the background, that the
// addresses in added are at mac now.
func announce(name string, index int, mac net.HardwareAddr, added []netip.Prefix) {
	var todo []netip.Prefix
	for _, p := range added {
		if announceable(p) {
			todo = append(todo, p)
		}
	}
	if len(todo) == 0 || len(mac) != 6 {
		return
	}
	go func() {
		for _, p := range todo {
			if p.Addr().Is6() {
				if err := waitDAD(index, p, 3*time.Second); err != nil {
					log.Printf("netmgr: %s: not announcing %s: %v", name, p, err)
					continue
				}
			}
			var err error
			for i := range announceRepeats {
				if i > 0 {
					time.Sleep(announceEvery)
				}
				if err = sendAnnouncement(index, mac, p.Addr()); err != nil {
					break
				}
			}
			if err != nil {
				log.Printf("netmgr: %s: announcing %s: %v", name, p, err)
			}
		}
	}()
}

// waitDAD waits until address p on link index is no longer tentative.
func waitDAD(index int, p netip.Prefix, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		have, err := listAddrs(index)
		if err != nil {
			return err
		}
		found := false
		for _, a := range have {
			if a.prefix != p {
				continue
			}
			found = true
			switch {
			case a.flags&unix.IFA_F_DADFAILED != 0:
				return errors.New("duplicate address detection failed: another node holds it")
			case a.flags&unix.IFA_F_TENTATIVE == 0:
				return nil
			}
		}
		if !found {
			return errors.New("gone from the interface meanwhile")
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("still tentative after %s", timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// sendAnnouncement sends one announcement of ip at mac on link index.
func sendAnnouncement(index int, mac net.HardwareAddr, ip netip.Addr) error {
	var proto uint16
	var dst [8]byte
	var frame []byte
	if ip.Is4() {
		proto, dst, frame = unix.ETH_P_ARP, [8]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, arpAnnouncement(mac, ip)
	} else {
		proto, dst, frame = unix.ETH_P_IPV6, [8]byte{0x33, 0x33, 0, 0, 0, 1}, unsolicitedNA(mac, ip)
	}
	// Protocol 0: a socket that only sends - it receives nothing.
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("packet socket: %w", err)
	}
	defer unix.Close(fd)
	to := &unix.SockaddrLinklayer{Protocol: htons(proto), Ifindex: index, Halen: 6, Addr: dst}
	if err := unix.Sendto(fd, frame, 0, to); err != nil {
		return fmt.Errorf("send: %w", err)
	}
	return nil
}

func htons(v uint16) uint16 { return v<<8 | v>>8 }

// arpAnnouncement is the ARP payload announcing ip at mac: a request
// whose sender and target are both ip (RFC 5227 2.3).
func arpAnnouncement(mac net.HardwareAddr, ip netip.Addr) []byte {
	b := make([]byte, 28)
	binary.BigEndian.PutUint16(b[0:], 1)      // Ethernet
	binary.BigEndian.PutUint16(b[2:], 0x0800) // IPv4
	b[4], b[5] = 6, 4
	binary.BigEndian.PutUint16(b[6:], 1) // request
	copy(b[8:14], mac)
	a := ip.As4()
	copy(b[14:18], a[:])
	// target hardware address: zero
	copy(b[24:28], a[:])
	return b
}

// allNodes is ff02::1.
var allNodes = netip.MustParseAddr("ff02::1")

// unsolicitedNA is the IPv6 packet announcing ip at mac: a Neighbour
// Advertisement to all nodes, from ip, override set and solicited not,
// with the target link-layer address option.
func unsolicitedNA(mac net.HardwareAddr, ip netip.Addr) []byte {
	const icmpLen = 32 // 24 + an 8-byte option
	b := make([]byte, 40+icmpLen)
	b[0] = 0x60 // version 6
	binary.BigEndian.PutUint16(b[4:], icmpLen)
	b[6], b[7] = unix.IPPROTO_ICMPV6, 255 // next header, hop limit
	src, dst := ip.As16(), allNodes.As16()
	copy(b[8:24], src[:])
	copy(b[24:40], dst[:])
	m := b[40:]
	m[0] = 136                               // Neighbour Advertisement
	binary.BigEndian.PutUint32(m[4:], 1<<29) // override
	copy(m[8:24], src[:])
	m[24], m[25] = 2, 1 // target link-layer address, 8 bytes
	copy(m[26:32], mac)
	binary.BigEndian.PutUint16(m[2:], icmpv6Checksum(src, dst, m))
	return b
}

// icmpv6Checksum is ICMPv6 message m's checksum (RFC 4443 2.3): the
// one's complement sum over the pseudo-header and m, its checksum field
// zero.
func icmpv6Checksum(src, dst [16]byte, m []byte) uint16 {
	var sum uint32
	add := func(b []byte) {
		for i := 0; i+1 < len(b); i += 2 {
			sum += uint32(binary.BigEndian.Uint16(b[i:]))
		}
		if len(b)%2 == 1 {
			sum += uint32(b[len(b)-1]) << 8
		}
	}
	add(src[:])
	add(dst[:])
	var tail [8]byte
	binary.BigEndian.PutUint32(tail[0:], uint32(len(m)))
	tail[7] = unix.IPPROTO_ICMPV6
	add(tail[:])
	add(m[:2])
	add(m[4:]) // the checksum field left out
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}
