package clusters

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// Updating a cluster in place (docs/guide/clusters.md "Updating a
// cluster", #375): each member gets its own A/B update, one at a time -
// the cluster healthy first, the member drained (the balancer told
// first), its node's update with the automatic revert, the member back
// on the target release on its other slot, its stick tables learnt from
// its peers, undrained. No new machine: its address, MAC and name stay.
// The orchestrator says how a member's node is updated (Update): the
// Controller through a machine's own update path or a bundle it resolves,
// janusctl from a URL.

// UpgradeOptions are an in-place update's choices.
type UpgradeOptions struct {
	// Release is the version the members go to, as their nodes report
	// it once updated.
	Release string
	// Schematic is the image the members go to, its ID; "": each keeps
	// its own.
	Schematic string
	// Members are updated in this order; none: every member, a VRRP
	// cluster's MASTERs last (UpgradeOrder).
	Members []string
	// Update starts member m's own update to the target - checked by the
	// node, confirmed once HAProxy is healthy on the new slot, reverted
	// otherwise - and returns once the node is rebooting, with what it
	// said. A failure means the node didn't switch slots.
	Update func(ctx context.Context, m Member) (string, error)
	// Prepare readies member m's update while it still serves - its
	// bundle found, the image factory's build waited for: what fails
	// there leaves it in service, and what it fetches doesn't go through
	// the traffic its drain moves (a Controller reaching the factory
	// through this very cluster's address). Nil: nothing to ready.
	Prepare func(ctx context.Context, m Member) error
	// Done is told how member m's update ended: nil once it serves the
	// target, the error otherwise. Nil: nobody is told.
	Done func(m Member, err error)
	// BackTimeout bounds the wait for a member to come back from its
	// update; 0: 20 minutes.
	BackTimeout time.Duration
	// Poll is how often a member coming back is asked; 0: 2 seconds.
	Poll time.Duration
	// WaitBalancer: the balancer in front is asked too (BalancerSees) -
	// every other member in service before one goes, the member out of
	// it once drained, back in it once undrained - the next member left
	// alone until it is. The cluster's hook must be able to say
	// (Cluster.CanConfirm).
	WaitBalancer bool
	// Settle: a pause once a member is back in service, before the next
	// one goes - its clients settled on it; 0: none.
	Settle time.Duration
	// Handoff: a member's connections are handed over while it is out
	// and once it is back (handoff.go); nil: they are cut.
	Handoff *HandoffOptions
}

// MaxSettle bounds UpgradeOptions.Settle.
const MaxSettle = 30 * time.Minute

// UpgradeTimeout is how long a member has to come back from its update
// by default: the machines' own update timeout.
const UpgradeTimeout = 20 * time.Minute

// The stages of a member's update, MemberState.Stage.
const (
	StageDraining = "draining"
	StageUpdating = "updating"
	StageBooting  = "rebooting"
	StageLearning = "learning tables"
	// StageHandingOver: drained, it serves its connections, the others
	// copying them to it, until they end or the grace runs out.
	StageHandingOver = "handing over"
)

// RevertedError: a member's node reverted to its previous release - its
// new slot didn't confirm healthy - and serves on it; the update stops.
type RevertedError struct {
	Member, Release, Running string
}

func (e *RevertedError) Error() string {
	return fmt.Sprintf("%s reverted to Janus %s: the new release didn't confirm healthy on it (its own logs say why) - back in service on its previous release, the members after it left alone", e.Member, e.Running)
}

// NotBackError: a member didn't come back from its update in time; it
// stays drained, the update stops.
type NotBackError struct {
	Member  string
	Timeout time.Duration
	Err     error
}

func (e *NotBackError) Error() string {
	return fmt.Sprintf("%s isn't back on the target %s after its update: %v - it stays drained (undrain it once it serves), the members after it left alone", e.Member, e.Timeout, e.Err)
}

func (e *NotBackError) Unwrap() error { return e.Err }

// OnTarget reports whether v - a member's node's version - is the
// update's target.
func (o UpgradeOptions) OnTarget(v *janusv1alpha1.VersionResponse) bool {
	return v.GetVersion() == o.Release && (o.Schematic == "" || v.GetSchematicId() == o.Schematic)
}

// check refuses what cluster c can't do of o.
func (o UpgradeOptions) check(c *Cluster) error {
	if o.WaitBalancer {
		if why, ok := c.CanConfirm(); !ok {
			return fmt.Errorf("waiting for the balancer: %s", why)
		}
	}
	if o.Settle < 0 || o.Settle > MaxSettle {
		return fmt.Errorf("the pause between members: 0 to %s", MaxSettle)
	}
	if o.Handoff != nil {
		return o.Handoff.check()
	}
	return nil
}

// describe is o's choices, for the journal's first line.
func (o UpgradeOptions) describe(c *Cluster) string {
	var parts []string
	if o.WaitBalancer {
		how, _ := c.CanConfirm()
		parts = append(parts, "the balancer asked ("+how+")")
	}
	if o.Settle > 0 {
		parts = append(parts, "a pause of "+o.Settle.String()+" between members")
	}
	if h := o.Handoff; h != nil {
		var addrs []string
		for _, a := range h.addrs {
			addrs = append(addrs, a.String())
		}
		parts = append(parts, fmt.Sprintf("connections on %s handed over (a grace of %s, copied back for %s)", strings.Join(addrs, ", "), h.Grace, h.keep()))
	}
	if len(parts) == 0 {
		return ""
	}
	return " - " + strings.Join(parts, ", ")
}

// UpgradeOrder is the order cluster c's members are updated in: names'
// when given - each a member, once -, else every member's, VRRP MASTERs
// last so each virtual IP moves once; a BGP or ECMP cluster keeps its
// members' order.
func (e *Engine) UpgradeOrder(ctx context.Context, c *Cluster, names []string) ([]Member, error) {
	if len(names) > 0 {
		var out []Member
		for _, n := range names {
			m := c.Member(n)
			if m == nil {
				return nil, fmt.Errorf("%s isn't a member of cluster %s", n, c.Name)
			}
			if slices.ContainsFunc(out, func(o Member) bool { return o.Name == n }) {
				return nil, fmt.Errorf("%s is named twice", n)
			}
			out = append(out, *m)
		}
		return out, nil
	}
	var backups, masters []Member
	for _, m := range c.Members {
		if mm, err := e.Nodes.VRRPMasters(ctx, m); err == nil && len(mm) > 0 {
			masters = append(masters, m)
		} else {
			backups = append(backups, m)
		}
	}
	return append(backups, masters...), nil
}

// UpgradeCheck is what an update needs before it touches member m of
// cluster c: every other member in service, HAProxy answering, its peers
// connected and its stick tables in sync - so m's traffic has somewhere
// to go, and its tables someone to learn them from.
func (e *Engine) UpgradeCheck(ctx context.Context, c *Cluster, m Member) error {
	for _, o := range c.Members {
		if o.Name == m.Name {
			continue
		}
		info, err := e.Nodes.ClusterGet(ctx, o)
		if err != nil {
			return fmt.Errorf("%s doesn't answer: %v", o.Name, err)
		}
		if info.GetDrained() {
			return fmt.Errorf("%s is drained: undrain it first - %s's traffic would have nowhere to go", o.Name, m.Name)
		}
		if err := e.Nodes.Healthy(ctx, o); err != nil {
			return fmt.Errorf("%s's HAProxy doesn't answer: %v", o.Name, err)
		}
		// In service for its routers too: a member back from a reboot
		// announces again only once its BGP sessions are re-established.
		why, err := e.Nodes.Announcing(ctx, o)
		if err != nil {
			return fmt.Errorf("%s's BGP: %v", o.Name, err)
		}
		if why != "" {
			return fmt.Errorf("%s announces nothing to its BGP peers yet: %s", o.Name, why)
		}
		st, err := e.Nodes.ClusterStatus(ctx, o)
		if err != nil {
			return fmt.Errorf("%s's peers: %v", o.Name, err)
		}
		for _, p := range st.GetPeers() {
			if p.GetLocal() {
				continue
			}
			if !p.GetUp() {
				return fmt.Errorf("%s doesn't reach its peer %s: the cluster isn't healthy (janusctl cluster status)", o.Name, p.GetName())
			}
			for _, t := range p.GetTables() {
				if !t.GetSynced() {
					return fmt.Errorf("%s's stick table %s isn't in sync with %s yet: try again in a moment", o.Name, t.GetName(), p.GetName())
				}
			}
		}
	}
	return nil
}

// Upgrade updates cluster id's members in place, one at a time
// (UpgradeOptions). A member already on the target is skipped: calling
// again resumes a stopped update. The first failure stops it, the
// members after it left alone: a member whose update didn't start is
// back in service; one that reverted is back in service on its previous
// release (RevertedError); one not back in time stays drained
// (NotBackError).
func (e *Engine) Upgrade(ctx context.Context, id string, opts UpgradeOptions, actor string) error {
	c, ok := e.Store.Get(id)
	if !ok {
		return ErrNotFound
	}
	if opts.Release == "" || opts.Update == nil {
		return errors.New("an update needs a release and a way to update a node")
	}
	if len(c.Members) < 2 {
		return fmt.Errorf("cluster %s has one member: its traffic would have nowhere to go while it updates - update its node on its own, or add a member first", c.Name)
	}
	if err := opts.check(c); err != nil {
		return err
	}
	if opts.Handoff != nil {
		addrs, why := e.HandoffReady(ctx, c)
		if why != "" {
			return fmt.Errorf("handing connections over: %s", why)
		}
		h := *opts.Handoff
		h.addrs = addrs
		opts.Handoff = &h
	}
	order, err := e.UpgradeOrder(ctx, c, opts.Members)
	if err != nil {
		return err
	}
	var names []string
	for _, m := range order {
		names = append(names, m.Name)
	}
	e.Journal(id, actor, "upgrade", "", 0, "updating %s to Janus %s, one at a time: %s%s", c.Name, opts.Release, strings.Join(names, ", "), opts.describe(c))
	updated := 0
	for _, m := range order {
		// The pause is owed once a member was updated, before the next
		// one that will be.
		var pause time.Duration
		if updated > 0 {
			pause = opts.Settle
		}
		done, err := e.upgradeMember(ctx, c, m, opts, pause, actor)
		e.setStage(id, m.Name, "")
		if opts.Done != nil && (done || err != nil) {
			opts.Done(m, err)
		}
		if err != nil {
			_ = e.Store.UpdateState(id, func(s *State) {
				ms := s.Members[m.Name]
				ms.Error = err.Error()
				s.Members[m.Name] = ms
			})
			e.Journal(id, actor, "upgrade", m.Name, 0, "the update stops at %s: %v", m.Name, err)
			return err
		}
		if done {
			updated++
		}
	}
	e.Journal(id, actor, "upgrade", "", 0, "%s runs Janus %s: %d member(s) updated, %d already on it", c.Name, opts.Release, updated, len(order)-updated)
	return nil
}

// upgradeMember updates member m, after a pause when one is owed; done:
// it was updated (not skipped).
func (e *Engine) upgradeMember(ctx context.Context, c *Cluster, m Member, opts UpgradeOptions, pause time.Duration, actor string) (done bool, err error) {
	v0, err := e.Nodes.Version(ctx, m)
	if err != nil {
		return false, fmt.Errorf("%s doesn't answer: %v", m.Name, err)
	}
	if opts.OnTarget(v0) {
		e.Journal(c.ID, actor, "upgrade", m.Name, 0, "%s runs Janus %s already: skipped", m.Name, opts.Release)
		return false, nil
	}
	if pause > 0 {
		e.Journal(c.ID, actor, "upgrade", m.Name, 0, "a pause of %s before %s goes", pause, m.Name)
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(pause):
		}
	}
	// Healthy first - given a moment: the member updated before may
	// still be acknowledging its tables' last updates.
	if err := e.waitHealthy(ctx, c, m, opts); err != nil {
		return false, fmt.Errorf("not updating %s: %v", m.Name, err)
	}
	boot0, err := e.Nodes.BootTime(ctx, m)
	if err != nil {
		return false, fmt.Errorf("%s's boot time: %v", m.Name, err)
	}
	if opts.Prepare != nil {
		if err := opts.Prepare(ctx, m); err != nil {
			return false, fmt.Errorf("not updating %s: %v - still in service on Janus %s", m.Name, err, v0.GetVersion())
		}
	}
	// Its connections handed over first: the others copy them to it once
	// the router sends them their packets.
	// It takes no new connection - their senders try again a second
	// later, by then elsewhere - and the others read its connections.
	h := opts.Handoff
	if h != nil {
		if err := e.refuseNew(ctx, m, h.addrs, HandoffRefuse); err != nil {
			return false, fmt.Errorf("not updating %s: refusing new connections: %v - still in service on Janus %s", m.Name, err, v0.GetVersion())
		}
		n, by, err := e.handOut(ctx, c, m, h.addrs, h.Grace+5*time.Minute)
		if err != nil {
			e.stopHandOut(ctx, c, m, actor)
			_ = e.refuseNew(ctx, m, h.addrs, 0)
			return false, fmt.Errorf("not updating %s: handing its connections over: %v - still in service on Janus %s", m.Name, err, v0.GetVersion())
		}
		e.Journal(c.ID, actor, "handoff", m.Name, 0, "%s copy %s's connections to it while it is out - %d now", strings.Join(by, ", "), m.Name, n)
	}
	// undo: the handoff's part of a member left in service.
	undo := func() {
		if h != nil {
			e.stopHandOut(ctx, c, m, actor)
			_ = e.refuseNew(ctx, m, h.addrs, 0)
		}
	}
	// Out of service first: the balancer told, then its virtual IPs and
	// routes moved; its connections finish.
	e.setStage(c.ID, m.Name, StageDraining)
	if err := e.Drain(ctx, c.ID, m.Name, actor); err != nil {
		undo()
		return false, fmt.Errorf("%s didn't drain: %v", m.Name, err)
	}
	if opts.WaitBalancer {
		// Its traffic gone from the balancer before its node reboots.
		if err := e.waitBalancer(ctx, c, m.Name, false, opts.Poll, actor); err != nil {
			undo()
			if uerr := e.Undrain(ctx, c.ID, m.Name, actor); uerr != nil {
				return false, fmt.Errorf("not updating %s: %v; and back in service: %v", m.Name, err, uerr)
			}
			return false, fmt.Errorf("not updating %s: %v - back in service on Janus %s", m.Name, err, v0.GetVersion())
		}
	}
	// Out, it serves its connections until they end - the grace at most.
	if h != nil {
		e.setStage(c.ID, m.Name, StageHandingOver)
		if err := e.graceWait(ctx, c, m, h, actor); err != nil {
			return false, err
		}
	}
	e.setStage(c.ID, m.Name, StageUpdating)
	e.Journal(c.ID, actor, "upgrade", m.Name, 0, "%s updating from Janus %s (slot %s) to %s", m.Name, v0.GetVersion(), v0.GetActiveSlot(), opts.Release)
	said, err := opts.Update(ctx, m)
	if err != nil {
		// Its slots weren't switched: it serves on as it was.
		undo()
		if uerr := e.Undrain(ctx, c.ID, m.Name, actor); uerr != nil {
			return false, fmt.Errorf("%s's update failed: %v; and back in service: %v", m.Name, err, uerr)
		}
		return false, fmt.Errorf("%s's update failed: %v - back in service on Janus %s", m.Name, err, v0.GetVersion())
	}
	if said == "" {
		said = "rebooting"
	}
	e.setStage(c.ID, m.Name, StageBooting)
	e.Journal(c.ID, actor, "upgrade", m.Name, 0, "%s: %s", m.Name, said)
	v, err := e.waitBack(ctx, c, m, opts, v0, boot0, actor)
	if err != nil {
		return false, err
	}
	// Its connections ended with its reboot: the others copy them no more.
	if h != nil {
		e.stopHandOut(ctx, c, m, actor)
	}
	// Back: in service once its tables are learnt.
	e.setStage(c.ID, m.Name, StageLearning)
	if err := e.waitLearnt(ctx, c, m, actor); err != nil {
		return false, fmt.Errorf("%s: %v - it stays drained", m.Name, err)
	}
	// It copies the others' connections to them once the router sends it
	// part of their packets.
	if h != nil {
		if got, err := e.handIn(ctx, c, m, h.addrs, h.keep(), true); err != nil {
			e.Journal(c.ID, actor, "handoff", m.Name, 0, "%v - their connections the router moves to it are reset", err)
		} else {
			e.Journal(c.ID, actor, "handoff", m.Name, 0, "%s copies the others' connections to them for %s: %s", m.Name, h.keep(), strings.Join(got, ", "))
		}
	}
	if err := e.Undrain(ctx, c.ID, m.Name, actor); err != nil {
		return false, fmt.Errorf("%s back in service: %v", m.Name, err)
	}
	// Its routers hear of it once its BGP sessions are back: the next
	// member goes out of service only then.
	if err := e.waitAnnouncing(ctx, c, m, opts, actor); err != nil {
		return false, err
	}
	// And the balancer sends it traffic again, when it is asked.
	if opts.WaitBalancer {
		if err := e.waitBalancer(ctx, c, m.Name, true, opts.Poll, actor); err != nil {
			return false, fmt.Errorf("%s is back in service, but %v - the next members left alone", m.Name, err)
		}
	}
	// The connections the others took meanwhile, read again now that the
	// router spreads them.
	if h != nil {
		if _, err := e.handIn(ctx, c, m, h.addrs, h.keep(), false); err != nil {
			e.Journal(c.ID, actor, "handoff", m.Name, 0, "%v", err)
		}
	}
	if !opts.OnTarget(v) {
		return false, &RevertedError{Member: m.Name, Release: opts.Release, Running: v.GetVersion()}
	}
	_ = e.Store.UpdateState(c.ID, func(s *State) {
		ms := s.Members[m.Name]
		ms.Release, ms.Schematic, ms.Slot = v.GetVersion(), v.GetSchematicId(), v.GetActiveSlot()
		s.Members[m.Name] = ms
	})
	e.Journal(c.ID, actor, "upgrade", m.Name, 0, "%s runs Janus %s on slot %s, in service", m.Name, v.GetVersion(), v.GetActiveSlot())
	return true, nil
}

// waitBack waits until member m, rebooting into its update, is back: a
// new boot, janusd answering, HAProxy healthy, its peers connected - on
// the target release on its other slot, or reverted to its previous one
// (the version it returns says which). A member back undrained - the
// release it was drained on kept no drain across a reboot - is drained
// again at once, until its tables are learnt - after each boot: a revert
// is two.
func (e *Engine) waitBack(ctx context.Context, c *Cluster, m Member, opts UpgradeOptions, v0 *janusv1alpha1.VersionResponse, boot0 time.Time, actor string) (*janusv1alpha1.VersionResponse, error) {
	timeout, poll := opts.BackTimeout, opts.Poll
	if timeout <= 0 {
		timeout = UpgradeTimeout
	}
	if poll <= 0 {
		poll = 2 * time.Second
	}
	deadline := time.Now().Add(timeout)
	last := errors.New("it hasn't rebooted")
	// checked: the boot whose drain was checked; down: it didn't answer
	// once at least - it went through a reboot, its boot time didn't
	// just move with its clock.
	var checked time.Time
	down := false
	for {
		if time.Now().After(deadline) {
			return nil, &NotBackError{Member: m.Name, Timeout: timeout, Err: last}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(poll):
		}
		bctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		bt, err := e.Nodes.BootTime(bctx, m)
		cancel()
		switch {
		case err != nil:
			last = fmt.Errorf("it doesn't answer: %v", err)
			down = true
			continue
		case !bt.After(boot0.Add(2 * time.Second)):
			// The boot time moves a second or so with the clock: a reboot
			// moves it by the node's uptime at least.
			last = errors.New("it hasn't rebooted yet")
			continue
		}
		if !bt.Equal(checked) {
			if info, err := e.Nodes.ClusterGet(ctx, m); err == nil && !info.GetDrained() {
				dctx, cancel := context.WithTimeout(ctx, 90*time.Second)
				msg, err := e.Nodes.ClusterDrain(dctx, m, time.Minute)
				cancel()
				if err != nil {
					last = fmt.Errorf("back in service on its own, and draining it again failed: %v", err)
					continue
				}
				e.Journal(c.ID, actor, "drain", m.Name, 0, "%s came back in service on its own - the release it was drained on kept no drain across a reboot: drained again until its tables are learnt (%s)", m.Name, msg)
			}
			checked = bt
		}
		v, err := e.back(ctx, c, m)
		if err != nil {
			last = err
			continue
		}
		switch {
		case opts.OnTarget(v) && (v0.GetActiveSlot() == "" || v.GetActiveSlot() != v0.GetActiveSlot()):
			return v, nil
		case opts.OnTarget(v):
			last = fmt.Errorf("it runs Janus %s on slot %s, the slot it updated from", v.GetVersion(), v.GetActiveSlot())
		case v.GetVersion() == v0.GetVersion() && v.GetActiveSlot() == v0.GetActiveSlot() && !down:
			last = errors.New("it runs what it ran, and was never seen rebooting")
		case v.GetVersion() == v0.GetVersion() && v.GetActiveSlot() == v0.GetActiveSlot():
			// Rebooted on the slot it left: the new one didn't confirm
			// healthy, the node went back.
			e.Journal(c.ID, actor, "upgrade", m.Name, 0, "%s reverted to Janus %s on slot %s", m.Name, v.GetVersion(), v.GetActiveSlot())
			return v, nil
		default:
			last = fmt.Errorf("it runs Janus %s (schematic %s) on slot %s, not the target", v.GetVersion(), ShortID(v.GetSchematicId()), v.GetActiveSlot())
		}
	}
}

// back is member m's version once it is back from its reboot: janusd
// answering, HAProxy healthy, every peer connected.
func (e *Engine) back(ctx context.Context, c *Cluster, m Member) (*janusv1alpha1.VersionResponse, error) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	v, err := e.Nodes.Version(cctx, m)
	if err != nil {
		return nil, fmt.Errorf("it doesn't answer: %v", err)
	}
	if err := e.Nodes.Healthy(cctx, m); err != nil {
		return nil, fmt.Errorf("its HAProxy doesn't answer: %v", err)
	}
	if len(c.Members) > 1 {
		st, err := e.Nodes.ClusterStatus(cctx, m)
		if err != nil {
			return nil, fmt.Errorf("its peers: %v", err)
		}
		for _, p := range st.GetPeers() {
			if !p.GetLocal() && !p.GetUp() {
				return nil, fmt.Errorf("its peer %s isn't connected", p.GetName())
			}
		}
	}
	return v, nil
}

// AnnounceTimeout bounds the wait for a member back in service to
// announce its routes to its BGP peers again: their sessions are
// re-established, a collision with the session its previous boot left
// resolved.
var AnnounceTimeout = 2 * time.Minute

// waitAnnouncing waits until member m, undrained, announces its routes
// to its BGP peers again - at once without BGP.
func (e *Engine) waitAnnouncing(ctx context.Context, c *Cluster, m Member, opts UpgradeOptions, actor string) error {
	poll := opts.Poll
	if poll <= 0 {
		poll = 2 * time.Second
	}
	deadline := time.Now().Add(AnnounceTimeout)
	waited := false
	for {
		why, err := e.Nodes.Announcing(ctx, m)
		if err == nil && why == "" {
			if waited {
				e.Journal(c.ID, actor, "upgrade", m.Name, 0, "%s announces its routes to its BGP peers again", m.Name)
			}
			return nil
		}
		if err != nil {
			why = err.Error()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s is back in service but announces nothing to its BGP peers after %s: %s - the next members left alone: look at its sessions (janusctl network bgp status)", m.Name, AnnounceTimeout, why)
		}
		waited = true
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// waitHealthy waits until UpgradeCheck passes for member m, LearnTimeout
// at most.
func (e *Engine) waitHealthy(ctx context.Context, c *Cluster, m Member, opts UpgradeOptions) error {
	timeout, poll := e.LearnTimeout, opts.Poll
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if poll <= 0 {
		poll = 2 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		err := e.UpgradeCheck(ctx, c, m)
		if err == nil && opts.WaitBalancer {
			err = e.balancerHasOthers(ctx, c, m.Name)
		}
		if err == nil || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// setStage records where member name's update is, "" once it's over.
func (e *Engine) setStage(id, name, stage string) {
	_ = e.Store.UpdateState(id, func(s *State) {
		ms := s.Members[name]
		if ms.Stage == stage {
			return
		}
		ms.Stage = stage
		if stage != "" {
			ms.Error = ""
		}
		s.Members[name] = ms
	})
}
