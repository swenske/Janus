// Package netmgr applies the node's network configuration
// (internal/netconfig) to the running system over rtnetlink - links,
// 802.1Q VLANs, MTUs, static addresses, default routes - plus the
// hostname and /etc/resolv.conf, and runs a new configuration on trial:
// in effect at once, kept only if confirmed in time, reverted otherwise.
//
// DHCP is janusd's own client (dhcp.go), on the interface in DHCP mode -
// or, with nothing configured, on every physical interface until one
// gets a lease: it renews the lease and starts over when it's lost. The
// kernel does no DHCP at boot any more (the UKI's command line has no
// ip=dhcp): a node configured statically comes up at once.
package netmgr

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/events"
	"github.com/swenske/Janus/internal/netconfig"
)

// Options configures a Manager.
type Options struct {
	// Manage enables changing the system at all. Off, the Manager only
	// reports: janusd runs natively on a developer's or CI machine in
	// several tests, and in a container for local-dev, where touching the
	// host's network would be a disaster. rootfs/init turns it on for a
	// real node (janusd -manage-host).
	Manage bool
	// ResolvConfPath is /etc/resolv.conf.
	ResolvConfPath string
	// OnChange is called, outside any lock, after the configuration in
	// effect changed (applied, reverted) - the node's addresses or
	// hostname may have too.
	OnChange func()
}

var (
	ErrNotManaged   = errors.New("this janusd doesn't manage the network (it runs without -manage-host: not a Janus node)")
	ErrTrialPending = errors.New("a network configuration is already on trial - confirm it, or wait for it to revert")
	ErrNoTrial      = errors.New("no network configuration is on trial")
)

// ConflictError is a valid configuration that doesn't fit this node - an
// interface it names doesn't exist.
type ConflictError struct{ Err error }

func (e *ConflictError) Error() string { return e.Err.Error() }
func (e *ConflictError) Unwrap() error { return e.Err }

// InvalidError is a configuration refused as such (see netconfig.Validate).
type InvalidError struct{ Err error }

func (e *InvalidError) Error() string { return e.Err.Error() }
func (e *InvalidError) Unwrap() error { return e.Err }

// Manager owns the node's network configuration.
type Manager struct {
	opts Options

	mu        sync.Mutex
	cfg       *janusv1alpha1.NetworkConfig // in effect: persisted, or on trial
	isDefault bool                         // nothing was ever persisted
	// kernelLease is the kernel's boot lease, on a kernel booted with
	// ip=dhcp (the UKI isn't any more): used until janusd's client has
	// its own on that interface.
	kernelLease *Lease
	leases      map[string]*Lease                       // janusd's client's, by MAC
	clients     map[string]*dhcpRun                     // running, by MAC
	modes       map[string]janusv1alpha1.AddressingMode // by interface, from the last applied plan
	dhcp        string                                  // the DHCP interface (plan.dhcp), from the last applied plan
	dhcpLease   *Lease                                  // its lease, if it has one
	waiting     bool                                    // an interface in DHCP mode has no lease yet
	main        []netip.Addr                            // the main interface's addresses (mainAddresses), from the last applied plan
	trial       *trial
}

// dhcpRun is one interface's running client.
type dhcpRun struct {
	mac    string
	cancel context.CancelFunc
}

type trial struct {
	prev          *janusv1alpha1.NetworkConfig
	prevIsDefault bool
	names         map[string]string // physical interface names by MAC, before the trial
	revertAt      time.Time
	timer         *time.Timer
}

// New loads the persisted configuration and, when managing, the boot
// lease. A stored configuration that doesn't parse is logged and
// replaced by the defaults for this boot - a node must stay reachable.
func New(opts Options) *Manager {
	if opts.ResolvConfPath == "" {
		opts.ResolvConfPath = "/etc/resolv.conf"
	}
	m := &Manager{opts: opts, modes: map[string]janusv1alpha1.AddressingMode{}, leases: map[string]*Lease{}, clients: map[string]*dhcpRun{}}
	cfg, isDefault, err := netconfig.Load()
	if err != nil {
		log.Printf("netmgr: %v - using the default configuration this boot", err)
		cfg, isDefault = &janusv1alpha1.NetworkConfig{}, true
	}
	m.cfg, m.isDefault = cfg, isDefault
	if opts.Manage {
		if m.kernelLease, err = loadOrCaptureBootLease(); err != nil {
			log.Printf("netmgr: the kernel's boot DHCP lease: %v", err)
		}
		m.leases = loadLeases(sinceBoot())
	}
	return m
}

// Start applies the configuration in effect. If it can't be (an
// interface it names is gone), the defaults are applied instead - DHCP -
// and the error is returned for logging. When an interface is in DHCP
// mode, Start waits for its lease (DHCPBootWait at most): what follows
// at boot sees the node's address.
func (m *Manager) Start() error {
	if !m.opts.Manage {
		return nil
	}
	if err := loopbackUp(); err != nil {
		log.Printf("netmgr: %v", err)
	}
	m.mu.Lock()
	err := m.applyLocked(m.cfg)
	if err != nil {
		m.cfg = &janusv1alpha1.NetworkConfig{}
		if derr := m.applyLocked(m.cfg); derr != nil {
			log.Printf("netmgr: default configuration: %v", derr)
		}
	}
	m.mu.Unlock()
	m.changed()

	start := time.Now()
	for {
		m.mu.Lock()
		waiting := m.waiting
		m.mu.Unlock()
		if !waiting {
			break
		}
		if time.Since(start) >= DHCPBootWait {
			log.Printf("netmgr: no DHCP lease within %s - going on, the client keeps asking", DHCPBootWait)
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	return err
}

// loopbackUp brings lo up. The kernel's ip=dhcp did it at boot, before
// init - every node had 127.0.0.1 and ::1 without asking: HAProxy's
// local peer (the stick tables a reload hands over, 127.0.0.1:10057),
// the extensions' local listeners. Nothing else does now.
func loopbackUp() error {
	lo, err := netlink.LinkByName("lo")
	if err != nil {
		return fmt.Errorf("loopback: %w", err)
	}
	if lo.Attrs().Flags&net.FlagUp != 0 {
		return nil
	}
	if err := netlink.LinkSetUp(lo); err != nil {
		return fmt.Errorf("set lo up: %w", err)
	}
	return nil
}

// leaseSetLocked is what buildPlan knows of DHCP now. Called with mu
// held.
func (m *Manager) leaseSetLocked() leaseSet {
	ls := leaseSet{byMAC: map[string]*Lease{}}
	now := sinceBoot()
	for mac, l := range m.leases {
		if !l.expired(now) {
			ls.byMAC[mac] = l
		}
	}
	if k := m.kernelLease; k != nil {
		ls.kernel, _ = netip.ParsePrefix(k.Address)
		if ls.byMAC[k.MAC] == nil {
			ls.byMAC[k.MAC] = k
		}
	}
	return ls
}

// Managed reports whether this Manager changes the system.
func (m *Manager) Managed() bool { return m.opts.Manage }

// Config returns the configuration in effect (on trial, if one is) and
// whether it's the untouched default.
func (m *Manager) Config() (*janusv1alpha1.NetworkConfig, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg, m.isDefault && m.trial == nil
}

// DHCPNTPServers returns the NTP servers the DHCP interface's lease
// provided.
func (m *Manager) DHCPNTPServers() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.dhcpLease == nil {
		return nil
	}
	return append([]string(nil), m.dhcpLease.NTP...)
}

// ApplyTrial puts cfg in effect on trial: it reverts to the previous
// configuration at the returned time unless Confirm is called first.
// Nothing is touched if cfg is invalid or can't apply to this node's
// interfaces. Returns the node's global addresses under cfg.
func (m *Manager) ApplyTrial(cfg *janusv1alpha1.NetworkConfig, timeout time.Duration) ([]string, time.Time, error) {
	if !m.opts.Manage {
		return nil, time.Time{}, ErrNotManaged
	}
	if timeout == 0 {
		timeout = netconfig.DefaultConfirmTimeout
	}
	if timeout < 5*time.Second || timeout > netconfig.MaxConfirmTimeout {
		return nil, time.Time{}, &InvalidError{fmt.Errorf("confirm timeout must be between 5s and %s", netconfig.MaxConfirmTimeout)}
	}
	if err := netconfig.Validate(cfg); err != nil {
		return nil, time.Time{}, &InvalidError{err}
	}

	m.mu.Lock()
	if m.trial != nil {
		m.mu.Unlock()
		return nil, time.Time{}, ErrTrialPending
	}
	// Dry run first: a configuration naming a missing interface fails
	// here, with nothing changed.
	links, err := listLinks()
	if err != nil {
		m.mu.Unlock()
		return nil, time.Time{}, err
	}
	if _, err := buildPlan(cfg, links, m.leaseSetLocked()); err != nil {
		m.mu.Unlock()
		return nil, time.Time{}, &ConflictError{err}
	}
	prev, prevIsDefault := m.cfg, m.isDefault
	names := namesByMAC(links)
	if err := m.applyLocked(cfg); err != nil {
		if rerr := m.restoreLocked(names, prev); rerr != nil {
			log.Printf("netmgr: restore the previous configuration after a failed apply: %v", rerr)
		}
		m.mu.Unlock()
		m.changed()
		return nil, time.Time{}, fmt.Errorf("apply: %w (the previous configuration was restored)", err)
	}
	m.cfg, m.isDefault = cfg, false
	revertAt := time.Now().Add(timeout)
	m.trial = &trial{prev: prev, prevIsDefault: prevIsDefault, names: names, revertAt: revertAt, timer: time.AfterFunc(timeout, m.revertTrial)}
	m.mu.Unlock()

	events.Publish("network.trial", map[string]any{"revert_at_unix": revertAt.Unix()})
	log.Printf("netmgr: new network configuration on trial, reverting at %s unless confirmed", revertAt.Format(time.RFC3339))
	m.changed()
	addrs, _ := globalAddresses()
	return addrs, revertAt, nil
}

func (m *Manager) revertTrial() {
	m.mu.Lock()
	t := m.trial
	if t == nil {
		m.mu.Unlock()
		return
	}
	m.trial = nil
	err := m.restoreLocked(t.names, t.prev)
	m.cfg, m.isDefault = t.prev, t.prevIsDefault
	m.mu.Unlock()
	if err != nil {
		log.Printf("netmgr: revert: %v", err)
	}
	log.Printf("netmgr: network configuration not confirmed in time - reverted to the previous one")
	events.Publish("network.reverted", nil)
	m.changed()
}

// Confirm keeps the configuration on trial and persists it. local is the
// node-side address of the connection the confirmation came in on: it
// must be one of the node's addresses now, and not loopback - proof that
// the node is reachable under the new configuration.
func (m *Manager) Confirm(local netip.Addr) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.trial == nil {
		return "", ErrNoTrial
	}
	local = local.Unmap()
	if !local.IsValid() || local.IsLoopback() {
		return "", fmt.Errorf("confirm over one of the node's network addresses, not %s", local)
	}
	addrs, err := globalAddresses()
	if err != nil {
		return "", err
	}
	found := false
	for _, a := range addrs {
		if p, err := netip.ParsePrefix(a); err == nil && p.Addr() == local {
			found = true
		}
	}
	if !found {
		return "", fmt.Errorf("%s isn't one of the node's addresses under the new configuration", local)
	}
	if err := netconfig.Save(m.cfg); err != nil {
		return "", fmt.Errorf("persist the configuration: %w", err)
	}
	m.trial.timer.Stop()
	m.trial = nil
	events.Publish("network.confirmed", map[string]string{"via": local.String()})
	log.Printf("netmgr: network configuration confirmed via %s and saved", local)
	return local.String(), nil
}

// Trial reports whether a configuration is on trial, and when it reverts.
func (m *Manager) Trial() (bool, time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.trial == nil {
		return false, time.Time{}
	}
	return true, m.trial.revertAt
}

func (m *Manager) changed() {
	if m.opts.OnChange != nil {
		m.opts.OnChange()
	}
}

// restoreLocked puts the physical interfaces' names back as they were
// (a trial may have renamed some by MAC, and the previous configuration
// refers to the old names), then applies prev. Called with mu held.
func (m *Manager) restoreLocked(names map[string]string, prev *janusv1alpha1.NetworkConfig) error {
	links, err := listLinks()
	if err != nil {
		return err
	}
	p := &plan{}
	for _, l := range links {
		if want, ok := names[l.mac]; ok && l.kind == kindPhysical && l.name != want {
			p.renames = append(p.renames, rename{from: l.name, to: want})
		}
	}
	if len(p.renames) > 0 {
		if err := execute(p); err != nil {
			return fmt.Errorf("restore interface names: %w", err)
		}
	}
	return m.applyLocked(prev)
}

func namesByMAC(links []linkInfo) map[string]string {
	names := map[string]string{}
	for _, l := range links {
		if l.kind == kindPhysical && l.mac != "" {
			names[l.mac] = l.name
		}
	}
	return names
}

// applyLocked makes the system match cfg. Called with mu held.
func (m *Manager) applyLocked(cfg *janusv1alpha1.NetworkConfig) error {
	links, err := listLinks()
	if err != nil {
		return err
	}
	ls := m.leaseSetLocked()
	p, err := buildPlan(cfg, links, ls)
	if err != nil {
		return err
	}
	if err := execute(p); err != nil {
		return err
	}
	m.syncClientsLocked(p, ls)
	m.modes = map[string]janusv1alpha1.AddressingMode{}
	for _, ip := range p.ifaces {
		m.modes[ip.name] = ip.mode
	}
	for _, l := range p.down {
		m.modes[l.name] = janusv1alpha1.AddressingMode_ADDRESSING_MODE_DISABLED
	}
	m.dhcp, m.dhcpLease, m.waiting = p.dhcp, nil, false
	for _, ip := range p.ifaces {
		if ip.mode != janusv1alpha1.AddressingMode_ADDRESSING_MODE_DHCP || ip.existing == nil {
			continue
		}
		lease := ls.byMAC[ip.existing.mac]
		if ip.name == p.dhcp {
			m.dhcpLease = lease
		}
		if lease == nil && (p.dhcp == "" || ip.name == p.dhcp) {
			m.waiting = true
		}
	}
	m.main = mainAddresses(p)
	return m.applyHostAndDNSLocked(cfg, links)
}

// MainAddresses returns the addresses of the node's main interface in
// the configuration in effect (mainAddresses) - what the exporters
// listen on by default. None when not managing the host's network.
func (m *Manager) MainAddresses() []netip.Addr {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.main)
}

// applyHostAndDNSLocked sets the hostname and writes resolv.conf: the
// configured values, else the DHCP interface's lease's, else defaults.
func (m *Manager) applyHostAndDNSLocked(cfg *janusv1alpha1.NetworkConfig, links []linkInfo) error {
	lease := m.dhcpLease
	host := cfg.GetHostname()
	if host == "" && lease != nil {
		host = lease.Hostname
	}
	if host == "" {
		host = netconfig.DefaultHostname(firstMAC(links))
	}
	if cur, _ := os.Hostname(); cur != host {
		if err := unix.Sethostname([]byte(host)); err != nil {
			return fmt.Errorf("set hostname %s: %w", host, err)
		}
		log.Printf("netmgr: hostname %s", host)
	}

	var dhcpDNS, dhcpSearch []string
	if lease != nil {
		dhcpDNS = lease.DNS
		if lease.Domain != "" {
			dhcpSearch = []string{lease.Domain}
		}
	}
	servers, search := netconfig.EffectiveDNS(cfg, dhcpDNS, dhcpSearch)
	return writeIfChanged(m.opts.ResolvConfPath, netconfig.ResolvConf(servers, search))
}

func firstMAC(links []linkInfo) net.HardwareAddr {
	idx := -1
	var mac string
	for _, l := range links {
		if l.kind == kindPhysical && l.mac != "" && (idx < 0 || l.index < idx) {
			idx, mac = l.index, l.mac
		}
	}
	hw, _ := net.ParseMAC(mac)
	return hw
}

// writeIfChanged replaces path atomically when its content differs.
func writeIfChanged(path string, data []byte) error {
	if cur, err := os.ReadFile(path); err == nil && string(cur) == string(data) {
		return nil
	}
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".janusd")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// --- executing a plan ---

func execute(p *plan) error {
	for _, l := range p.deleteVLANs {
		if link, err := netlink.LinkByName(l.name); err == nil {
			if err := netlink.LinkDel(link); err != nil {
				return fmt.Errorf("delete VLAN %s: %w", l.name, err)
			}
		}
	}

	// Renames go through temporary names, so swapping two names works.
	for i, r := range p.renames {
		link, err := netlink.LinkByName(r.from)
		if err != nil {
			return fmt.Errorf("rename %s: %w", r.from, err)
		}
		if err := netlink.LinkSetDown(link); err != nil {
			return fmt.Errorf("rename %s: %w", r.from, err)
		}
		if err := netlink.LinkSetName(link, fmt.Sprintf("jnrn%d", i)); err != nil {
			return fmt.Errorf("rename %s: %w", r.from, err)
		}
	}
	for i, r := range p.renames {
		link, err := netlink.LinkByName(fmt.Sprintf("jnrn%d", i))
		if err != nil {
			return fmt.Errorf("rename %s: %w", r.from, err)
		}
		if err := netlink.LinkSetName(link, r.to); err != nil {
			return fmt.Errorf("rename %s to %s: %w", r.from, r.to, err)
		}
	}

	for _, l := range p.down {
		link, err := netlink.LinkByIndex(l.index)
		if err != nil {
			continue // gone meanwhile
		}
		if err := flush(link, p.lease); err != nil {
			return fmt.Errorf("%s: %w", l.name, err)
		}
		if err := netlink.LinkSetDown(link); err != nil {
			return fmt.Errorf("set %s down: %w", l.name, err)
		}
	}

	for _, ip := range p.ifaces {
		if err := executeIface(ip, p.lease); err != nil {
			return fmt.Errorf("%s: %w", ip.name, err)
		}
	}
	return nil
}

func executeIface(ip ifPlan, lease netip.Prefix) error {
	var link netlink.Link
	var err error
	if ip.existing == nil { // a VLAN to create
		parent, err := netlink.LinkByName(ip.vlanParent)
		if err != nil {
			return fmt.Errorf("VLAN parent %s: %w", ip.vlanParent, err)
		}
		if err := netlink.LinkSetUp(parent); err != nil {
			return fmt.Errorf("set VLAN parent %s up: %w", ip.vlanParent, err)
		}
		vlan := &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: ip.name, ParentIndex: parent.Attrs().Index}, VlanId: ip.vlanID}
		if err := netlink.LinkAdd(vlan); err != nil {
			return fmt.Errorf("create VLAN %d on %s: %w", ip.vlanID, ip.vlanParent, err)
		}
	}
	if link, err = netlink.LinkByName(ip.name); err != nil {
		return err
	}
	if ip.mtu > 0 && link.Attrs().MTU != ip.mtu {
		if err := netlink.LinkSetMTU(link, ip.mtu); err != nil {
			return fmt.Errorf("set MTU %d: %w", ip.mtu, err)
		}
	}
	if ip.mode == janusv1alpha1.AddressingMode_ADDRESSING_MODE_DISABLED {
		if err := flush(link, lease); err != nil {
			return err
		}
		return netlink.LinkSetDown(link)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("set up: %w", err)
	}
	if err := syncAddrs(link, ip.addrs, lease); err != nil {
		return err
	}
	proto := netlink.RouteProtocol(unix.RTPROT_STATIC)
	if ip.mode == janusv1alpha1.AddressingMode_ADDRESSING_MODE_DHCP {
		proto = unix.RTPROT_BOOT // what the kernel's boot DHCP used
	}
	var want []netlink.Route
	for _, gw := range []netip.Addr{ip.gw4, ip.gw6} {
		if !gw.IsValid() {
			continue
		}
		family := netlink.FAMILY_V4
		if gw.Is6() {
			family = netlink.FAMILY_V6
		}
		want = append(want, netlink.Route{LinkIndex: link.Attrs().Index, Gw: net.IP(gw.AsSlice()), Priority: ip.metric, Protocol: proto, Family: family})
	}
	return syncRoutes(link, want)
}

// ownProto is the IFA_PROTO value netmgr gives the addresses it adds
// (Linux 6.3+): only those - and the kernel's boot DHCP address - are its
// to take away. Another daemon's addresses on the same interface stay:
// keepalived's virtual ones were removed by every apply, the VIP gone
// from the master until keepalived noticed and took it back.
const ownProto = 0x4a

// ifaProto is IFA_PROTO, which golang.org/x/sys doesn't name (IFA_MAX).
const ifaProto = 0xb

// infinite is an address lifetime that never ends (INFINITY_LIFE_TIME):
// the address stays permanent.
const infinite = 0xffffffff

// addr is an address on a link, with what syncAddrs needs of it.
type addr struct {
	prefix netip.Prefix
	flags  uint32
	proto  uint8
}

// managed reports whether an address is one janusd adds and removes:
// global IPv4 addresses, and permanent (not SLAAC) global IPv6 ones.
// Link-local and kernel-autoconfigured addresses are left alone.
func managed(a addr) bool {
	if !a.prefix.Addr().IsGlobalUnicast() {
		return false
	}
	return a.prefix.Addr().Is4() || a.flags&unix.IFA_F_PERMANENT != 0
}

// syncAddrs adds what's missing before removing what's no longer wanted,
// so an address change doesn't drop connectivity in between. It removes
// only its own addresses (ownProto) and lease, the boot DHCP address.
func syncAddrs(link netlink.Link, want []netip.Prefix, lease netip.Prefix) error {
	index := link.Attrs().Index
	have, err := listAddrs(index)
	if err != nil {
		return err
	}
	present := map[netip.Prefix]bool{}
	for _, a := range have {
		if managed(a) {
			present[a.prefix] = true
		}
	}
	wanted := map[netip.Prefix]bool{}
	for _, p := range want {
		wanted[p] = true
		if present[p] {
			continue
		}
		if err := addAddr(index, p); err != nil && !errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("add address %s: %w", p, err)
		}
	}
	removed := false
	for _, a := range have {
		if !managed(a) || wanted[a.prefix] || (a.proto != ownProto && a.prefix != lease) {
			continue
		}
		del := &netlink.Addr{IPNet: &net.IPNet{IP: net.IP(a.prefix.Addr().AsSlice()), Mask: net.CIDRMask(a.prefix.Bits(), a.prefix.Addr().BitLen())}}
		if err := netlink.AddrDel(link, del); err != nil && !errors.Is(err, unix.EADDRNOTAVAIL) {
			return fmt.Errorf("remove address %s: %w", a.prefix, err)
		}
		removed = true
	}
	if !removed {
		return nil
	}
	// Removing an interface's primary IPv4 address deletes the secondary
	// ones of its subnet along with it, unless promote_secondaries is on
	// (rootfs/init turns it on): a new address in the old one's subnet
	// would be gone. Put back whatever is missing.
	if have, err = listAddrs(index); err != nil {
		return err
	}
	present = map[netip.Prefix]bool{}
	for _, a := range have {
		if managed(a) {
			present[a.prefix] = true
		}
	}
	for _, p := range want {
		if present[p] {
			continue
		}
		if err := addAddr(index, p); err != nil && !errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("add address %s: %w", p, err)
		}
	}
	return nil
}

// addAddr adds p to link index as netmgr's (ownProto). A single IPv6
// address (a /128) is an anycast or service address several nodes hold
// at once: it skips duplicate address detection - with DAD, every node
// but the first to add it on a segment had it "dadfailed", unusable -,
// and it's deprecated from the start (preferred lifetime 0, valid
// forever): still answered on, never picked as the source of the node's
// own connections. As a preferred address, Linux took it as the source
// of every outgoing IPv6 connection: replies came back to whichever node
// the router's ECMP chose, or nowhere before the route was announced. An
// IPv4 /32 needs neither - Linux picks the address on the gateway's
// subnet.
func addAddr(index int, p netip.Prefix) error {
	family := unix.AF_INET
	if p.Addr().Is6() {
		family = unix.AF_INET6
	}
	req := nl.NewNetlinkRequest(unix.RTM_NEWADDR, unix.NLM_F_CREATE|unix.NLM_F_EXCL|unix.NLM_F_ACK)
	msg := nl.NewIfAddrmsg(family)
	msg.Index = uint32(index)
	msg.Prefixlen = uint8(p.Bits())
	req.AddData(msg)
	ip := p.Addr().AsSlice()
	req.AddData(nl.NewRtAttr(unix.IFA_LOCAL, ip))
	req.AddData(nl.NewRtAttr(unix.IFA_ADDRESS, ip))
	if p.Addr().Is4() && p.Bits() < 31 {
		// The broadcast address, as netlink.AddrAdd sets it (RFC 3021).
		mask := net.CIDRMask(p.Bits(), 32)
		bcast := make(net.IP, 4)
		for i := range bcast {
			bcast[i] = ip[i] | ^mask[i]
		}
		req.AddData(nl.NewRtAttr(unix.IFA_BROADCAST, bcast))
	}
	if p.Addr().Is6() && p.IsSingleIP() {
		flags := make([]byte, 4)
		nl.NativeEndian().PutUint32(flags, unix.IFA_F_NODAD)
		req.AddData(nl.NewRtAttr(unix.IFA_FLAGS, flags))
		life := nl.IfaCacheInfo{IfaCacheinfo: unix.IfaCacheinfo{Valid: infinite}} // preferred: 0
		req.AddData(nl.NewRtAttr(unix.IFA_CACHEINFO, life.Serialize()))
	}
	req.AddData(nl.NewRtAttr(ifaProto, []byte{ownProto}))
	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	return err
}

// listAddrs lists link index's addresses with their flags and protocol -
// which netlink.AddrList doesn't read.
func listAddrs(index int) ([]addr, error) {
	var msgs [][]byte
	var err error
	for range 3 { // a dump interrupted by a change meanwhile is redone
		req := nl.NewNetlinkRequest(unix.RTM_GETADDR, unix.NLM_F_DUMP)
		req.AddData(nl.NewIfAddrmsg(unix.AF_UNSPEC))
		if msgs, err = req.Execute(unix.NETLINK_ROUTE, unix.RTM_NEWADDR); !errors.Is(err, nl.ErrDumpInterrupted) {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("list addresses: %w", err)
	}
	var out []addr
	for _, m := range msgs {
		msg := nl.DeserializeIfAddrmsg(m)
		if int(msg.Index) != index {
			continue
		}
		attrs, err := nl.ParseRouteAttr(m[msg.Len():])
		if err != nil {
			return nil, fmt.Errorf("list addresses: %w", err)
		}
		a := addr{flags: uint32(msg.Flags)}
		var local, address []byte
		for _, at := range attrs {
			switch at.Attr.Type {
			case unix.IFA_LOCAL:
				local = at.Value
			case unix.IFA_ADDRESS:
				address = at.Value
			case unix.IFA_FLAGS:
				if len(at.Value) == 4 {
					a.flags = nl.NativeEndian().Uint32(at.Value)
				}
			case ifaProto:
				if len(at.Value) > 0 {
					a.proto = at.Value[0]
				}
			}
		}
		if local == nil { // IPv6 sends the address as IFA_ADDRESS alone
			local = address
		}
		ip, ok := netip.AddrFromSlice(local)
		if !ok {
			continue
		}
		a.prefix = netip.PrefixFrom(ip.Unmap(), int(msg.Prefixlen))
		out = append(out, a)
	}
	return out, nil
}

// managedRoutes are a link's default routes installed by janusd (static,
// and boot for a DHCP lease's, as the kernel's boot DHCP installed
// them); router-advertised and other routes are left alone.
func managedRoutes(link netlink.Link) ([]netlink.Route, error) {
	var out []netlink.Route
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		routes, err := netlink.RouteListFiltered(family, &netlink.Route{LinkIndex: link.Attrs().Index}, netlink.RT_FILTER_OIF)
		if err != nil {
			return nil, fmt.Errorf("list routes: %w", err)
		}
		for _, r := range routes {
			if isDefault(r.Dst) && (r.Protocol == unix.RTPROT_STATIC || r.Protocol == unix.RTPROT_BOOT) {
				out = append(out, r)
			}
		}
	}
	return out, nil
}

// syncRoutes installs the wanted default routes, then removes the other
// managed ones.
func syncRoutes(link netlink.Link, want []netlink.Route) error {
	for i := range want {
		if err := netlink.RouteReplace(&want[i]); err != nil {
			return fmt.Errorf("default route via %s: %w", want[i].Gw, err)
		}
	}
	have, err := managedRoutes(link)
	if err != nil {
		return err
	}
	for _, r := range have {
		keep := false
		for _, w := range want {
			if r.Gw.Equal(w.Gw) && r.Priority == w.Priority && r.Protocol == w.Protocol {
				keep = true
			}
		}
		if !keep {
			if err := netlink.RouteDel(&r); err != nil && !errors.Is(err, unix.ESRCH) {
				return fmt.Errorf("remove default route via %s: %w", r.Gw, err)
			}
		}
	}
	return nil
}

func flush(link netlink.Link, lease netip.Prefix) error {
	if err := syncRoutes(link, nil); err != nil {
		return err
	}
	return syncAddrs(link, nil, lease)
}

// --- reading the system ---

// physicalTypes are the link types treated as physical NICs: real
// Ethernet devices ("device"). Tests running in a throwaway network
// namespace add "veth".
var physicalTypes = map[string]bool{"device": true}

func listLinks() ([]linkInfo, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return nil, fmt.Errorf("list interfaces: %w", err)
	}
	out := make([]linkInfo, 0, len(links))
	for _, l := range links {
		a := l.Attrs()
		li := linkInfo{name: a.Name, index: a.Index, mac: a.HardwareAddr.String(), up: a.Flags&net.FlagUp != 0, mtu: a.MTU, parent: a.ParentIndex}
		switch {
		case a.Flags&net.FlagLoopback != 0:
			li.kind = kindLoopback
		case l.Type() == "vlan":
			li.kind = kindVLAN
			if v, ok := l.(*netlink.Vlan); ok {
				li.vlanID = v.VlanId
			}
		case physicalTypes[l.Type()] && a.EncapType == "ether":
			li.kind = kindPhysical
		default:
			li.kind = kindOther
		}
		out = append(out, li)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].index < out[j].index })
	return out, nil
}

// globalAddresses lists every global address on the node, in CIDR form.
func globalAddresses() ([]string, error) {
	addrs, err := netlink.AddrList(nil, netlink.FAMILY_ALL)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, a := range addrs {
		if p, ok := prefixOf(a.IPNet); ok && p.Addr().IsGlobalUnicast() {
			out = append(out, p.String())
		}
	}
	return out, nil
}

// Status reports what's in effect - also when not managing, read-only.
func (m *Manager) Status() (*janusv1alpha1.NetworkStatusResponse, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	modes := m.modes
	dhcp, lease := m.dhcp, m.dhcpLease
	trialPending, revertAt := m.trial != nil, time.Time{}
	if m.trial != nil {
		revertAt = m.trial.revertAt
	}
	m.mu.Unlock()

	resp := &janusv1alpha1.NetworkStatusResponse{TrialPending: trialPending, Managed: m.opts.Manage}
	if trialPending {
		resp.TrialRevertAtUnix = revertAt.Unix()
	}
	resp.Hostname, _ = os.Hostname()
	infos, _ := listLinks()
	byIndex := map[int]linkInfo{}
	for _, li := range infos {
		byIndex[li.index] = li
	}
	sort.Slice(links, func(i, j int) bool { return links[i].Attrs().Index < links[j].Attrs().Index })
	for _, l := range links {
		a := l.Attrs()
		li := byIndex[a.Index]
		s := &janusv1alpha1.NetworkInterfaceStatus{
			Name: a.Name, Kind: li.kind, Mac: a.HardwareAddr.String(), Mtu: uint32(a.MTU),
			Up: a.Flags&net.FlagUp != 0, Carrier: a.OperState == netlink.OperUp || a.Flags&net.FlagLoopback != 0,
			Mode: modes[a.Name],
		}
		if li.kind == kindVLAN {
			s.VlanId = uint32(li.vlanID)
			if p, ok := byIndex[li.parent]; ok {
				s.VlanParent = p.name
			}
		}
		if addrs, err := netlink.AddrList(l, netlink.FAMILY_ALL); err == nil {
			for _, ad := range addrs {
				if p, ok := prefixOf(ad.IPNet); ok {
					s.Addresses = append(s.Addresses, p.String())
				}
			}
		}
		if dhcp == a.Name && lease != nil {
			s.Dhcp = &janusv1alpha1.DHCPLease{Address: lease.Address, Server: lease.Server, Router: lease.Router, DnsServers: lease.DNS, NtpServers: lease.NTP, Domain: lease.Domain, Hostname: lease.Hostname, ObtainedUnix: wall(lease.Obtained), ExpiresUnix: wall(lease.Expires)}
		}
		resp.Interfaces = append(resp.Interfaces, s)
	}

	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		routes, err := netlink.RouteListFiltered(family, &netlink.Route{Table: unix.RT_TABLE_MAIN}, netlink.RT_FILTER_TABLE)
		if err != nil {
			continue
		}
		for _, r := range routes {
			nr := &janusv1alpha1.NetworkRoute{Destination: "default", Metric: uint32(r.Priority)}
			if !isDefault(r.Dst) {
				nr.Destination = r.Dst.String()
			}
			if r.Gw != nil {
				nr.Gateway = r.Gw.String()
			}
			if li, ok := byIndex[r.LinkIndex]; ok {
				nr.Interface = li.name
			}
			resp.Routes = append(resp.Routes, nr)
		}
	}

	resp.DnsServers, resp.DnsSearch = readResolvConf(m.opts.ResolvConfPath)
	return resp, nil
}

func readResolvConf(path string) (servers, search []string) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "nameserver":
			servers = append(servers, fields[1])
		case "search", "domain":
			search = append(search, fields[1:]...)
		}
	}
	return servers, search
}
