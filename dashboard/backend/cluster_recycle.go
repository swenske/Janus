package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

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
}

// awaitTimeout bounds a new machine's creation, registration and fleet
// trust (a var: the tests shorten it).
var awaitTimeout = 20 * time.Minute

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
	spec := old
	spec.Name = name
	if version != "" {
		spec.Version = version
	}
	spec.NICs = nil
	for _, n := range old.NICs {
		if n.Mode == "static" {
			return machines.Spec{}, fmt.Errorf("its interface %s has a static address a new machine can't take while it runs - give its replacement", n.Name)
		}
		n.MAC = ""
		spec.NICs = append(spec.NICs, n)
	}
	spec.DNS = append([]string(nil), old.DNS...)
	spec.NTP = append([]string(nil), old.NTP...)
	spec.Extensions = append([]string(nil), old.Extensions...)
	return spec, nil
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
	if err := a.runner.phase(id, machines.PhaseDestroying, "destroying: recycled out of its cluster"); err != nil {
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
			spec, err := cloneSpec(oldMachine.Spec, a.recycleName(c), req.Version)
			if err != nil {
				return fmt.Errorf("%s: %v", name, err)
			}
			if created, err = a.newMachine(spec, "replacing "+name+" in cluster "+c.Name); err != nil {
				return fmt.Errorf("%s's replacement: %v", name, err)
			}
			a.clusters.Journal(id, actor, "recycle", name, 0, "%s: a new machine, %s, on %s", name, spec.Name, spec.HypervisorID)
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
		if err := a.clusters.Replace(ctx, id, name, repl, clusters.ReplaceOptions{NewImage: req.NewImage}, actor); err != nil {
			// What failed decides: a replacement in service stays, and
			// the old member's machine goes once the member is out.
			cur, ok := a.clusters.Store.Get(id)
			inService := ok && cur.Member(repl.Name) != nil
			switch {
			case !inService && created != nil:
				a.destroyMachine(created.ID)
				err = fmt.Errorf("%v; %s destroyed", err, repl.Name)
			case inService && cur.Member(name) == nil && oldMachine != nil:
				a.destroyMachine(oldMachine.ID)
				err = fmt.Errorf("%v; %s's machine destroyed", err, name)
			}
			return err
		}
		if oldMachine != nil {
			a.destroyMachine(oldMachine.ID)
			a.clusters.Journal(id, actor, "recycle", name, 0, "%s replaced by %s: its machine destroyed", name, repl.Name)
		} else {
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
