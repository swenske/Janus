package netmgr

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/nclient4"
	"github.com/mdlayher/packet"
	"github.com/vishvananda/netlink"
	"golang.org/x/net/bpf"
	"golang.org/x/sys/unix"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/events"
	"github.com/swenske/Janus/internal/netconfig"
)

// janusd's DHCPv4 client: one per interface in DHCP mode, run by the
// Manager (only with -manage-host). It takes a lease (discover, offer,
// request, acknowledgement), renews it from T1 on until it expires, and
// starts over when it's refused or has expired. Its messages go through
// a packet socket bound to the interface (nclient4): the interface has
// no address before its first lease, and nftables never sees them - a
// firewall ruleset can't cut a node off its own lease.
//
// It identifies itself by its MAC address alone (no client identifier),
// as the kernel's ip=dhcp did: a server's reservation, and the address a
// node had before this client existed, stay the same.

// dhcpLeaseDir holds the leases janusd's client obtained, one file per
// interface MAC, under RunDir.
const dhcpLeaseDir = "dhcp"

// Timing of janusd's client - vars for tests.
var (
	// dhcpTimeout is a first message's wait for an answer; nclient4
	// doubles it on each of dhcpTries.
	dhcpTimeout = 2 * time.Second
	dhcpTries   = 3
	// dhcpRetryMax caps the wait between two attempts at a lease.
	dhcpRetryMax = 30 * time.Second
	// dhcpRenewRetryMin and Max bound the wait between two renewal
	// attempts: half what is left of the lease, within them.
	dhcpRenewRetryMin = 5 * time.Second
	dhcpRenewRetryMax = 60 * time.Second
	// DHCPBootWait is how long Start waits for the first lease when an
	// interface is in DHCP mode, so what follows at boot (the first
	// certificates, the log servers, the console's banner) sees the
	// node's address. The client keeps trying afterwards.
	DHCPBootWait = 30 * time.Second
)

// dhcpOptions are the options asked of the server: what the kernel's
// client asked, and the lease's times.
var dhcpOptions = []dhcpv4.OptionCode{
	dhcpv4.OptionSubnetMask,
	dhcpv4.OptionRouter,
	dhcpv4.OptionDomainNameServer,
	dhcpv4.OptionHostName,
	dhcpv4.OptionDomainName,
	dhcpv4.OptionInterfaceMTU,
	dhcpv4.OptionNTPServers,
	dhcpv4.OptionIPAddressLeaseTime,
	dhcpv4.OptionRenewTimeValue,
	dhcpv4.OptionRebindingTimeValue,
}

// leaseFromACK reads a lease out of the server's acknowledgement (and
// its offer, kept for renewals), obtained at now (since the boot).
func leaseFromACK(ifname, mac string, offer, ack *dhcpv4.DHCPv4, now time.Duration) (*Lease, error) {
	ip, ok := netip.AddrFromSlice(ack.YourIPAddr.To4())
	if !ok || !ip.IsGlobalUnicast() {
		return nil, fmt.Errorf("the server's acknowledgement gives no usable address (%s)", ack.YourIPAddr)
	}
	mask := ack.SubnetMask()
	if mask == nil {
		mask = ack.YourIPAddr.DefaultMask()
	}
	ones, bits := mask.Size()
	if bits != 32 {
		return nil, fmt.Errorf("the server's acknowledgement gives no usable subnet mask (%s)", net.IP(mask))
	}
	l := &Lease{
		Interface: ifname,
		MAC:       mac,
		Address:   netip.PrefixFrom(ip, ones).String(),
		Domain:    ack.DomainName(),
		Hostname:  ack.HostName(),
		Obtained:  now,
		Offer:     offer.ToBytes(),
		ACK:       ack.ToBytes(),
	}
	if s := ack.ServerIdentifier(); s != nil {
		l.Server = s.String()
	}
	for _, r := range ack.Router() {
		if a, ok := netip.AddrFromSlice(r.To4()); ok && !a.IsUnspecified() {
			l.Router = a.String()
			break
		}
	}
	l.DNS = ipStrings(ack.DNS())
	l.NTP = ipStrings(ack.NTPServers())
	if v := ack.Options.Get(dhcpv4.OptionInterfaceMTU); len(v) == 2 {
		if mtu := int(v[0])<<8 | int(v[1]); mtu >= 576 {
			l.MTU = mtu
		}
	}
	// RFC 2131 4.4.5: T1 half the lease, T2 seven eighths, unless the
	// server says. A lease without a time (a server that gives none)
	// lasts a day, like dnsmasq's default.
	leaseTime := ack.IPAddressLeaseTime(24 * time.Hour)
	if leaseTime < 10*time.Second {
		leaseTime = 10 * time.Second
	}
	t1 := ack.IPAddressRenewalTime(leaseTime / 2)
	t2 := ack.IPAddressRebindingTime(leaseTime * 7 / 8)
	if t2 <= 0 || t2 > leaseTime {
		t2 = leaseTime * 7 / 8
	}
	if t1 <= 0 || t1 > t2 {
		t1 = leaseTime / 2
	}
	l.Renew = now + t1
	l.Rebind = now + t2
	l.Expires = now + leaseTime
	return l, nil
}

func ipStrings(ips []net.IP) []string {
	var out []string
	for _, ip := range ips {
		if a, ok := netip.AddrFromSlice(ip.To4()); ok && !a.IsUnspecified() {
			out = append(out, a.String())
		}
	}
	return out
}

func leaseFile(mac string) string {
	return filepath.Join(RunDir, dhcpLeaseDir, strings.ReplaceAll(mac, ":", "")+".json")
}

// saveLease records l for a restarted janusd (tmp + rename: whole or
// absent).
func saveLease(l *Lease) error {
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	path := leaseFile(l.MAC)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func forgetLease(mac string) {
	if err := os.Remove(leaseFile(mac)); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("netmgr: forget the DHCP lease of %s: %v", mac, err)
	}
}

// loadLeases reads the leases janusd's client (or janusd -boot-dhcp's,
// for a NoCloud seedfrom) recorded earlier in this boot, by MAC; expired
// ones are dropped. now is the time since the boot.
func loadLeases(now time.Duration) map[string]*Lease {
	out := map[string]*Lease{}
	entries, err := os.ReadDir(filepath.Join(RunDir, dhcpLeaseDir))
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(RunDir, dhcpLeaseDir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var l Lease
		if err := json.Unmarshal(data, &l); err != nil || l.MAC == "" || l.kernel() {
			log.Printf("netmgr: %s isn't a DHCP lease - ignored", path)
			continue
		}
		if l.expired(now) {
			_ = os.Remove(path)
			continue
		}
		out[l.MAC] = &l
	}
	return out
}

// dhcpTransport is one interface's way to its DHCP server - nclient4 in
// janusd, a fake in tests.
type dhcpTransport interface {
	// acquire takes a new lease, asking for requested if valid.
	acquire(ctx context.Context, requested netip.Addr) (*Lease, error)
	// renew asks to extend l.
	renew(ctx context.Context, l *Lease) (*Lease, error)
}

// errNAK is a server refusing a request: the lease is no more.
var errNAK = errors.New("the DHCP server refused (NAK)")

// packetTransport is nclient4 on a packet socket opened for each
// exchange on the interface with that index - it may be renamed
// meanwhile, its index stays.
type packetTransport struct {
	index int
	mac   string
}

// dhcpFilter keeps what reaches the packet socket to DHCP's answers:
// IPv4 UDP datagrams to port 68, unfragmented. Without it, the socket
// sees - and nclient4 reads and drops - every IPv4 packet on the
// interface while an exchange lasts: all of a busy node's traffic. The
// socket is SOCK_DGRAM: the filter starts at the IP header.
var dhcpFilter = func() []bpf.RawInstruction {
	prog, err := bpf.Assemble([]bpf.Instruction{
		bpf.LoadAbsolute{Off: 9, Size: 1},                                         // 0: IP protocol
		bpf.JumpIf{Cond: bpf.JumpNotEqual, Val: unix.IPPROTO_UDP, SkipTrue: 6},    // 1: not UDP: drop
		bpf.LoadAbsolute{Off: 6, Size: 2},                                         // 2: flags, fragment offset
		bpf.JumpIf{Cond: bpf.JumpBitsSet, Val: 0x1fff, SkipTrue: 4},               // 3: a fragment's tail: drop
		bpf.LoadMemShift{Off: 0},                                                  // 4: X = the IP header's length
		bpf.LoadIndirect{Off: 2, Size: 2},                                         // 5: UDP destination port
		bpf.JumpIf{Cond: bpf.JumpNotEqual, Val: nclient4.ClientPort, SkipTrue: 1}, // 6: not 68: drop
		bpf.RetConstant{Val: 0x40000},                                             // 7: keep
		bpf.RetConstant{Val: 0},                                                   // 8: drop
	})
	if err != nil {
		panic(err)
	}
	return prog
}()

// waitForEntropy makes sure the kernel's random pool is ready. The DHCP
// messages' transaction IDs come from u-root's rand, which polls
// getrandom without blocking until it is - spinning, and never making
// the kernel gather entropy, which a blocking getrandom does (jitter
// entropy). Early in a boot, under a hypervisor without RDRAND or
// virtio-rng, the pool isn't: janusd -boot-dhcp spun for minutes before
// sending anything.
var waitForEntropy = sync.OnceFunc(func() {
	var b [1]byte
	_, _ = cryptorand.Read(b[:]) // blocks until the pool is ready
})

func (t *packetTransport) client() (*nclient4.Client, string, error) {
	waitForEntropy()
	ifc, err := net.InterfaceByIndex(t.index)
	if err != nil {
		return nil, "", err
	}
	raw, err := packet.Listen(ifc, packet.Datagram, unix.ETH_P_IP, &packet.Config{Filter: dhcpFilter})
	if err != nil {
		return nil, "", err
	}
	conn := nclient4.NewBroadcastUDPConn(raw, &net.UDPAddr{Port: nclient4.ClientPort})
	c, err := nclient4.NewWithConn(conn, ifc.HardwareAddr, nclient4.WithTimeout(dhcpTimeout), nclient4.WithRetry(dhcpTries))
	if err != nil {
		raw.Close()
		return nil, "", err
	}
	return c, ifc.Name, nil
}

func (t *packetTransport) acquire(ctx context.Context, requested netip.Addr) (*Lease, error) {
	c, name, err := t.client()
	if err != nil {
		return nil, err
	}
	defer c.Close()
	ask := dhcpv4.WithRequestedOptions(dhcpOptions...)
	discover := []dhcpv4.Modifier{ask}
	if requested.Is4() {
		// In the discover only: the request asks for what was offered.
		discover = append(discover, dhcpv4.WithOption(dhcpv4.OptRequestedIPAddress(net.IP(requested.AsSlice()))))
	}
	offer, err := c.DiscoverOffer(ctx, discover...)
	if err != nil {
		return nil, err
	}
	nl, err := c.RequestFromOffer(ctx, offer, ask)
	if err != nil {
		var nak *nclient4.ErrNak
		if errors.As(err, &nak) {
			return nil, fmt.Errorf("%w: %v", errNAK, err)
		}
		return nil, err
	}
	return leaseFromACK(name, t.mac, nl.Offer, nl.ACK, sinceBoot())
}

func (t *packetTransport) renew(ctx context.Context, l *Lease) (*Lease, error) {
	offer, err := dhcpv4.FromBytes(l.Offer)
	if err != nil {
		return nil, fmt.Errorf("the lease's offer: %w", err)
	}
	ack, err := dhcpv4.FromBytes(l.ACK)
	if err != nil {
		return nil, fmt.Errorf("the lease's acknowledgement: %w", err)
	}
	c, name, err := t.client()
	if err != nil {
		return nil, err
	}
	defer c.Close()
	nl, err := c.Renew(ctx, &nclient4.Lease{Offer: offer, ACK: ack}, dhcpv4.WithRequestedOptions(dhcpOptions...))
	if err != nil {
		var nak *nclient4.ErrNak
		if errors.As(err, &nak) {
			return nil, fmt.Errorf("%w: %v", errNAK, err)
		}
		return nil, err
	}
	return leaseFromACK(name, t.mac, nl.Offer, nl.ACK, sinceBoot())
}

// dhcpClient runs one interface's client until its context ends.
type dhcpClient struct {
	mac   string
	index int
	t     dhcpTransport
	// held is the lease the client starts from: one recorded earlier in
	// this boot (by a janusd since restarted, or by janusd -boot-dhcp
	// for init) to renew when due, or the kernel's boot lease, whose
	// address it asks for.
	held *Lease
	// bound and lost report to the Manager; they're called from the
	// client's goroutine.
	bound func(*Lease)
	lost  func(reason string)
	// now and sleep are sinceBoot and a context-aware sleep, faked in
	// tests.
	now   func() time.Duration
	sleep func(ctx context.Context, d time.Duration) bool
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (c *dhcpClient) run(ctx context.Context) {
	if c.now == nil {
		c.now = sinceBoot
	}
	if c.sleep == nil {
		c.sleep = sleepCtx
	}
	var lease *Lease
	var requested netip.Addr
	if h := c.held; h != nil {
		if p, err := netip.ParsePrefix(h.Address); err == nil {
			requested = p.Addr()
		}
		if !h.kernel() && !h.expired(c.now()) {
			lease = h // already in effect: renewed when due
			log.Printf("netmgr: %s: DHCP lease %s in hand, renewed in %s", h.Interface, h.Address, max(h.Renew-c.now(), 0).Round(time.Second))
		}
	}
	retry := time.Second
	failures := 0
	for ctx.Err() == nil {
		if lease == nil {
			l, err := c.t.acquire(ctx, requested)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				failures++
				// Every failure while it's new, then one in ten: a node
				// on a network whose server is down keeps asking, quietly.
				if failures <= 3 || failures%10 == 0 {
					log.Printf("netmgr: %s: no DHCP lease yet (attempt %d): %v", c.mac, failures, err)
				}
				if !c.sleep(ctx, retry) {
					return
				}
				retry = min(retry*2, dhcpRetryMax)
				continue
			}
			failures, retry = 0, time.Second
			lease = l
			requested, _ = addrOf(l)
			c.bound(l)
			continue
		}

		// Bound: renew from T1 (unicast in RFC 2131, broadcast here - a
		// server takes either), again and again until it expires.
		if !c.sleep(ctx, lease.Renew-c.now()) {
			return
		}
		for lease != nil && ctx.Err() == nil {
			left := lease.Expires - c.now()
			if left <= 0 {
				c.lost("expired")
				lease = nil
				break
			}
			l, err := c.t.renew(ctx, lease)
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, errNAK) {
				log.Printf("netmgr: %s: %v - asking for a new lease", c.mac, err)
				c.lost("refused")
				lease = nil
				break
			}
			if err == nil {
				lease = l
				c.bound(l)
				break
			}
			wait := min(max(left/2, dhcpRenewRetryMin), dhcpRenewRetryMax, left)
			log.Printf("netmgr: %s: DHCP renewal: %v - again in %s, the lease ends in %s", c.mac, err, wait.Round(time.Second), left.Round(time.Second))
			if !c.sleep(ctx, wait) {
				return
			}
		}
	}
}

func addrOf(l *Lease) (netip.Addr, bool) {
	p, err := netip.ParsePrefix(l.Address)
	if err != nil {
		return netip.Addr{}, false
	}
	return p.Addr(), true
}

// newTransport is how a client reaches its server - a var for tests.
var newTransport = func(index int, mac string) dhcpTransport {
	return &packetTransport{index: index, mac: mac}
}

// syncClientsLocked runs a client on each interface p has in DHCP mode,
// and stops the others. A stopped client's lease stays in hand until it
// expires (it isn't released): a configuration that brings DHCP back on
// that interface takes it again at once. Called with mu held.
func (m *Manager) syncClientsLocked(p *plan, ls leaseSet) {
	want := map[string]*linkInfo{}
	for _, ip := range p.ifaces {
		if ip.mode == janusv1alpha1.AddressingMode_ADDRESSING_MODE_DHCP && ip.existing != nil && ip.existing.mac != "" {
			want[ip.existing.mac] = ip.existing
		}
	}
	for mac, r := range m.clients {
		if want[mac] == nil {
			r.cancel()
			delete(m.clients, mac)
		}
	}
	for mac, l := range want {
		if m.clients[mac] != nil {
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		r := &dhcpRun{mac: mac, cancel: cancel}
		m.clients[mac] = r
		c := &dhcpClient{
			mac:   mac,
			index: l.index,
			t:     newTransport(l.index, mac),
			held:  ls.byMAC[mac],
			bound: func(lease *Lease) { m.leaseBound(r, lease) },
			lost:  func(reason string) { m.leaseLost(r, reason) },
		}
		go c.run(ctx)
	}
}

// leaseBound takes a lease r's client obtained or renewed.
func (m *Manager) leaseBound(r *dhcpRun, l *Lease) {
	m.mu.Lock()
	if m.clients[r.mac] != r {
		m.mu.Unlock() // stopped meanwhile
		return
	}
	old := m.leases[r.mac]
	m.leases[r.mac] = l
	if err := saveLease(l); err != nil {
		log.Printf("netmgr: %s: record the DHCP lease: %v", l.Interface, err)
	}
	length := (l.Expires - l.Obtained).Round(time.Second)
	if old != nil && old.same(l) {
		if m.dhcpLease == old {
			m.dhcpLease = l // its new times, for Status
		}
		m.mu.Unlock()
		log.Printf("netmgr: %s: DHCP lease %s renewed for %s", l.Interface, l.Address, length)
		return
	}
	log.Printf("netmgr: %s: DHCP lease %s from %s (router %s) for %s", l.Interface, l.Address, orNone(l.Server), orNone(l.Router), length)
	if err := m.applyLocked(m.cfg); err != nil {
		log.Printf("netmgr: apply the DHCP lease of %s: %v", l.Interface, err)
	}
	m.mu.Unlock()
	events.Publish("network.dhcp_lease", map[string]string{"interface": l.Interface, "address": l.Address})
	m.changed()
}

// leaseLost drops the lease of r's client: refused by the server, or
// expired without a renewal.
func (m *Manager) leaseLost(r *dhcpRun, reason string) {
	m.mu.Lock()
	if m.clients[r.mac] != r {
		m.mu.Unlock()
		return
	}
	old := m.leases[r.mac]
	delete(m.leases, r.mac)
	forgetLease(r.mac)
	if k := m.kernelLease; k != nil && k.MAC == r.mac {
		m.kernelLease = nil // the server's word is the newer one
	}
	name := r.mac
	if old != nil {
		name = old.Interface
		log.Printf("netmgr: %s: DHCP lease %s %s - asking for a new one", name, old.Address, reason)
	}
	if err := m.applyLocked(m.cfg); err != nil {
		log.Printf("netmgr: %s without its DHCP lease: %v", name, err)
	}
	m.mu.Unlock()
	events.Publish("network.dhcp_lease_lost", map[string]string{"interface": name, "reason": reason})
	m.changed()
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// BootDHCP brings the network up by DHCP before janusd runs, for init:
// a NoCloud seed to fetch over the network (seedfrom) can't wait for
// janusd. As the default configuration does: every physical interface
// asks, the first lease is applied - its address (as netmgr's), default
// route and resolvers in resolvConf - and the others go down. The lease
// is recorded, so janusd's client takes it over and renews it, or
// janusd's configuration replaces it.
func BootDHCP(ctx context.Context, resolvConf string) (*Lease, error) {
	links, err := listLinks()
	if err != nil {
		return nil, err
	}
	var physical []linkInfo
	for _, l := range links {
		if l.kind == kindPhysical && l.mac != "" {
			physical = append(physical, l)
		}
	}
	if len(physical) == 0 {
		return nil, errors.New("no physical Ethernet interface")
	}
	if err := loopbackUp(); err != nil {
		log.Printf("netmgr: %v", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	got := make(chan *Lease, len(physical))
	for _, l := range physical {
		link, err := netlink.LinkByIndex(l.index)
		if err != nil {
			continue
		}
		if err := netlink.LinkSetUp(link); err != nil {
			log.Printf("netmgr: %s: set up: %v", l.name, err)
			continue
		}
		log.Printf("netmgr: %s: asking DHCP for a lease", l.name)
		go func(l linkInfo) {
			t := newTransport(l.index, l.mac)
			for retry := time.Second; ctx.Err() == nil; retry = min(retry*2, dhcpRetryMax) {
				lease, err := t.acquire(ctx, netip.Addr{})
				if err == nil {
					got <- lease
					return
				}
				if ctx.Err() == nil {
					log.Printf("netmgr: %s: no DHCP lease yet: %v", l.name, err)
				}
				if !sleepCtx(ctx, retry) {
					return
				}
			}
		}(l)
	}
	var lease *Lease
	select {
	case lease = <-got:
	case <-ctx.Done():
		return nil, fmt.Errorf("no DHCP lease: %w", ctx.Err())
	}
	cancel()

	var winner linkInfo
	for _, l := range physical {
		if l.mac == lease.MAC {
			winner = l
		}
	}
	lp, err := leasePlan(lease, &winner, winner.name, autoMetric(0), 0)
	if err != nil {
		return nil, err
	}
	p := &plan{ifaces: []ifPlan{lp}}
	for _, l := range physical {
		if l.mac != lease.MAC {
			p.down = append(p.down, l)
		}
	}
	if err := execute(p); err != nil {
		return nil, err
	}
	var search []string
	if lease.Domain != "" {
		search = []string{lease.Domain}
	}
	servers, search := netconfig.EffectiveDNS(&janusv1alpha1.NetworkConfig{}, lease.DNS, search)
	if err := writeIfChanged(resolvConf, netconfig.ResolvConf(servers, search)); err != nil {
		return nil, err
	}
	if err := saveLease(lease); err != nil {
		return nil, err
	}
	return lease, nil
}
