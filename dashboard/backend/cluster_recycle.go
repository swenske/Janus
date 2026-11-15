package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"time"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"

	"github.com/swenske/Janus/dashboard/backend/internal/machines"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
	"github.com/swenske/Janus/internal/clusters"
)

// Recycling a cluster's members (docs/cluster.md "Recycling members"):
// each named member replaced, one at a time, by a new one - a new
// machine like it when it is one of this Controller's machines, else the
// node given - the traffic moving once (clusters.Engine.Replace), then
// the old machine destroyed. A newer version or another image goes with
// it: how a cluster changes release or schematic member by member.

// recycleRequest is POST /api/clusters/{id}/recycle.
type recycleRequest struct {
	// Members are replaced in this order.
	Members []string `json:"members"`
	// With: a member's replacement, an existing node; a member left out
	// is replaced by a new machine like its own.
	With map[string]clusterMemberRequest `json:"with,omitempty"`
	// Version: the new machines' release; empty: the old ones'.
	Version string `json:"version,omitempty"`
	// NewImage: the replacements may run another image than the members
	// (other extensions, HAProxy branch, kernel).
	NewImage bool `json:"new_image,omitempty"`
	// KeepAddress: each replacement takes its member's address over once
	// the member has left - its interface's static addressing - and comes
	// into service there (clusters.ReplaceOptions.KeepAddress).
	KeepAddress bool `json:"keep_address,omitempty"`
	// TemporaryAddress: a new machine's address on that interface until
	// then - CIDR, or "dhcp" -, where its member's is static; implies
	// KeepAddress. One is enough for several members: each gives it back
	// as it takes its member's.
	TemporaryAddress string `json:"temporary_address,omitempty"`
}

// keepAddress reports whether the replacements take their members'
// addresses over.
func (r recycleRequest) keepAddress() bool { return r.KeepAddress || r.TemporaryAddress != "" }

// awaitTimeout bounds a new machine's creation, registration and fleet
// trust (a var: the tests shorten it).
var awaitTimeout = 20 * time.Minute

// hypervisorName is hypervisor record id's name, for the journal - its
// id when it is gone.
func (a *app) hypervisorName(id string) string {
	if h, ok := a.hypervisors.Get(id); ok {
		return h.Name
	}
	return id
}

// machineOf is the machine node id runs on, nil when it isn't one of
// this Controller's machines.
func (a *app) machineOf(nodeID string) *machines.Machine {
	for _, m := range a.machines.List() {
		if m.NodeID == nodeID {
			return m
		}
	}
	return nil
}

// cloneSpec is a new machine's spec, like old's: its own name, a new MAC
// on each interface, version when given. A static address can't be
// cloned - two machines would hold it.
func cloneSpec(old machines.Spec, name, version string) (machines.Spec, error) {
	return cloneKeeping(old, name, version, -1)
}

// cloneKeeping is cloneSpec, interface keep's static address kept: a
// temporary one (cloneTakingOver).
func cloneKeeping(old machines.Spec, name, version string, keep int) (machines.Spec, error) {
	spec := old
	spec.Name = name
	if version != "" {
		spec.Version = version
	}
	spec.NICs = nil
	for i, n := range old.NICs {
		if n.Mode == "static" && i != keep {
			return machines.Spec{}, fmt.Errorf("its interface %s has a static address a new machine can't take while it runs", n.Name)
		}
		n.MAC = ""
		spec.NICs = append(spec.NICs, n)
	}
	spec.DNS = append([]string(nil), old.DNS...)
	spec.NTP = append([]string(nil), old.NTP...)
	spec.Extensions = append([]string(nil), old.Extensions...)
	return spec, nil
}

// cloneTakingOver is cloneSpec for a new machine that takes its member's
// address addr over: the interface holding it gets temporary instead -
// an address in CIDR form, or "dhcp" - until the member has left.
func cloneTakingOver(old machines.Spec, name, version, addr, temporary string) (machines.Spec, error) {
	tmp := old
	tmp.NICs = slices.Clone(old.NICs)
	found := -1
	for i, n := range tmp.NICs {
		if n.Mode != "static" || !slices.ContainsFunc(n.Addresses, func(a string) bool { return addressIs(a, addr) }) {
			continue
		}
		found = i
		switch {
		case temporary == "":
			return machines.Spec{}, fmt.Errorf("its interface %s has a static address a new machine can't take while it runs: give it a temporary address (CIDR, or dhcp) until then", n.Name)
		case strings.EqualFold(temporary, "dhcp"):
			n.Mode, n.Addresses, n.Gateway = "dhcp", nil, ""
		default:
			if addressIs(temporary, addr) {
				return machines.Spec{}, fmt.Errorf("the temporary address %s is %s's own: the new machine runs beside it until it leaves", temporary, old.Name)
			}
			n.Addresses = []string{temporary}
		}
		tmp.NICs[i] = n
	}
	if found < 0 {
		return machines.Spec{}, fmt.Errorf("its address %s isn't a static one of its machine's interfaces: there's no address to take over", addr)
	}
	return cloneKeeping(tmp, name, version, found)
}

// addressIs reports whether cidr (192.0.2.11/24) is the address ip.
func addressIs(cidr, ip string) bool {
	p, err := netip.ParsePrefix(cidr)
	if err != nil {
		return false
	}
	a, err := netip.ParseAddr(ip)
	return err == nil && p.Addr().Unmap() == a.Unmap()
}

// destroyAndWait destroys machine id and waits until it's gone.
func (a *app) destroyAndWait(ctx context.Context, id string) error {
	a.destroyMachine(id)
	for {
		m, ok := a.machines.Get(id)
		if !ok {
			return nil
		}
		if m.Phase == machines.PhaseFailed {
			return fmt.Errorf("destroying machine %s failed: %s", m.Spec.Name, m.Error)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// releaseMember is the cluster engine's Release: member m, out of its
// cluster, gives its address up for its replacement - its machine
// destroyed, or, a node the Controller didn't make, powered off.
func (a *app) releaseMember(ctx context.Context, m clusters.Member) error {
	if mach := a.machineOf(m.NodeID); mach != nil {
		return a.destroyAndWait(ctx, mach.ID)
	}
	n, ok := a.store.Get(m.NodeID)
	if !ok {
		return nil // no longer the Controller's: whether its address is free, the engine checks
	}
	sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := nodes.Shutdown(sctx, n); err != nil {
		return fmt.Errorf("powering %s off: %v", n.Name, err)
	}
	return nil
}

// forgetReleased is the cluster engine's Forget: member m, released -
// powered off, its address its replacement's now -, removed from the
// Controller's nodes like a node deleted (its page, its connection, its
// metrics). A machine's went with the machine.
func (a *app) forgetReleased(ctx context.Context, m clusters.Member) error {
	n, ok := a.store.Get(m.NodeID)
	if !ok {
		return nil
	}
	if err := a.removeNode(ctx, n.ID); err != nil {
		return err
	}
	log.Printf("cluster member %s: node %s (%s) forgotten - powered off, its address its replacement's", m.Name, n.Name, n.ID)
	return nil
}

// moveMemberNetwork is the cluster engine's MoveNetwork: cfg on trial on
// member m's node, confirmed where it answers then - the Controller
// follows it there (its record's address), and reads a machine's
// interfaces back into its record.
func (a *app) moveMemberNetwork(ctx context.Context, m clusters.Member, cfg *janusv1alpha1.NetworkConfig) error {
	n, err := a.memberNode(m)
	if err != nil {
		return err
	}
	nctx, cancel := context.WithTimeout(ctx, clusters.MoveWindow+time.Minute)
	defer cancel()
	if err := nodes.ApplyNetwork(nctx, n, a.store, cfg, clusters.MoveWindow); err != nil {
		return err
	}
	if mach := a.machineOf(n.ID); mach != nil {
		sctx, scancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer scancel()
		_ = a.runner.syncAs(sctx, mach.ID, "its cluster moved its address")
	}
	return nil
}

// recycleName is a new machine's name in cluster c: <cluster>-<n>, the
// first n no machine, node or member has.
func (a *app) recycleName(c *clusters.Cluster) string {
	taken := map[string]bool{}
	for _, m := range a.machines.List() {
		taken[m.Spec.Name] = true
	}
	for _, n := range a.store.List() {
		taken[strings.ToLower(n.Name)] = true
	}
	for _, m := range c.Members {
		taken[m.Name] = true
	}
	for n := 1; ; n++ {
		if name := fmt.Sprintf("%s-%d", c.Name, n); !taken[name] {
			return name
		}
	}
}

// newMachine creates a machine of spec, as the API's creation does.
func (a *app) newMachine(spec machines.Spec, why string) (*machines.Machine, error) {
	a.createMu.Lock()
	defer a.createMu.Unlock()
	if err := a.checkSpec(&spec); err != nil {
		return nil, err
	}
	m := &machines.Machine{Spec: spec, Phase: machines.PhasePending}
	m.Log("requested: %d vCPU, %d MiB on %s - %s", spec.VCPUs, spec.MemoryMiB, spec.HypervisorID, why)
	if err := a.machines.Add(m); err != nil {
		return nil, err
	}
	a.runner.start(m.ID, func(ctx context.Context) error { return a.runner.create(ctx, m.ID) })
	return m, nil
}

// awaitMachine waits for machine id's node: registered, answering, and
// trusting the fleet - what a cluster's member needs.
func (a *app) awaitMachine(ctx context.Context, id string) (*store.Node, error) {
	deadline := time.Now().Add(awaitTimeout)
	for {
		m, ok := a.machines.Get(id)
		if !ok {
			return nil, errors.New("the machine is gone")
		}
		if m.Phase == machines.PhaseFailed {
			return nil, fmt.Errorf("its creation failed: %s", m.Error)
		}
		if m.Phase == machines.PhaseReady {
			if n, ok := a.store.Get(m.NodeID); ok && n.TrustsFleet() {
				return n, nil
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("not ready and trusting the fleet after %s (%s)", awaitTimeout, m.Phase)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// destroyMachine has machine id destroyed in the background, as the
// API's deletion does.
func (a *app) destroyMachine(id string) {
	a.runner.stop(id)
	if err := a.runner.phase(id, machines.PhaseDestroying, "destroying: out of its cluster"); err != nil {
		return
	}
	a.runner.start(id, func(ctx context.Context) error { return a.runner.destroy(ctx, id) })
}

// recycle replaces cluster id's members as req says, one at a time; the
// first failure stops it.
func (a *app) recycle(ctx context.Context, id string, req recycleRequest, actor string) error {
	for _, name := range req.Members {
		c, ok := a.clusters.Store.Get(id)
		if !ok {
			return clusters.ErrNotFound
		}
		old := c.Member(name)
		if old == nil {
			return fmt.Errorf("%s isn't a member any more", name)
		}
		var repl clusters.Member
		var created *machines.Machine
		oldMachine := a.machineOf(old.NodeID)
		if w, ok := req.With[name]; ok {
			m, err := a.member(w)
			if err != nil {
				return fmt.Errorf("%s's replacement: %v", name, err)
			}
			repl = m
		} else {
			if oldMachine == nil {
				return fmt.Errorf("%s isn't a machine of this Controller: give its replacement", name)
			}
			var spec machines.Spec
			var err error
			if req.keepAddress() {
				spec, err = cloneTakingOver(oldMachine.Spec, a.recycleName(c), req.Version, old.Address, req.TemporaryAddress)
			} else if spec, err = cloneSpec(oldMachine.Spec, a.recycleName(c), req.Version); err != nil {
				err = fmt.Errorf("%v: keep its address - the new machine on a temporary address until it leaves -, or give its replacement", err)
			}
			if err != nil {
				return fmt.Errorf("%s: %v", name, err)
			}
			if c.Placement != nil {
				hv, warnings, err := a.recycleHypervisor(ctx, c, name, spec)
				if err != nil {
					return fmt.Errorf("%s's replacement: %v", name, err)
				}
				spec.HypervisorID = hv
				for _, wn := range warnings {
					a.clusters.Journal(id, actor, "placement", name, 0, "%s", wn)
				}
			}
			if created, err = a.newMachine(spec, "replacing "+name+" in cluster "+c.Name); err != nil {
				return fmt.Errorf("%s's replacement: %v", name, err)
			}
			a.clusters.Journal(id, actor, "recycle", name, 0, "%s: a new machine, %s, on %s", name, spec.Name, a.hypervisorName(spec.HypervisorID))
			n, err := a.awaitMachine(ctx, created.ID)
			if err != nil {
				a.destroyMachine(created.ID)
				return fmt.Errorf("%s's replacement %s: %v - destroyed", name, spec.Name, err)
			}
			if repl, err = a.member(clusterMemberRequest{NodeID: n.ID, Name: spec.Name}); err != nil {
				a.destroyMachine(created.ID)
				return fmt.Errorf("%s's replacement %s: %v - destroyed", name, spec.Name, err)
			}
		}
		if err := a.clusters.Replace(ctx, id, name, repl, clusters.ReplaceOptions{NewImage: req.NewImage, KeepAddress: req.keepAddress()}, actor); err != nil {
			// What failed decides: a replacement in service stays, and
			// the old member's machine goes once the member is out.
			cur, ok := a.clusters.Store.Get(id)
			inService := ok && cur.Member(repl.Name) != nil
			switch {
			case !inService && created != nil:
				a.destroyMachine(created.ID)
				err = fmt.Errorf("%v; %s destroyed", err, repl.Name)
			case inService && cur.Member(name) == nil && oldMachine != nil:
				// Destroyed already when its address was to be taken over.
				if _, still := a.machines.Get(oldMachine.ID); still {
					a.destroyMachine(oldMachine.ID)
					err = fmt.Errorf("%v; %s's machine destroyed", err, name)
				}
			}
			return err
		}
		switch {
		case oldMachine != nil && req.keepAddress():
			a.clusters.Journal(id, actor, "recycle", name, 0, "%s replaced by %s at its address: its machine destroyed", name, repl.Name)
		case oldMachine != nil:
			a.destroyMachine(oldMachine.ID)
			a.clusters.Journal(id, actor, "recycle", name, 0, "%s replaced by %s: its machine destroyed", name, repl.Name)
		case req.keepAddress():
			a.clusters.Journal(id, actor, "recycle", name, 0, "%s replaced by %s at its address: powered off, no longer one of the Controller's nodes", name, repl.Name)
		default:
			a.clusters.Journal(id, actor, "recycle", name, 0, "%s replaced by %s: it stays one of the Controller's nodes, drained until it reboots", name, repl.Name)
		}
	}
	return nil
}

func (a *app) handleClusterRecycle(w http.ResponseWriter, r *http.Request) {
	c, ok := a.clusterOr404(w, r)
	if !ok {
		return
	}
	var req recycleRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if len(req.Members) == 0 {
		writeError(w, http.StatusBadRequest, "members: the ones to replace")
		return
	}
	if t := req.TemporaryAddress; t != "" && !strings.EqualFold(t, "dhcp") {
		if p, err := netip.ParsePrefix(t); err != nil || !p.Addr().IsGlobalUnicast() {
			writeError(w, http.StatusBadRequest, "temporary_address: an address in CIDR form (192.0.2.30/24), or dhcp")
			return
		}
	}
	seen := map[string]bool{}
	for _, name := range req.Members {
		m := c.Member(name)
		switch {
		case m == nil:
			writeError(w, http.StatusBadRequest, name+" isn't a member")
			return
		case seen[name]:
			writeError(w, http.StatusBadRequest, name+" twice")
			return
		}
		seen[name] = true
		if _, given := req.With[name]; !given && a.machineOf(m.NodeID) == nil {
			writeError(w, http.StatusBadRequest, name+" isn't a machine of this Controller: give its replacement (with)")
			return
		}
	}
	for name := range req.With {
		if !seen[name] {
			writeError(w, http.StatusBadRequest, "with: "+name+" isn't among the members to replace")
			return
		}
	}
	actor := requestPrincipal(r).User
	a.runCluster(w, c, "recycling "+strings.Join(req.Members, ", "), actor, func(ctx context.Context) error {
		return a.recycle(ctx, c.ID, req, actor)
	})
}
