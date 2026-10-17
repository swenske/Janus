package netmgr

import (
	"errors"
	"net"
	"net/netip"

	"github.com/vishvananda/netlink"
)

// EgressInterface is the interface the kernel sends to dst by - from
// src when it's valid and one of this machine's addresses: ip route get
// dst from src. Policy routing and an address moved to another link (a
// VRRP or anycast one) are the kernel's to say, not a guess from which
// interface holds src. Read-only: it works without Options.Manage.
func EgressInterface(dst, src netip.Addr) (string, error) {
	if !dst.IsValid() {
		return "", errors.New("no destination")
	}
	var opt *netlink.RouteGetOptions
	if src.IsValid() && !src.IsUnspecified() {
		opt = &netlink.RouteGetOptions{SrcAddr: net.IP(src.Unmap().AsSlice())}
	}
	routes, err := netlink.RouteGetWithOptions(net.IP(dst.Unmap().AsSlice()), opt)
	if err != nil && opt != nil {
		// src gone since the connection was made: where dst goes now.
		routes, err = netlink.RouteGetWithOptions(net.IP(dst.Unmap().AsSlice()), nil)
	}
	if err != nil {
		return "", err
	}
	for _, r := range routes {
		if r.LinkIndex <= 0 {
			continue
		}
		link, err := netlink.LinkByIndex(r.LinkIndex)
		if err != nil {
			return "", err
		}
		return link.Attrs().Name, nil
	}
	return "", errors.New("no route says")
}
