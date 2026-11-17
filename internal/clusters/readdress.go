package clusters

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// A member's address (docs/cluster.md "Recycling members", docs/guide/
// clusters.md "Keeping a member's address"): the IP its peers listener
// binds and the others reach, what the configuration's templates name -
// keepalived's unicast_src_ip and peers, BIRD's router ID, the
// firewall's rules - and, outside the cluster, what a router's BGP
// neighbours, a firewall's rules, a NAT and the monitoring point at.
// Readdress moves a member to another address and tells every member;
// a replacement taking an old member's address over (ReplaceOptions.
// KeepAddress) is a Readdress to the old member's addressing once that
// one is gone.

// MoveWindow is how long a moving member's node waits for its new
// network configuration's confirmation before going back by itself: the
// orchestrator confirms over a new connection where the node answers,
// again every second until shortly before then. What it covers: the node
// taking the addresses and announcing them (netmgr: a gratuitous ARP, an
// unsolicited NA - a router whose entry still had the old member's MAC
// takes the new one at once; one ignoring them relearns it once that
// entry fails), its server certificate reissued for them, and the
// orchestrator's way back to it - a router in between included. Twice an
// ordinary change's minute: a takeover lets the old member go first,
// and a slow router mustn't revert it (#515's Proxmox proof: 44 s before
// announcements).
const MoveWindow = 2 * time.Minute

// Addressing is a member's interface's static addressing.
type Addressing struct {
	// Addresses in CIDR form ("192.0.2.11/24", "2001:db8::11/64"); the
	// first is the member's address - the one the others reach.
	Addresses []string `json:"addresses"`
	Gateway   string   `json:"gateway,omitempty"`
	Gateway6  string   `json:"gateway6,omitempty"`
}

// Check reports what is wrong with a.
func (a Addressing) Check() error {
	if len(a.Addresses) == 0 {
		return errors.New("addresses: at least the member's, in CIDR form (192.0.2.11/24)")
	}
	seen := map[netip.Addr]bool{}
	for _, s := range a.Addresses {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return fmt.Errorf("%q isn't an address in CIDR form (192.0.2.11/24)", s)
		}
		if !p.Addr().IsGlobalUnicast() {
			return fmt.Errorf("%s isn't an address the other members can reach", s)
		}
		if seen[p.Addr()] {
			return fmt.Errorf("%s twice", p.Addr())
		}
		seen[p.Addr()] = true
	}
	for _, g := range []struct{ name, ip string }{{"gateway", a.Gateway}, {"gateway6", a.Gateway6}} {
		if g.ip == "" {
			continue
		}
		ip, err := netip.ParseAddr(g.ip)
		if err != nil || (g.name == "gateway") != ip.Is4() {
			return fmt.Errorf("%s: %q isn't an IPv%s address", g.name, g.ip, map[bool]string{true: "4", false: "6"}[g.name == "gateway"])
		}
	}
	return nil
}

// Address is the member's address: the first one's IP.
func (a Addressing) Address() string {
	if len(a.Addresses) == 0 {
		return ""
	}
	p, err := netip.ParsePrefix(a.Addresses[0])
	if err != nil {
		return ""
	}
	return p.Addr().String()
}

func (a Addressing) String() string {
	s := strings.Join(a.Addresses, " ")
	if a.Gateway != "" {
		s += " via " + a.Gateway
	}
	if a.Gateway6 != "" {
		s += " via " + a.Gateway6
	}
	return s
}

// AddressingOf is the static addressing, in node configuration cfg, of
// the interface holding addr - the member's address first: what a
// replacement takes over. An address cfg doesn't list - one DHCP gives -
// is an error: the node's own configuration doesn't hold it.
func AddressingOf(cfg *janusv1alpha1.NetworkConfig, addr string) (Addressing, error) {
	want, err := netip.ParseAddr(addr)
	if err != nil {
		return Addressing{}, fmt.Errorf("%q isn't an IP address", addr)
	}
	for _, iface := range cfg.GetInterfaces() {
		if iface.GetMode() != janusv1alpha1.AddressingMode_ADDRESSING_MODE_STATIC {
			continue
		}
		var first string
		var rest []string
		for _, s := range iface.GetAddresses() {
			if p, err := netip.ParsePrefix(s); err == nil && p.Addr().Unmap() == want.Unmap() {
				first = s
			} else {
				rest = append(rest, s)
			}
		}
		if first != "" {
			return Addressing{Addresses: append([]string{first}, rest...), Gateway: iface.GetGateway(), Gateway6: iface.GetGateway6()}, nil
		}
	}
	return Addressing{}, fmt.Errorf("%s isn't a static address in its network configuration - one DHCP gives can't be taken over: a DHCP reservation follows a MAC, a replacement has its own", addr)
}

// Readdressed is node configuration cfg with to's addressing, static, on
// the interface holding address from - st, the node's network status,
// says which: a configuration left to DHCP doesn't list it. The rest -
// other interfaces, VLANs, MTUs, metrics, the hostname - stays; an
// interface that was DHCP's keeps the DNS servers it used, when cfg
// names none.
func Readdressed(cfg *janusv1alpha1.NetworkConfig, st *janusv1alpha1.NetworkStatusResponse, from string, to Addressing) (*janusv1alpha1.NetworkConfig, error) {
	if err := to.Check(); err != nil {
		return nil, err
	}
	fromIP, err := netip.ParseAddr(from)
	if err != nil {
		return nil, fmt.Errorf("%q isn't an IP address", from)
	}
	var holder *janusv1alpha1.NetworkInterfaceStatus
	for _, i := range st.GetInterfaces() {
		for _, a := range i.GetAddresses() {
			if p, err := netip.ParsePrefix(a); err == nil && p.Addr().Unmap() == fromIP.Unmap() {
				holder = i
			}
		}
	}
	if holder == nil {
		return nil, fmt.Errorf("no interface of the node holds %s", from)
	}
	out := &janusv1alpha1.NetworkConfig{}
	if cfg != nil {
		out = proto.Clone(cfg).(*janusv1alpha1.NetworkConfig)
	}
	var target *janusv1alpha1.NetworkInterface
	for _, iface := range out.GetInterfaces() {
		if iface.GetName() == holder.GetName() || (holder.GetKind() == "physical" && iface.GetMac() != "" && strings.EqualFold(iface.GetMac(), holder.GetMac())) {
			target = iface
			break
		}
	}
	if target == nil {
		// A configuration left to DHCP lists nothing: the interface listed
		// now, by its MAC when physical - the others, DHCP's default, had
		// no lease anyway (one interface keeps it).
		target = &janusv1alpha1.NetworkInterface{Name: holder.GetName()}
		switch {
		case holder.GetKind() == "vlan":
			target.Vlan = &janusv1alpha1.NetworkVLAN{Parent: holder.GetVlanParent(), Id: holder.GetVlanId()}
		case holder.GetMac() != "":
			target.Mac = strings.ToLower(holder.GetMac())
		}
		out.Interfaces = append(out.Interfaces, target)
	}
	wasDHCP := target.GetMode() == janusv1alpha1.AddressingMode_ADDRESSING_MODE_DHCP
	// None of to's addresses may stay on another interface.
	for _, iface := range out.GetInterfaces() {
		if iface == target {
			continue
		}
		for _, a := range iface.GetAddresses() {
			p, err := netip.ParsePrefix(a)
			if err != nil {
				continue
			}
			for _, b := range to.Addresses {
				if q, _ := netip.ParsePrefix(b); q.Addr() == p.Addr() {
					return nil, fmt.Errorf("%s is on its interface %s already", p.Addr(), iface.GetName())
				}
			}
		}
	}
	target.Mode = janusv1alpha1.AddressingMode_ADDRESSING_MODE_STATIC
	target.Addresses = slices.Clone(to.Addresses)
	target.Gateway, target.Gateway6 = to.Gateway, to.Gateway6
	if wasDHCP && len(out.GetDns().GetServers()) == 0 && len(st.GetDnsServers()) > 0 {
		if out.Dns == nil {
			out.Dns = &janusv1alpha1.NetworkDNS{}
		}
		out.Dns.Servers = slices.Clone(st.GetDnsServers())
		if len(out.Dns.GetSearch()) == 0 {
			out.Dns.Search = slices.Clone(st.GetDnsSearch())
		}
	}
	return out, nil
}

// AddressError: a replacement took a member's place but not its address
// - it serves on its own, the old member out of the cluster. Once the
// address is free, Readdress moves it there.
type AddressError struct {
	Old, New   string
	Addressing Addressing
	// Moved: the replacement is at the old member's address already, but
	// not every member knows it - it stays drained until an apply tells
	// them.
	Moved bool
	Err   error
}

func (e *AddressError) Error() string {
	if e.Moved {
		return fmt.Sprintf("%s replaced %s and is at its address %s, but: %v - %s stays drained: apply the cluster's version again, then undrain %s", e.New, e.Old, e.Addressing.Address(), e.Err, e.New, e.New)
	}
	return fmt.Sprintf("%s replaced %s and serves on its own address, but didn't take %s's address %s: %v - once %s is free, move %s there (readdress %s)", e.New, e.Old, e.Old, e.Addressing.Address(), e.Err, e.Addressing.Address(), e.New, e.Addressing)
}

func (e *AddressError) Unwrap() error { return e.Err }

// Readdress moves member name of cluster id to addressing to: drained
// first - the others serve -, its interface's addressing changed on
// trial and confirmed where the node answers then, the orchestrator
// following it (MoveNetwork); every member told its new address, the
// cluster's version applied again on each - its templates may name it -;
// back in service. A failure before the node moved puts it back in
// service where it was; one after leaves it drained, at its new address,
// for an apply to finish.
func (e *Engine) Readdress(ctx context.Context, id, name string, to Addressing, actor string) error {
	c, ok := e.Store.Get(id)
	if !ok {
		return ErrNotFound
	}
	m := c.Member(name)
	if m == nil {
		return fmt.Errorf("%s isn't a member", name)
	}
	if err := e.canReaddress(c, *m, to); err != nil {
		return err
	}
	if err := e.firewallFollows(c, name, m.Address, to.Address()); err != nil {
		return err
	}
	drained := e.Store.State(id).Members[name].Drained
	if !drained {
		dctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		err := e.Drain(dctx, id, name, actor)
		cancel()
		if err != nil {
			return fmt.Errorf("%s didn't drain before its address moves: %v", name, err)
		}
	}
	moved, err := e.moveMember(ctx, id, name, to, actor)
	if err != nil {
		if !moved && !drained {
			if uerr := e.Undrain(ctx, id, name, actor); uerr != nil {
				err = fmt.Errorf("%v; and back in service: %v", err, uerr)
			}
		}
		if moved {
			return fmt.Errorf("%s is at %s, but: %v - it stays drained: apply the cluster's version again, then undrain it", name, to.Address(), err)
		}
		return err
	}
	if drained {
		// It was drained before: so it stays.
		return nil
	}
	return e.Undrain(ctx, id, name, actor)
}

// canReaddress reports why member m of c can't move to to.
func (e *Engine) canReaddress(c *Cluster, m Member, to Addressing) error {
	if e.MoveNetwork == nil {
		return errors.New("this orchestrator can't move a member's address")
	}
	if err := to.Check(); err != nil {
		return err
	}
	addr := to.Address()
	if net.ParseIP(addr).Equal(net.ParseIP(m.Address)) {
		return fmt.Errorf("%s is at %s already", m.Name, addr)
	}
	for _, o := range c.Members {
		if o.Name != m.Name && net.ParseIP(o.Address).Equal(net.ParseIP(addr)) {
			return fmt.Errorf("%s is %s's address", addr, o.Name)
		}
	}
	if len(c.Members) < 2 {
		return fmt.Errorf("%s is the cluster's only member: nothing would serve while its address moves", m.Name)
	}
	return nil
}

// moveMember moves member name of cluster id, drained, to to; moved: its
// node is there - a failure after that is the others not told.
func (e *Engine) moveMember(ctx context.Context, id, name string, to Addressing, actor string) (moved bool, err error) {
	c, ok := e.Store.Get(id)
	if !ok {
		return false, ErrNotFound
	}
	m := c.Member(name)
	if m == nil {
		return false, fmt.Errorf("%s isn't a member", name)
	}
	cfg, err := e.Nodes.NetworkConfig(ctx, *m)
	if err != nil {
		return false, fmt.Errorf("%s's network configuration: %v", name, err)
	}
	st, err := e.Nodes.NetworkStatus(ctx, *m)
	if err != nil {
		return false, fmt.Errorf("%s's network: %v", name, err)
	}
	next, err := Readdressed(cfg, st, m.Address, to)
	if err != nil {
		return false, fmt.Errorf("%s: %v", name, err)
	}
	from := m.Address
	// Its VRRP out while it moves, back once every other member knows its
	// new address: from the moment its address changes, its keepalived
	// and the others' - each naming the other by the addresses they had,
	// unicast - no longer hear each other, and both would be MASTER
	// (#515's Proxmox proof: the virtual IP flapping 16 s between the
	// remaining member and the drained newcomer). A drained member's
	// priority is lowered, not nothing: alone, it takes the virtual IP.
	vrrpOut := c.Manages("vrrp") && c.Desired > 0
	if vrrpOut {
		if err := e.Nodes.ModuleApply(ctx, *m, "vrrp", nil, false); err != nil {
			return false, fmt.Errorf("%s's keepalived stopped before its address moves: %v", name, err)
		}
		e.Journal(id, actor, "address", name, 0, "%s's keepalived stopped while its address moves: it can't hear the others there until they know it", name)
	}
	if err := e.MoveNetwork(ctx, *m, next); err != nil {
		err = fmt.Errorf("%s moving to %s: %v", name, to, err)
		if vrrpOut {
			// Where it was: its keepalived back with the version.
			if aerr := e.Apply(ctx, id, c.Desired, []string{name}, actor); aerr != nil {
				err = fmt.Errorf("%v; and its keepalived back: %v", err, aerr)
			}
		}
		return false, err
	}
	e.Journal(id, actor, "address", name, 0, "%s moved from %s to %s: its network configuration confirmed there", name, from, to)
	addr := to.Address()
	c, err = e.Store.Update(id, func(c *Cluster) error {
		mm := c.Member(name)
		if mm == nil {
			return fmt.Errorf("%s isn't a member any more", name)
		}
		mm.Address = addr
		// An API address given on the old address follows it.
		if host, port, err := net.SplitHostPort(mm.API); err == nil && net.ParseIP(host).Equal(net.ParseIP(from)) {
			mm.API = net.JoinHostPort(addr, port)
		}
		return nil
	})
	if err != nil {
		return true, err
	}
	// Every member told: the peers section names it, the templates may -
	// the others first, then it: its keepalived starts once they all
	// name it where it is.
	if c.Desired > 0 {
		var others []string
		for _, o := range c.Members {
			if o.Name != name {
				others = append(others, o.Name)
			}
		}
		if len(others) > 0 {
			if err := e.Apply(ctx, id, c.Desired, others, actor); err != nil {
				return true, err
			}
		}
		if err := e.Apply(ctx, id, c.Desired, []string{name}, actor); err != nil {
			return true, err
		}
	} else {
		for _, o := range c.Members {
			if _, err := e.Nodes.ClusterSet(ctx, o, e.SetRequest(c, o, nil, nil)); err != nil {
				return true, fmt.Errorf("%s: ClusterSet: %v", o.Name, err)
			}
			if err := e.waitLearnt(ctx, c, o, actor); err != nil {
				return true, fmt.Errorf("%s: %v", o.Name, err)
			}
		}
	}
	e.Journal(id, actor, "address", name, c.Desired, "every member knows %s at %s", name, addr)
	_ = e.announce(ctx, id, "sync", "", actor) // its address changed; journaled
	return true, nil
}

// releaseAddress frees the address of member old, out of cluster id
// (Release: its machine destroyed, its node powered off), and waits
// until nothing answers there - checked from repl, which takes it.
func (e *Engine) releaseAddress(ctx context.Context, id string, old, repl Member, actor string) error {
	if err := e.Release(ctx, old); err != nil {
		return fmt.Errorf("freeing %s's address: %v", old.Name, err)
	}
	timeout := e.ReleaseTimeout
	if timeout <= 0 {
		timeout = 3 * time.Minute
	}
	deadline := time.Now().Add(timeout)
	target := APIAddress(old)
	for {
		pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := e.Nodes.Probe(pctx, repl, target)
		cancel()
		var refused *ProbeError
		if errors.As(err, &refused) {
			e.Journal(id, actor, "address", old.Name, 0, "%s's address %s is free: nothing answers at %s", old.Name, old.Address, target)
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("%s couldn't tell whether %s's address is free: %v", repl.Name, old.Name, err)
			}
			return fmt.Errorf("%s still answers at %s after %s: its address isn't free", old.Name, target, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// replaceKeepingAddress is Replace taking old's address over: repl joins
// drained on an address of its own; old is drained - the others serve -
// and leaves; its address is freed (Release) and repl moves there, every
// member told; repl comes into service. A failure before old's drain
// takes repl out again, the cluster as it was; one after it puts repl in
// service on its own address, for a Readdress once the cause is fixed
// (AddressError, NotRemovedError).
func (e *Engine) replaceKeepingAddress(ctx context.Context, c *Cluster, old, repl Member, opts ReplaceOptions, actor string) error {
	id := c.ID
	if e.Release == nil || e.MoveNetwork == nil {
		return errors.New("this orchestrator can't hand a member's address over")
	}
	if len(c.Members) < 2 {
		return fmt.Errorf("%s is the cluster's only member: nothing would serve while its address moves - add a member first, or replace it without keeping its address", old.Name)
	}
	cfg, err := e.Nodes.NetworkConfig(ctx, old)
	if err != nil {
		return fmt.Errorf("%s's network configuration: %v", old.Name, err)
	}
	to, err := AddressingOf(cfg, old.Address)
	if err != nil {
		return fmt.Errorf("%s: %v", old.Name, err)
	}
	for _, a := range to.Addresses {
		if p, err := netip.ParsePrefix(a); err == nil && net.ParseIP(repl.Address).Equal(net.IP(p.Addr().AsSlice())) {
			return fmt.Errorf("%s joins on an address of its own - it runs beside %s until that one leaves -, not %s: it takes %s's then", repl.Name, old.Name, repl.Address, to.Address())
		}
	}
	// The composition once old has left: repl in it, wherever it is.
	after := *c
	after.Members = append(slices.DeleteFunc(slices.Clone(c.Members), func(m Member) bool { return m.Name == old.Name }), repl)
	if err := e.firewallFollows(&after, repl.Name, repl.Address, to.Address()); err != nil {
		return err
	}
	e.Journal(id, actor, "replace", old.Name, 0, "%s to be replaced by %s, which joins drained on %s and takes %s's address %s once %s left", old.Name, repl.Name, repl.Address, old.Name, to, old.Name)
	if err := e.addMember(ctx, id, repl, actor, true, opts.NewImage); err != nil {
		return e.undoJoin(ctx, id, repl.Name, actor, fmt.Errorf("%s joining: %v", repl.Name, err))
	}
	// old out of service, repl not in yet: on an address of its own, it
	// isn't what routers, firewalls and NATs know - the others serve
	// while the address moves.
	if err := e.leaving(ctx, id, old.Name, false, actor); err != nil {
		return e.undoJoin(ctx, id, repl.Name, actor, err)
	}
	dctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	msg, err := e.Nodes.ClusterDrain(dctx, old, time.Minute)
	cancel()
	if err != nil {
		uerr := e.Nodes.ClusterUndrain(ctx, old)
		_ = e.announce(ctx, id, "in", old.Name, actor) // old serves on
		cause := fmt.Errorf("%s didn't drain: %v", old.Name, err)
		if uerr != nil {
			cause = fmt.Errorf("%v; and back in service: %v", cause, uerr)
		}
		return e.undoJoin(ctx, id, repl.Name, actor, cause)
	}
	e.setDrained(id, old.Name, true)
	e.Journal(id, actor, "drain", old.Name, 0, "%s drained, the others serve while its address goes to %s: %s", old.Name, repl.Name, msg)
	if err := e.removeMember(ctx, id, old.Name, false, true, actor); err != nil {
		if cur, ok := e.Store.Get(id); ok && cur.Member(old.Name) != nil {
			if uerr := e.Undrain(ctx, id, repl.Name, actor); uerr != nil {
				err = fmt.Errorf("%v; and %v", err, uerr)
			}
			return &NotRemovedError{Old: old.Name, New: repl.Name, Err: err, Address: old.Address}
		}
		// Out, but what follows its departure went wrong somewhere: the
		// apply after the move tells every member again.
		e.Journal(id, actor, "replace", repl.Name, 0, "%s left, but: %v", old.Name, err)
	}
	if err := e.releaseAddress(ctx, id, old, repl, actor); err != nil {
		return e.addressFailed(ctx, id, old.Name, repl.Name, to, false, err, actor)
	}
	moved, err := e.moveMember(ctx, id, repl.Name, to, actor)
	if moved {
		e.forget(ctx, id, old, repl.Name, actor)
	}
	if err != nil {
		return e.addressFailed(ctx, id, old.Name, repl.Name, to, moved, err, actor)
	}
	if err := e.Undrain(ctx, id, repl.Name, actor); err != nil {
		return fmt.Errorf("%s replaced %s at its address %s, but: %w", repl.Name, old.Name, to.Address(), err)
	}
	e.Journal(id, actor, "replace", repl.Name, 0, "%s replaced %s, at its address %s", repl.Name, old.Name, to.Address())
	return nil
}

// forget drops the orchestrator's record of old, released, now that
// repl holds its address - journaled, a failure too (it doesn't undo the
// replacement).
func (e *Engine) forget(ctx context.Context, id string, old Member, repl, actor string) {
	if e.Forget == nil {
		return
	}
	if err := e.Forget(ctx, old); err != nil {
		e.Journal(id, actor, "forget", old.Name, 0, "%s, released, still recorded as a node: %v - its record names %s's address now: remove it", old.Name, err, repl)
		return
	}
	e.Journal(id, actor, "forget", old.Name, 0, "%s forgotten: released, its address %s is %s's now", old.Name, old.Address, repl)
}

// firewallFollows reports a cluster whose firewall template depends on
// member name's own address: rendered for it at from and at to (c's
// composition, name in it), its firewall.nft differs. Such a ruleset -
// say, the API accepted only on daddr {{ .Member.Address }} - applied
// for from could refuse the member at to, the confirmation of its move
// among it, before the cluster's version is applied again with to: the
// node would revert, and a replacement taking an address over would have
// let the old member go for nothing. Nothing on a node can change the
// address and its ruleset in one trial, so it's refused before anything
// is touched.
func (e *Engine) firewallFollows(c *Cluster, name, from, to string) error {
	depends, err := e.firewallDependsOn(c, name, from, to)
	if err != nil || !depends {
		return err
	}
	return fmt.Errorf("version %d's firewall.nft depends on %s's own address (.Member.Address): its ruleset for %s could refuse it at %s - the confirmation of its move among the rest - before the cluster applies it again: match the interface (iifname) or the subnet instead of .Member.Address, apply that version, then move it", c.Desired, name, from, to)
}

// firewallDependsOn reports whether member name's firewall.nft, rendered
// from c's desired version, differs at address from and at to.
func (e *Engine) firewallDependsOn(c *Cluster, name, from, to string) (bool, error) {
	if !c.Manages("firewall") || c.Desired == 0 {
		return false, nil
	}
	v, err := e.Store.Version(c.ID, c.Desired)
	if err != nil {
		return false, err
	}
	render := func(addr string) ([]byte, error) {
		probe := *c
		probe.Members = slices.Clone(c.Members)
		for i := range probe.Members {
			if probe.Members[i].Name == name {
				probe.Members[i].Address = addr
			}
		}
		r, err := Render(v, &probe, name)
		if err != nil {
			return nil, err
		}
		return r.Modules["firewall"], nil
	}
	a, err := render(from)
	if err != nil {
		return false, fmt.Errorf("%s at %s: %v", name, from, err)
	}
	b, err := render(to)
	if err != nil {
		return false, fmt.Errorf("%s at %s: %v", name, to, err)
	}
	return !bytes.Equal(a, b), nil
}

// CheckReaddress reports why member name of cluster id can't move to to
// - what Readdress refuses before touching anything, for an API to
// answer at once rather than from a background job.
func (e *Engine) CheckReaddress(id, name string, to Addressing) error {
	c, ok := e.Store.Get(id)
	if !ok {
		return ErrNotFound
	}
	m := c.Member(name)
	if m == nil {
		return fmt.Errorf("%s isn't a member", name)
	}
	if err := e.canReaddress(c, *m, to); err != nil {
		return err
	}
	return e.firewallFollows(c, name, m.Address, to.Address())
}

// CheckKeepAddress reports why member name of cluster id can't be
// replaced by a newcomer taking its address over - what Replace with
// KeepAddress refuses before anything is touched, asked before an
// orchestrator makes a new machine for it (v2026.11.17's lab: a machine
// created, then destroyed, to hear it): the cluster's only member, an
// address its node's configuration doesn't hold statically, a firewall
// whose rules depend on the member's own address. temporary: the
// newcomer's address until then, in CIDR form - "", or "dhcp": not known
// yet (an address of the documentation's range stands for it).
func (e *Engine) CheckKeepAddress(ctx context.Context, id, name, temporary string) error {
	c, ok := e.Store.Get(id)
	if !ok {
		return ErrNotFound
	}
	old := c.Member(name)
	if old == nil {
		return fmt.Errorf("%s isn't a member", name)
	}
	if e.Release == nil || e.MoveNetwork == nil {
		return errors.New("this orchestrator can't hand a member's address over")
	}
	if len(c.Members) < 2 {
		return fmt.Errorf("%s is the cluster's only member: nothing would serve while its address moves - add a member first, or replace it without keeping its address", old.Name)
	}
	cfg, err := e.Nodes.NetworkConfig(ctx, *old)
	if err != nil {
		return fmt.Errorf("%s's network configuration: %v", old.Name, err)
	}
	to, err := AddressingOf(cfg, old.Address)
	if err != nil {
		return fmt.Errorf("%s: %v", old.Name, err)
	}
	probe := ""
	if p, err := netip.ParsePrefix(temporary); err == nil {
		probe = p.Addr().String()
		if net.ParseIP(probe).Equal(net.ParseIP(to.Address())) {
			return fmt.Errorf("the temporary address %s is %s's own: the newcomer runs beside it until it leaves", temporary, old.Name)
		}
	} else if ip := net.ParseIP(old.Address); ip != nil && ip.To4() == nil {
		probe = "2001:db8::1"
	} else {
		probe = "192.0.2.1"
	}
	depends, err := e.firewallDependsOn(c, old.Name, probe, old.Address)
	if err != nil {
		return err
	}
	if depends {
		return fmt.Errorf("version %d's firewall.nft depends on the member's own address (.Member.Address): the newcomer's ruleset at its temporary address could refuse the confirmation of its move to %s before the cluster applies it again: match the interface (iifname) or the subnet instead of .Member.Address, apply that version, then recycle %s", c.Desired, old.Address, old.Name)
	}
	return nil
}

// addressFailed: replacement repl took old's place in cluster id, but
// not its address to - back in service on its own unless it moved
// already (then the others don't all know it: it stays drained).
func (e *Engine) addressFailed(ctx context.Context, id, old, repl string, to Addressing, moved bool, cause error, actor string) error {
	if !moved {
		if err := e.Undrain(ctx, id, repl, actor); err != nil {
			cause = fmt.Errorf("%v; and %v", cause, err)
		}
	}
	return &AddressError{Old: old, New: repl, Addressing: to, Moved: moved, Err: cause}
}
