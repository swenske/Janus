package main

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/swenske/Janus/dashboard/backend/internal/machines"
	"github.com/swenske/Janus/internal/clusters"
)

// Placing a cluster's machines (docs/cluster.md "On a hypervisor"): the
// physical host each hypervisor record reaches (HostInfo.HostID, kept on
// the record by its status), the host each member runs on (its machine's,
// observed by the sync), and place() - never two members on one host
// when the cluster requires it. Two records reaching the same host count
// as one.

// hostSlot is a hypervisor record and the host it reaches.
type hostSlot struct {
	Hypervisor string // the record's ID
	Name       string // the record's name
	HostID     string
	HostName   string
	// FreeMiB and TotalMiB: the host's memory available and in all, as
	// its hypervisor said; 0 when it didn't.
	FreeMiB, TotalMiB int
}

// room is the memory host h can still give machines once placed MiB
// went to it, keeping what a host keeps free (hostReserveMiB); -1: not
// known.
func (h hostSlot) room(placed int) int {
	if h.TotalMiB == 0 {
		return -1
	}
	return h.FreeMiB - placed - hostReserveMiB(h.TotalMiB)
}

// hostSlots are the records ids, each with the host it reaches - asked
// now (cached briefly): a record never asked knows none yet.
func (a *app) hostSlots(ctx context.Context, ids []string) ([]hostSlot, error) {
	var out []hostSlot
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		h, ok := a.hypervisors.Get(id)
		if !ok {
			return nil, fmt.Errorf("no hypervisor %s", id)
		}
		st := a.hypervisorStatus(ctx, h)
		if st.Host == nil || st.Host.HostID == "" {
			why := st.Error
			if why == "" {
				why = "it doesn't say"
			}
			return nil, fmt.Errorf("which host %s reaches can't be told: %s", h.Name, why)
		}
		out = append(out, hostSlot{Hypervisor: h.ID, Name: h.Name, HostID: st.Host.HostID, HostName: or(st.Host.Hostname, st.Host.HostID),
			FreeMiB: int(st.Host.MemoryFree >> 20), TotalMiB: int(st.Host.MemoryTotal >> 20)})
	}
	return out, nil
}

// memberHost is the host member m runs on: its machine's as last seen,
// else its hypervisor's; "" when it isn't one of the Controller's
// machines.
func (a *app) memberHost(m clusters.Member) (id, name string) {
	mach := a.machineOf(m.NodeID)
	if mach == nil {
		return "", ""
	}
	if mach.HostID != "" {
		return mach.HostID, or(mach.HostName, mach.HostID)
	}
	if h, ok := a.hypervisors.Get(mach.Spec.HypervisorID); ok && h.HostID != "" {
		return h.HostID, or(h.HostName, h.HostID)
	}
	return "", ""
}

// place picks a slot for each of n new machines of memMiB among cands,
// given the hosts members run on already (host ID: their names). Among
// the hosts a rule allows, the one with the most memory free goes first
// - the load spread -, cands' order breaking ties; a host without room
// for one more machine (what it keeps free aside) is never picked.
// required: each on a host of its own, or refused naming the hosts
// found; preferred: hosts of their own first, then the least busy -
// warnings say which; off: by memory alone.
func place(cands []hostSlot, used map[string][]string, n, memMiB int, mode string) ([]hostSlot, []string, error) {
	if len(cands) == 0 {
		return nil, nil, fmt.Errorf("no hypervisor to place %d machine(s) on", n)
	}
	// One slot per host: the first record reaching it.
	var hosts []hostSlot
	records := map[string][]string{}
	for _, s := range cands {
		if len(records[s.HostID]) == 0 {
			hosts = append(hosts, s)
		}
		records[s.HostID] = append(records[s.HostID], s.Name)
	}
	placed := map[string]int{} // MiB this call put on each host
	count := map[string]int{}  // members and machines placed, by host
	for id, names := range used {
		count[id] = len(names)
	}
	fits := func(h hostSlot) bool {
		r := h.room(placed[h.HostID])
		return r < 0 || r >= memMiB
	}
	// best is the host of pool with the most room left that fits, ok
	// false when none does.
	best := func(pool []hostSlot, ok func(hostSlot) bool) (hostSlot, bool) {
		var b hostSlot
		found := false
		for _, h := range pool {
			if !ok(h) || !fits(h) {
				continue
			}
			if !found || h.room(placed[h.HostID]) > b.room(placed[b.HostID]) {
				b, found = h, true
			}
		}
		return b, found
	}
	take := func(h hostSlot) {
		placed[h.HostID] += memMiB
		count[h.HostID]++
	}
	var picks []hostSlot
	var warnings []string
	anyHost := func(hostSlot) bool { return true }
	for len(picks) < n {
		if mode == clusters.AntiAffinityOff {
			h, ok := best(hosts, anyHost)
			if !ok {
				return nil, nil, noRoom(hosts, placed, memMiB)
			}
			take(h)
			picks = append(picks, h)
			continue
		}
		// A host of its own: no member there, nothing placed yet.
		h, ok := best(hosts, func(h hostSlot) bool { return count[h.HostID] == 0 })
		if !ok && mode == clusters.AntiAffinityRequired {
			if freeHosts(hosts, used) >= n { // enough hosts, not enough memory
				return nil, nil, noRoom(hosts, placed, memMiB)
			}
			return nil, nil, fmt.Errorf("anti-affinity: %d new machine(s) need as many hosts no member runs on - the hypervisors given reach %s", n, describeHosts(hosts, records, used, placed))
		}
		if !ok {
			least := -1
			for _, x := range hosts {
				if fits(x) && (least < 0 || count[x.HostID] < least) {
					least = count[x.HostID]
				}
			}
			if least < 0 {
				return nil, nil, noRoom(hosts, placed, memMiB)
			}
			h, _ = best(hosts, func(x hostSlot) bool { return count[x.HostID] == least })
			if mode == clusters.AntiAffinityPreferred {
				warnings = append(warnings, fmt.Sprintf("a machine goes to host %s, which already has %d member(s) of the cluster - anti-affinity preferred, not enough hosts", h.HostName, count[h.HostID]))
			}
		}
		take(h)
		picks = append(picks, h)
	}
	return picks, warnings, nil
}

// freeHosts counts the hosts no member runs on.
func freeHosts(hosts []hostSlot, used map[string][]string) int {
	n := 0
	for _, h := range hosts {
		if len(used[h.HostID]) == 0 {
			n++
		}
	}
	return n
}

// noRoom says no host can take a machine of memMiB more.
func noRoom(hosts []hostSlot, placed map[string]int, memMiB int) error {
	var parts []string
	for _, h := range hosts {
		parts = append(parts, fmt.Sprintf("%s %d MiB free, %d kept", h.HostName, h.FreeMiB-placed[h.HostID], hostReserveMiB(h.TotalMiB)))
	}
	return fmt.Errorf("no host has room for a machine of %d MiB: %s", memMiB, strings.Join(parts, "; "))
}

func describeHosts(hosts []hostSlot, records, used map[string][]string, placed map[string]int) string {
	var parts []string
	for _, h := range hosts {
		s := h.HostName
		if rs := records[h.HostID]; len(rs) > 1 {
			s += " (" + strings.Join(rs, " and ") + ": one host)"
		}
		if ms := used[h.HostID]; len(ms) > 0 {
			s += ", where " + strings.Join(ms, ", ") + " run(s)"
		}
		if r := h.room(placed[h.HostID]); r >= 0 {
			s += fmt.Sprintf(", %d MiB to give", r)
		}
		parts = append(parts, s)
	}
	return fmt.Sprintf("%d host(s): %s", len(hosts), strings.Join(parts, "; "))
}

// antiAffinity checks a mode; empty is required.
func antiAffinity(mode string) (string, error) {
	switch mode {
	case "":
		return clusters.AntiAffinityRequired, nil
	case clusters.AntiAffinityRequired, clusters.AntiAffinityPreferred, clusters.AntiAffinityOff:
		return mode, nil
	}
	return "", fmt.Errorf("anti_affinity %q: required, preferred or off", mode)
}

// placementProblems are what an operator should know about where c's
// members run.
func placementProblems(c *clusters.Cluster, views []clusterMemberView) []string {
	if c.Placement == nil || c.Placement.AntiAffinity == clusters.AntiAffinityOff {
		return nil
	}
	byHost := map[string][]string{}
	hostName := map[string]string{}
	var order, unknown []string
	for _, v := range views {
		if v.HostID == "" {
			unknown = append(unknown, v.Name)
			continue
		}
		if _, ok := byHost[v.HostID]; !ok {
			order = append(order, v.HostID)
		}
		byHost[v.HostID] = append(byHost[v.HostID], v.Name)
		hostName[v.HostID] = v.HostName
	}
	var out []string
	for _, id := range order {
		if names := byHost[id]; len(names) > 1 {
			out = append(out, fmt.Sprintf("%s run on the same host, %s: losing it stops them all", strings.Join(names, " and "), hostName[id]))
		}
	}
	if len(unknown) > 0 {
		out = append(out, fmt.Sprintf("%s: on a host the Controller can't tell - anti-affinity isn't checked for it", strings.Join(unknown, ", ")))
	}
	return out
}

// --- a cluster made of new machines ---

// clusterMachinesRequest is POST /api/clusters's "machines": the cluster
// made of count new machines.
type clusterMachinesRequest struct {
	Count int `json:"count"`
	// Spec is every machine's: its name and hypervisor are the
	// Controller's to give.
	Spec         machines.Spec `json:"spec"`
	Hypervisors  []string      `json:"hypervisors"`
	AntiAffinity string        `json:"anti_affinity"`
}

// freeNames are n machine names in cluster name: <name>-<i>, the first
// i no machine or node has.
func (a *app) freeNames(name string, n int) []string {
	taken := map[string]bool{}
	for _, m := range a.machines.List() {
		taken[m.Spec.Name] = true
	}
	for _, nd := range a.store.List() {
		taken[strings.ToLower(nd.Name)] = true
	}
	var out []string
	for i := 1; len(out) < n; i++ {
		if s := fmt.Sprintf("%s-%d", name, i); !taken[s] {
			out = append(out, s)
		}
	}
	return out
}

// templateSpec is spec for machine name on hypervisor hv - checked as
// its creation will check it.
func (a *app) templateSpec(spec machines.Spec, name, hv string) (machines.Spec, error) {
	s := spec
	s.Name, s.HypervisorID = name, hv
	s.NICs = nil
	for _, n := range spec.NICs {
		if n.Mode == "static" {
			return machines.Spec{}, fmt.Errorf("interface %s: a static address can't be given to every machine of a cluster - DHCP, or add nodes made one by one", n.Name)
		}
		n.MAC = ""
		s.NICs = append(s.NICs, n)
	}
	s.DNS, s.NTP, s.Extensions = slices.Clone(spec.DNS), slices.Clone(spec.NTP), slices.Clone(spec.Extensions)
	check := s
	check.NICs = slices.Clone(s.NICs)
	a.createMu.Lock()
	err := a.checkSpec(&check)
	a.createMu.Unlock()
	return s, err
}

// clusterOnHypervisors answers POST /api/clusters with machines: placed
// and checked before anything is made, then made, waited for, and the
// cluster created from them.
func (a *app) clusterOnHypervisors(w http.ResponseWriter, r *http.Request, name string, peersPort int, managed []string, req clusterMachinesRequest) {
	mode, err := antiAffinity(req.AntiAffinity)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Count < 1 || req.Count > 16 {
		writeError(w, http.StatusBadRequest, "machines.count: 1 to 16")
		return
	}
	slots, err := a.hostSlots(r.Context(), req.Hypervisors)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	memMiB := req.Spec.MemoryMiB
	if memMiB == 0 {
		memMiB = 1024 // a machine's default (checkSpec)
	}
	picks, warnings, err := place(slots, nil, req.Count, memMiB, mode)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var specs []machines.Spec
	for i, n := range a.freeNames(strings.TrimSpace(name), req.Count) {
		s, err := a.templateSpec(req.Spec, n, picks[i].Hypervisor)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("%s on %s: %v", n, picks[i].Name, err))
			return
		}
		specs = append(specs, s)
	}
	c, err := a.clusters.Store.Create(name, peersPort, managed)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if c, err = a.clusters.Store.Update(c.ID, func(c *clusters.Cluster) error {
		c.Placement = &clusters.Placement{AntiAffinity: mode, Hypervisors: slices.Clone(req.Hypervisors)}
		return nil
	}); err != nil {
		_ = a.clusters.Store.Remove(c.ID)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	actor := requestPrincipal(r).User
	var where []string
	for i, s := range specs {
		where = append(where, s.Name+" on "+picks[i].Name+" ("+picks[i].HostName+")")
	}
	a.clusters.Journal(c.ID, actor, "create", "", 0, "cluster %s to be made of %d new machine(s), anti-affinity %s: %s", c.Name, len(specs), mode, strings.Join(where, ", "))
	for _, wn := range warnings {
		a.clusters.Journal(c.ID, actor, "placement", "", 0, "%s", wn)
	}
	a.runCluster(w, c, "creating machines", actor, func(ctx context.Context) error { return a.buildOnHypervisors(ctx, c.ID, specs, actor) })
}

// buildOnHypervisors makes cluster id's machines, waits until each node
// is registered and trusts the fleet, then creates the cluster from them
// - the first its source. A failure before the cluster exists destroys
// the machines made.
func (a *app) buildOnHypervisors(ctx context.Context, id string, specs []machines.Spec, actor string) error {
	c, ok := a.clusters.Store.Get(id)
	if !ok {
		return clusters.ErrNotFound
	}
	var made []*machines.Machine
	fail := func(err error) error {
		for _, m := range made {
			a.destroyMachine(m.ID)
		}
		return fmt.Errorf("%v - the machines made are destroyed", err)
	}
	for _, s := range specs {
		m, err := a.newMachine(s, "a member of cluster "+c.Name)
		if err != nil {
			return fail(fmt.Errorf("%s: %v", s.Name, err))
		}
		made = append(made, m)
	}
	members := make([]clusters.Member, len(made))
	errs := make([]error, len(made))
	var wg sync.WaitGroup
	for i, m := range made {
		wg.Go(func() {
			n, err := a.awaitMachine(ctx, m.ID)
			if err == nil {
				members[i], err = a.member(clusterMemberRequest{NodeID: n.ID, Name: m.Spec.Name})
			}
			if err != nil {
				errs[i] = fmt.Errorf("%s: %v", m.Spec.Name, err)
			}
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return fail(err)
		}
	}
	if _, err := a.clusters.Store.Update(id, func(c *clusters.Cluster) error {
		c.Members, c.SecretsFrom = members, members[0].Name
		return nil
	}); err != nil {
		return fail(err)
	}
	a.clusters.Journal(id, actor, "create", "", 0, "%d machine(s) ready and trusting the fleet - the cluster made from them, from %s", len(members), members[0].Name)
	return a.clusters.Create(ctx, id, members[0].Name, actor)
}

// --- recycling ---

// recycleHypervisor is where the machine replacing member name goes, in
// cluster c with a placement: among its hypervisors those spec fits,
// placed against the other members' hosts - the replaced member's host
// is free (it leaves), taken last. Unreachable records are skipped.
func (a *app) recycleHypervisor(ctx context.Context, c *clusters.Cluster, name string, spec machines.Spec) (string, []string, error) {
	picks, warnings, err := a.placeMore(ctx, c, name, spec, 1)
	if err != nil {
		return "", nil, err
	}
	return picks[0], warnings, nil
}

// placeMore places n new machines of spec in cluster c, as
// recycleHypervisor places one (name: the member replaced, "" for
// none): the hypervisors they go to, in order.
func (a *app) placeMore(ctx context.Context, c *clusters.Cluster, name string, spec machines.Spec, n int) ([]string, []string, error) {
	used := map[string][]string{}
	oldHost := ""
	for _, m := range c.Members {
		id, _ := a.memberHost(m)
		switch {
		case m.Name == name:
			oldHost = id
		case id != "":
			used[id] = append(used[id], m.Name)
		}
	}
	var cands, last []hostSlot
	var skipped []string
	for _, hv := range c.Placement.Hypervisors {
		slots, err := a.hostSlots(ctx, []string{hv})
		if err != nil {
			skipped = append(skipped, err.Error())
			continue
		}
		if _, err := a.templateSpec(spec, spec.Name, hv); err != nil {
			skipped = append(skipped, fmt.Sprintf("%s: %v", slots[0].Name, err))
			continue
		}
		if slots[0].HostID == oldHost {
			last = append(last, slots[0])
		} else {
			cands = append(cands, slots[0])
		}
	}
	// Elsewhere than the replaced member's host when a rule allows it
	// without sharing a host: that one only frees its memory once the
	// member is gone. Its host when the others would mean a host shared
	// (preferred) or none (required) - the replacement has it to itself
	// once the member leaves (v2026.10.25's lab: preferred put it beside
	// another member, the replaced one's host left empty).
	picks, warnings, err := place(cands, used, n, spec.MemoryMiB, c.Placement.AntiAffinity)
	if (err != nil || len(warnings) > 0) && len(last) > 0 {
		if p, w, err2 := place(append(cands, last...), used, n, spec.MemoryMiB, c.Placement.AntiAffinity); err2 == nil && (err != nil || len(w) < len(warnings)) {
			picks, warnings, err = p, w, nil
		}
	}
	if err != nil {
		if len(skipped) > 0 {
			err = fmt.Errorf("%v (left out: %s)", err, strings.Join(skipped, "; "))
		}
		return nil, nil, err
	}
	var hvs []string
	for _, p := range picks {
		hvs = append(hvs, p.Hypervisor)
	}
	return hvs, warnings, nil
}
