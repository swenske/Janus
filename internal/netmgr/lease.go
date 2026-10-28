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
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// Lease is a DHCP lease on one interface: what janusd's own DHCP client
// (dhcp.go) obtained, or - on a kernel booted with ip=dhcp, which the
// UKI no longer is - what the kernel's client did at boot.
//
// Leases are kept under RunDir, which is tmpfs: a restarted janusd
// (Restart RPC, crash) takes its leases back and renews them when due, a
// reboot starts over.
type Lease struct {
	Interface string   `json:"interface"`
	MAC       string   `json:"mac"` // follows the interface through a rename
	Address   string   `json:"address"`
	Router    string   `json:"router,omitempty"`
	Server    string   `json:"server,omitempty"`
	DNS       []string `json:"dns,omitempty"`
	Domain    string   `json:"domain,omitempty"`
	NTP       []string `json:"ntp,omitempty"`
	// Hostname DHCP provided (option 12). Empty when it gave none.
	Hostname string `json:"hostname,omitempty"`
	// MTU DHCP provided (option 26), applied when the configuration
	// sets none - as the kernel's client did. 0: none.
	MTU int `json:"mtu,omitempty"`

	// When it was obtained, and when it's to be renewed (T1), rebound
	// (T2) and when it expires: time since the boot (CLOCK_BOOTTIME) -
	// a wall clock stepped by NTP after the lease was taken (a node
	// without a battery-backed clock boots in the past) doesn't move
	// them, and the files live under /run, for this boot only. All zero
	// for the kernel's boot lease: the kernel never renewed it, and
	// didn't say.
	Obtained time.Duration `json:"obtained,omitempty"`
	Renew    time.Duration `json:"renew,omitempty"`
	Rebind   time.Duration `json:"rebind,omitempty"`
	Expires  time.Duration `json:"expires,omitempty"`
	// The server's offer and acknowledgement, as received: a renewal is
	// built from them (also after janusd restarted).
	Offer []byte `json:"offer,omitempty"`
	ACK   []byte `json:"ack,omitempty"`
}

// sinceBoot is CLOCK_BOOTTIME: the time since the boot, suspend
// included, whatever the wall clock says. A var for tests.
var sinceBoot = func() time.Duration {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts); err != nil {
		return 0
	}
	return time.Duration(ts.Nano())
}

// kernel reports whether l is the kernel's boot lease (ip=dhcp).
func (l *Lease) kernel() bool { return l.Expires == 0 }

// expired reports whether a lease of janusd's client has run out at now
// (since the boot).
func (l *Lease) expired(now time.Duration) bool {
	return !l.kernel() && now >= l.Expires
}

// wall is t (since the boot) on the wall clock, in Unix seconds.
func wall(t time.Duration) int64 {
	if t == 0 {
		return 0
	}
	return time.Now().Add(t - sinceBoot()).Unix()
}

// same reports whether l and o configure the node alike - a renewal
// that changed nothing but the lease's times needs no re-apply.
func (l *Lease) same(o *Lease) bool {
	if l == nil || o == nil {
		return l == o
	}
	return l.Address == o.Address && l.Router == o.Router && l.Server == o.Server &&
		strings.Join(l.DNS, ",") == strings.Join(o.DNS, ",") && l.Domain == o.Domain &&
		strings.Join(l.NTP, ",") == strings.Join(o.NTP, ",") && l.Hostname == o.Hostname && l.MTU == o.MTU
}

// RunDir holds the leases; a var so tests can point it elsewhere.
var RunDir = "/run/janus"

// Paths the capture reads - vars for the same reason.
var (
	pnpPath        = "/proc/net/pnp"
	ntpServersPath = "/proc/net/ipconfig/ntp_servers"
)

const bootLeaseFile = "boot-lease.json"

// loadOrCaptureBootLease returns the kernel's boot lease captured
// earlier in this boot, or captures it now. nil, nil: the kernel didn't
// configure anything by DHCP (no ip=dhcp - the UKI's command line has
// none -, or no server answered). Kept for a kernel booted with ip=dhcp
// all the same: janusd's client then takes that interface over,
// asking for the same address.
func loadOrCaptureBootLease() (*Lease, error) {
	path := filepath.Join(RunDir, bootLeaseFile)
	if data, err := os.ReadFile(path); err == nil {
		var l Lease
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
		record = &Lease{}
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

func captureBootLease() (*Lease, error) {
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

	l := &Lease{
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
