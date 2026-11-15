package netmgr

import (
	"bytes"
	"encoding/binary"
	"net"
	"net/netip"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func TestAnnounceable(t *testing.T) {
	for p, want := range map[string]bool{
		"192.0.2.11/24":    true,
		"2001:db8::11/64":  true,
		"192.0.2.11/32":    false, // anycast, a service address
		"2001:db8::11/128": false,
		"fe80::1/64":       false,
		"127.0.0.2/8":      false,
	} {
		if got := announceable(netip.MustParsePrefix(p)); got != want {
			t.Errorf("%s: announced %v", p, got)
		}
	}
}

func TestARPAnnouncement(t *testing.T) {
	mac, _ := net.ParseMAC("52:54:00:bb:01:01")
	b := arpAnnouncement(mac, netip.MustParseAddr("192.168.87.2"))
	want := []byte{
		0, 1, 8, 0, 6, 4, 0, 1, // Ethernet, IPv4, request
		0x52, 0x54, 0, 0xbb, 1, 1, 192, 168, 87, 2, // sender: this MAC, the address
		0, 0, 0, 0, 0, 0, 192, 168, 87, 2, // target: no MAC, the address
	}
	if !bytes.Equal(b, want) {
		t.Errorf("ARP announcement\n got %x\nwant %x", b, want)
	}
}

func TestUnsolicitedNA(t *testing.T) {
	mac, _ := net.ParseMAC("52:54:00:bb:01:01")
	ip := netip.MustParseAddr("2001:db8::2")
	b := unsolicitedNA(mac, ip)
	if len(b) != 72 || b[0]>>4 != 6 || binary.BigEndian.Uint16(b[4:]) != 32 || b[6] != unix.IPPROTO_ICMPV6 || b[7] != 255 {
		t.Fatalf("IPv6 header: %x", b[:40])
	}
	if src, _ := netip.AddrFromSlice(b[8:24]); src != ip {
		t.Errorf("source %s", src)
	}
	if dst, _ := netip.AddrFromSlice(b[24:40]); dst != allNodes {
		t.Errorf("destination %s", dst)
	}
	m := b[40:]
	if m[0] != 136 || m[1] != 0 || binary.BigEndian.Uint32(m[4:]) != 1<<29 {
		t.Errorf("type %d code %d flags %x: want a Neighbour Advertisement, override only", m[0], m[1], m[4:8])
	}
	if target, _ := netip.AddrFromSlice(m[8:24]); target != ip {
		t.Errorf("target %s", target)
	}
	if m[24] != 2 || m[25] != 1 || !bytes.Equal(m[26:32], mac) {
		t.Errorf("target link-layer address option %x", m[24:32])
	}
	// The checksum: the sum over the pseudo-header and the message,
	// checksum included, is all ones.
	var sum uint32
	words := append(append(append([]byte{}, b[8:40]...), 0, 0, 0, 32, 0, 0, 0, unix.IPPROTO_ICMPV6), m...)
	for i := 0; i < len(words); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(words[i:]))
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	if sum != 0xffff {
		t.Errorf("checksum %x doesn't verify (%x)", binary.BigEndian.Uint16(m[2:]), sum)
	}
}

// TestAnnounceInNetns adds addresses to a real veth, as an apply does,
// and reads what its peer receives: a gratuitous ARP for the IPv4
// address and an unsolicited Neighbour Advertisement for the IPv6 one,
// both from the interface's MAC - none for a /32. Like TestInNetns, only
// in a throwaway network namespace:
//
//	go test -c -o netmgr.test ./internal/netmgr
//	sudo JANUS_NETNS_TEST=1 unshare -nu ./netmgr.test -test.run TestAnnounceInNetns -test.v
func TestAnnounceInNetns(t *testing.T) {
	if os.Getenv("JANUS_NETNS_TEST") != "1" {
		t.Skip("set JANUS_NETNS_TEST=1 and run under `unshare -nu` as root")
	}
	if links, err := netlink.LinkList(); err != nil || len(links) != 1 {
		t.Fatalf("refusing to run: this isn't a fresh network namespace (%v)", err)
	}
	defer func(e time.Duration) { announceEvery = e }(announceEvery)
	announceEvery = 50 * time.Millisecond
	mac, _ := net.ParseMAC("52:54:00:00:0a:01")
	must(t, netlink.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "ann0", HardwareAddr: mac}, PeerName: "annp0"}))
	link, err := netlink.LinkByName("ann0")
	must(t, err)
	peer, err := netlink.LinkByName("annp0")
	must(t, err)
	must(t, netlink.LinkSetUp(peer))
	must(t, netlink.LinkSetUp(link))

	// What the peer receives, every frame.
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, int(htons(unix.ETH_P_ALL)))
	must(t, err)
	defer unix.Close(fd)
	must(t, unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_ALL), Ifindex: peer.Attrs().Index}))
	tv := unix.NsecToTimeval(int64(200 * time.Millisecond))
	must(t, unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv))

	added, err := syncAddrs(link, []netip.Prefix{netip.MustParsePrefix("192.0.2.11/24"), netip.MustParsePrefix("2001:db8::11/64"), netip.MustParsePrefix("192.0.2.99/32")}, netip.Prefix{})
	must(t, err)
	if len(added) != 3 {
		t.Fatalf("added %v", added)
	}
	announce("ann0", link.Attrs().Index, link.Attrs().HardwareAddr, added)

	var garps, nas int
	var other []string
	deadline := time.Now().Add(5 * time.Second)
	buf := make([]byte, 2048)
	for time.Now().Before(deadline) && (garps < announceRepeats || nas < announceRepeats) {
		n, _, err := unix.Recvfrom(fd, buf, 0)
		if err != nil {
			continue // the timeout: read again
		}
		f := buf[:n]
		if n < 14 || !bytes.Equal(f[6:12], mac) {
			continue
		}
		switch binary.BigEndian.Uint16(f[12:]) {
		case unix.ETH_P_ARP:
			a := f[14:]
			sender, _ := netip.AddrFromSlice(a[14:18])
			target, _ := netip.AddrFromSlice(a[24:28])
			if bytes.Equal(f[0:6], []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}) && sender == target && bytes.Equal(a[8:14], mac) {
				if sender != netip.MustParseAddr("192.0.2.11") {
					other = append(other, "ARP for "+sender.String())
					continue
				}
				garps++
			}
		case unix.ETH_P_IPV6:
			ip := f[14:]
			if len(ip) >= 40+32 && ip[6] == unix.IPPROTO_ICMPV6 && ip[40] == 136 {
				target, _ := netip.AddrFromSlice(ip[48:64])
				if target != netip.MustParseAddr("2001:db8::11") || ip[7] != 255 || binary.BigEndian.Uint32(ip[44:])&(1<<29) == 0 {
					other = append(other, "NA for "+target.String())
					continue
				}
				nas++
			}
		}
	}
	if garps != announceRepeats || nas != announceRepeats || len(other) > 0 {
		t.Errorf("the peer received %d gratuitous ARP and %d unsolicited NA for the addresses (want %d each), and %v", garps, nas, announceRepeats, other)
	}
	if slices.Contains(other, "ARP for 192.0.2.99") {
		t.Error("a /32 was announced")
	}
}
