package clusters

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// Handing a member's connections over (docs/guide/clusters.md "Handing
// connections over", internal/handoff on the nodes): in a cluster whose
// members all hold the service addresses - BGP anycast, a router's ECMP
// routes -, the router spreads the connections over the members by a
// hash. Taking a member out moves its connections' packets to the others,
// which reset them; bringing it back moves part of the connections
// opened meanwhile to it, reset in turn. With a handoff, the others copy
// a member's connections to it while it is out - it serves them until
// they end, or its update's grace runs out -, and a member back copies
// the others' connections to them.

// HandoffOptions are an update's handoff choices.
type HandoffOptions struct {
	// Grace bounds how long a drained member keeps serving its
	// connections before its update; it goes once it has none left.
	Grace time.Duration
	// Keep is how long a member back copies the connections the others
	// had to them; 0: DefaultHandoffKeep.
	Keep time.Duration

	addrs []netip.Addr // the shared addresses, found by Upgrade
}

// The handoff's bounds and defaults.
const (
	DefaultHandoffGrace = 2 * time.Minute
	MaxHandoffGrace     = time.Hour
	DefaultHandoffKeep  = time.Hour
	MaxHandoffKeep      = 24 * time.Hour
)

// HandoffPoll is how often a drained member's connections are counted
// during its grace.
var HandoffPoll = 5 * time.Second

// SharedAddresses are the anycast addresses of cluster c: those every
// member holds as a host's own (a /32, a /128) - not a subnet's address
// every member happens to have too (a NAT's private network, a
// container's) -, but none's own member address, link-local or loopback
// ones.
func (e *Engine) SharedAddresses(ctx context.Context, c *Cluster) ([]netip.Addr, error) {
	var shared []netip.Addr
	for i, m := range c.Members {
		prefixes, err := e.Nodes.Prefixes(ctx, m)
		if err != nil {
			return nil, fmt.Errorf("%s's addresses: %v", m.Name, err)
		}
		var mine []netip.Addr
		for _, p := range prefixes {
			a := p.Addr().Unmap().WithZone("")
			if p.Bits() != a.BitLen() || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsMulticast() || a.IsUnspecified() {
				continue
			}
			mine = append(mine, a)
		}
		if i == 0 {
			shared = mine
			continue
		}
		shared = slices.DeleteFunc(shared, func(a netip.Addr) bool { return !slices.Contains(mine, a) })
	}
	shared = slices.DeleteFunc(shared, func(a netip.Addr) bool {
		return slices.ContainsFunc(c.Members, func(m Member) bool { own, err := netip.ParseAddr(m.Address); return err == nil && own.Unmap() == a })
	})
	slices.SortFunc(shared, func(a, b netip.Addr) int { return a.Compare(b) })
	return slices.Compact(shared), nil
}

// HandoffReady says whether cluster c's members can hand their
// connections over: the addresses they share, or why not.
func (e *Engine) HandoffReady(ctx context.Context, c *Cluster) ([]netip.Addr, string) {
	if len(c.Members) < 2 {
		return nil, "the cluster has one member"
	}
	var why []string
	for _, m := range c.Members {
		info, err := e.Nodes.ClusterGet(ctx, m)
		switch {
		case err != nil:
			why = append(why, fmt.Sprintf("%s doesn't answer: %v", m.Name, err))
		case info.GetHandoffUnavailable() != "":
			why = append(why, m.Name+": "+info.GetHandoffUnavailable())
		}
	}
	if len(why) > 0 {
		return nil, strings.Join(why, "; ")
	}
	addrs, err := e.SharedAddresses(ctx, c)
	if err != nil {
		return nil, err.Error()
	}
	if len(addrs) == 0 {
		return nil, "the members share no address: a handoff is for an anycast cluster (BGP, a router's ECMP routes), where every member holds the service addresses as /32s or /128s - a virtual IP moves with its connections' packets"
	}
	return addrs, ""
}

// check refuses o's durations out of bounds.
func (o *HandoffOptions) check() error {
	if o.Grace <= 0 || o.Grace > MaxHandoffGrace {
		return fmt.Errorf("the handoff's grace: 1 s to %s", MaxHandoffGrace)
	}
	if o.Keep < 0 || o.Keep > MaxHandoffKeep {
		return fmt.Errorf("the handoff's keep: up to %s", MaxHandoffKeep)
	}
	return nil
}

func (o *HandoffOptions) keep() time.Duration {
	if o.Keep == 0 {
		return DefaultHandoffKeep
	}
	return o.Keep
}

// HandoffHold is how long a member back holds the packets of connections
// it has no trace of: its readings of the others' connections catch up
// with the router meanwhile. HandoffRefuse bounds how long a member about
// to be drained refuses new connections - its drain ends it sooner.
var (
	HandoffHold   = 2 * time.Minute
	HandoffRefuse = time.Minute
)

func handoffReq(owner string, addrs []netip.Addr, d time.Duration) *janusv1alpha1.ClusterHandoffRequest {
	req := &janusv1alpha1.ClusterHandoffRequest{Owner: owner}
	if d > 0 {
		req.Seconds = uint32(max(1, d.Round(time.Second)/time.Second))
	}
	for _, a := range addrs {
		req.Addresses = append(req.Addresses, a.String())
	}
	return req
}

func seconds(d time.Duration) uint32 { return uint32(max(1, d.Round(time.Second)/time.Second)) }

// refuseNew has member m refuse new connections on addrs for d - the
// others read its connections once it takes no new one -, or stop.
func (e *Engine) refuseNew(ctx context.Context, m Member, addrs []netip.Addr, d time.Duration) error {
	req := handoffReq("", addrs, 0)
	if d > 0 {
		req.RefuseSeconds = seconds(d)
	} else {
		req.Stop = true
	}
	_, err := e.Nodes.ClusterHandoff(ctx, m, req)
	return err
}

// handOut has every other member of cluster c in service copy member
// m's connections to it for d: the count, as read by the last of them.
func (e *Engine) handOut(ctx context.Context, c *Cluster, m Member, addrs []netip.Addr, d time.Duration) (int, []string, error) {
	st := e.Store.State(c.ID)
	flows := 0
	var by []string
	for _, o := range c.Members {
		if o.Name == m.Name || st.Members[o.Name].Drained {
			continue
		}
		resp, err := e.Nodes.ClusterHandoff(ctx, o, handoffReq(m.Name, addrs, d))
		if err != nil {
			return 0, nil, fmt.Errorf("%s: %v", o.Name, err)
		}
		flows, by = int(resp.GetState().GetFlows()), append(by, o.Name)
	}
	return flows, by, nil
}

// stopHandOut has the other members stop copying m's connections - they
// ended with its reboot.
func (e *Engine) stopHandOut(ctx context.Context, c *Cluster, m Member, actor string) {
	for _, o := range c.Members {
		if o.Name == m.Name {
			continue
		}
		req := handoffReq(m.Name, nil, 0)
		req.Stop = true
		if _, err := e.Nodes.ClusterHandoff(ctx, o, req); err != nil {
			e.Journal(c.ID, actor, "handoff", o.Name, 0, "%s didn't stop copying %s's connections: %v - it stops by itself", o.Name, m.Name, err)
		}
	}
}

// handIn has member m copy each other member's connections to it for d -
// hold: and hold the packets of connections it has no trace of meanwhile;
// the counts by member.
func (e *Engine) handIn(ctx context.Context, c *Cluster, m Member, addrs []netip.Addr, d time.Duration, hold bool) ([]string, error) {
	var got []string
	for _, o := range c.Members {
		if o.Name == m.Name {
			continue
		}
		req := handoffReq(o.Name, addrs, d)
		if hold {
			req.HoldSeconds, hold = seconds(HandoffHold), false
		}
		resp, err := e.Nodes.ClusterHandoff(ctx, m, req)
		if err != nil {
			return got, fmt.Errorf("%s copying %s's connections: %v", m.Name, o.Name, err)
		}
		got = append(got, fmt.Sprintf("%s's %d", o.Name, resp.GetState().GetFlows()))
	}
	return got, nil
}

// graceWait lets drained member m serve its connections - the others
// copying them to it - until it has none left or the grace runs out.
func (e *Engine) graceWait(ctx context.Context, c *Cluster, m Member, h *HandoffOptions, actor string) error {
	start := time.Now()
	deadline := start.Add(h.Grace)
	left := -1
	for {
		// Read again at once, the end moved past the grace: a reboot
		// takes a moment.
		n, _, err := e.handOut(ctx, c, m, h.addrs, time.Until(deadline)+time.Minute)
		if err == nil {
			left = n
		}
		if left == 0 {
			e.Journal(c.ID, actor, "handoff", m.Name, 0, "%s has no connection left after %s", m.Name, time.Since(start).Round(time.Second))
			return nil
		}
		if time.Now().After(deadline) {
			e.Journal(c.ID, actor, "handoff", m.Name, 0, "%s still has %d connection(s) after its grace of %s: they end with its reboot", m.Name, left, h.Grace)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(min(HandoffPoll, time.Until(deadline)+time.Millisecond)):
		}
	}
}
