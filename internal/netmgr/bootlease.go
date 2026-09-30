package netmgr

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// BootLease is what the kernel's own DHCP client (ip=dhcp, IP_PNP)
// obtained at boot. The kernel configures that one interface, installs
// the default route through an ioctl (so with protocol "boot"), and
// exposes the rest in /proc: the resolvers and domain in /proc/net/pnp,
// the NTP servers (option 42) in /proc/net/ipconfig/ntp_servers.
//
// It's captured the first time janusd starts in a boot - before anything
// reconfigures that interface - and kept under RunDir, which is tmpfs: a
// restarted janusd (Restart RPC, crash) finds it again, a reboot starts
// over with a fresh lease. That's what lets ADDRESSING_MODE_DHCP restore
// the lease after a static configuration was tried and reverted.
//
// The kernel never renews the lease; a userspace DHCP client with
// renewal is a planned improvement (docs/architecture.md).
type BootLease struct {
	Interface string   `json:"interface"`
	MAC       string   `json:"mac"` // follows the interface through a rename
	Address   string   `json:"address"`
	Router    string   `json:"router,omitempty"`
	Server    string   `json:"server,omitempty"`
	DNS       []string `json:"dns,omitempty"`
	Domain    string   `json:"domain,omitempty"`
	NTP       []string `json:"ntp,omitempty"`
	// Hostname DHCP provided (option 12). Empty when it gave none - the
	// kernel then names the host after its IP address, which isn't kept.
	Hostname string `json:"hostname,omitempty"`
}

// RunDir holds the captured boot lease; a var so tests can point it
// elsewhere.
var RunDir = "/run/janus"

// Paths the capture reads - vars for the same reason.
var (
	pnpPath        = "/proc/net/pnp"
	ntpServersPath = "/proc/net/ipconfig/ntp_servers"
)

const bootLeaseFile = "boot-lease.json"

// loadOrCaptureBootLease returns the lease captured earlier in this boot,
// or captures it now. nil, nil: the kernel didn't configure anything by
// DHCP (no ip=dhcp, or no server answered).
func loadOrCaptureBootLease() (*BootLease, error) {
	path := filepath.Join(RunDir, bootLeaseFile)
	if data, err := os.ReadFile(path); err == nil {
		var l BootLease
		if err := json.Unmarshal(data, &l); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if l.Interface == "" {
			return nil, nil // captured: no boot DHCP
		}
		return &l, nil
	}

	l, err := captureBootLease()
	if err != nil {
		return nil, err
	}
	record := l
	if record == nil {
		record = &BootLease{}
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(RunDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return nil, err
	}
	return l, nil
}

func captureBootLease() (*BootLease, error) {
	pnp, err := os.ReadFile(pnpPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil // no IP_PNP in this kernel
	}
	if err != nil {
		return nil, err
	}
	info := parsePnp(string(pnp))
	if !info.dhcp {
		return nil, nil
	}

	// The kernel's default route: protocol boot, through the interface it
	// configured.
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Protocol: unix.RTPROT_BOOT}, netlink.RT_FILTER_PROTOCOL)
	if err != nil {
		return nil, fmt.Errorf("list routes: %w", err)
	}
	var link netlink.Link
	var router netip.Addr
	for _, r := range routes {
		if !isDefault(r.Dst) || r.LinkIndex == 0 {
			continue
		}
		if l, err := netlink.LinkByIndex(r.LinkIndex); err == nil {
			link = l
			router, _ = netip.AddrFromSlice(r.Gw.To4())
			break
		}
	}
	if link == nil {
		// DHCP answered without a router: find the one interface holding
		// a global IPv4 address.
		links, err := netlink.LinkList()
		if err != nil {
			return nil, err
		}
		for _, l := range links {
			if a := globalIPv4(l); a.IsValid() {
				link = l
				break
			}
		}
	}
	if link == nil {
		return nil, nil
	}
	addr := globalIPv4(link)
	if !addr.IsValid() {
		return nil, nil
	}

	l := &BootLease{
		Interface: link.Attrs().Name,
		MAC:       link.Attrs().HardwareAddr.String(),
		Address:   addr.String(),
		Server:    info.server,
		DNS:       info.nameservers,
		Domain:    info.domain,
	}
	if router.IsValid() {
		l.Router = router.String()
	}
	if data, err := os.ReadFile(ntpServersPath); err == nil {
		for _, f := range strings.Fields(string(data)) {
			if a, err := netip.ParseAddr(f); err == nil && !a.IsUnspecified() {
				l.NTP = append(l.NTP, a.String())
			}
		}
	}
	// Without option 12 the kernel sets the hostname to the address
	// itself ("%pI4") - that's not a name DHCP gave.
	if h, err := os.Hostname(); err == nil && h != "" && h != "(none)" && h != addr.Addr().String() {
		l.Hostname = h
	}
	return l, nil
}

type pnpInfo struct {
	dhcp        bool
	domain      string
	server      string
	nameservers []string
}

// parsePnp reads /proc/net/pnp: "#PROTO: DHCP", "domain x",
// "nameserver a.b.c.d" (up to three), "bootserver a.b.c.d".
func parsePnp(s string) pnpInfo {
	var info pnpInfo
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		switch {
		case len(f) == 2 && f[0] == "#PROTO:":
			info.dhcp = f[1] == "DHCP"
		case len(f) == 2 && f[0] == "domain":
			info.domain = f[1]
		case len(f) == 2 && f[0] == "nameserver":
			if a, err := netip.ParseAddr(f[1]); err == nil && !a.IsUnspecified() {
				info.nameservers = append(info.nameservers, a.String())
			}
		case len(f) == 2 && f[0] == "bootserver":
			info.server = f[1]
		}
	}
	return info
}

// isDefault reports whether dst is a default route's destination: nil,
// 0.0.0.0/0 or ::/0 depending on the netlink version and family.
func isDefault(dst *net.IPNet) bool {
	if dst == nil {
		return true
	}
	ones, _ := dst.Mask.Size()
	return ones == 0 && dst.IP.IsUnspecified()
}

// globalIPv4 is link's first global IPv4 address, as a prefix.
func globalIPv4(link netlink.Link) netip.Prefix {
	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return netip.Prefix{}
	}
	for _, a := range addrs {
		p, ok := prefixOf(a.IPNet)
		if ok && p.Addr().IsGlobalUnicast() {
			return p
		}
	}
	return netip.Prefix{}
}

func prefixOf(n *net.IPNet) (netip.Prefix, bool) {
	if n == nil {
		return netip.Prefix{}, false
	}
	a, ok := netip.AddrFromSlice(n.IP)
	if !ok {
		return netip.Prefix{}, false
	}
	a = a.Unmap()
	ones, bits := n.Mask.Size()
	if bits == 0 {
		return netip.Prefix{}, false
	}
	return netip.PrefixFrom(a, ones), true
}
