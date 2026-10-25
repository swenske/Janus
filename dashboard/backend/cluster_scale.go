package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/machines"
	"github.com/swenske/Janus/dashboard/backend/internal/nodeproxy"
	"github.com/swenske/Janus/internal/clusters"
)

// Scaling a cluster horizontally (docs/cluster.md section 6): the
// primitive - POST /api/clusters/{id}/scale {members}, a member more is
// a new machine like the newest member's, placed; a member less the
// newest, drained, out, its machine destroyed - and the engine that
// uses it (clusterScaler): every member's load every 15 s, aggregated,
// clusters.Decide, journaled, acted on when its mode is on. Only for a
// cluster made on hypervisors: scaling makes and destroys machines.

// newestMachine is cluster c's member last joined that is one of the
// Controller's machines.
func (a *app) newestMachine(c *clusters.Cluster) (*clusters.Member, *machines.Machine) {
	var mem *clusters.Member
	var mach *machines.Machine
	for i := range c.Members {
		m := &c.Members[i]
		if x := a.machineOf(m.NodeID); x != nil && (mem == nil || !m.Joined.Before(mem.Joined)) {
			mem, mach = m, x
		}
	}
	return mem, mach
}

// scaleOut adds a member to cluster id: a new machine like its newest
// member's - its variables too -, placed against every member's host,
// within the policy's ceilings and the host's room.
func (a *app) scaleOut(ctx context.Context, id, actor string) error {
	c, ok := a.clusters.Store.Get(id)
	if !ok {
		return clusters.ErrNotFound
	}
	if c.Placement == nil {
		return errors.New("a member more is a new machine: a cluster made on hypervisors only")
	}
	model, mach := a.newestMachine(c)
	if mach == nil {
		return errors.New("no member is one of this Controller's machines to make another like")
	}
	spec, err := cloneSpec(mach.Spec, a.recycleName(c), "")
	if err != nil {
		return fmt.Errorf("a machine like %s: %v", model.Name, err)
	}
	hv, warnings, err := a.recycleHypervisor(ctx, c, "", spec)
	if err != nil {
		return err
	}
	spec.HypervisorID = hv
	if err := a.underCeiling(c, spec, 1); err != nil {
		return err
	}
	if h, ok := a.hypervisors.Get(hv); ok {
		if err := a.hostRoom(ctx, h, spec.MemoryMiB); err != nil {
			return err
		}
	}
	for _, w := range warnings {
		a.clusters.Journal(id, actor, "placement", "", 0, "%s", w)
	}
	created, err := a.newMachine(spec, "a member more for cluster "+c.Name)
	if err != nil {
		return err
	}
	a.clusters.Journal(id, actor, "scale", spec.Name, 0, "a member more: %s, a new machine like %s, on %s", spec.Name, model.Name, a.hypervisorName(spec.HypervisorID))
	node, err := a.awaitMachine(ctx, created.ID)
	if err != nil {
		a.destroyMachine(created.ID)
		return fmt.Errorf("%s: %v - destroyed", spec.Name, err)
	}
	m, err := a.member(clusterMemberRequest{NodeID: node.ID, Name: spec.Name, Vars: maps.Clone(model.Vars)})
	if err == nil {
		err = a.clusters.AddMember(ctx, id, m, actor)
	}
	if err != nil {
		if cur, ok := a.clusters.Store.Get(id); ok && cur.Member(spec.Name) != nil {
			_ = a.clusters.RemoveMember(ctx, id, spec.Name, true, actor)
		}
		a.destroyMachine(created.ID)
		return fmt.Errorf("%s: %v - taken out and destroyed", spec.Name, err)
	}
	return nil
}

// underCeiling checks that n members more like spec keep cluster c
// within its auto-scaling policy's ceilings on vCPUs and memory.
func (a *app) underCeiling(c *clusters.Cluster, spec machines.Spec, n int) error {
	as := c.Autoscale
	if as == nil || (as.MaxVCPUs == 0 && as.MaxMemoryMiB == 0) {
		return nil
	}
	vcpus, mem := n*spec.VCPUs, n*spec.MemoryMiB
	for _, m := range c.Members {
		if x := a.machineOf(m.NodeID); x != nil {
			vcpus, mem = vcpus+x.Spec.VCPUs, mem+x.Spec.MemoryMiB
		}
	}
	if (as.MaxVCPUs > 0 && vcpus > as.MaxVCPUs) || (as.MaxMemoryMiB > 0 && mem > as.MaxMemoryMiB) {
		what := "a member more"
		if n > 1 {
			what = fmt.Sprintf("%d members more", n)
		}
		return fmt.Errorf("%s would make %d vCPUs and %d MiB in all - above the policy's ceiling (%d vCPUs, %d MiB)", what, vcpus, mem, as.MaxVCPUs, as.MaxMemoryMiB)
	}
	return nil
}

// scaleCheck is what refuses cluster c going to members before
// anything is made or taken out - the request answered with it, not a
// job failing a moment later: members more need a machine to make them
// like, the hosts and the memory the placement asks, and room under the
// policy's ceilings; members fewer need as many of the Controller's
// machines to take out.
func (a *app) scaleCheck(ctx context.Context, c *clusters.Cluster, members int) error {
	n := members - len(c.Members)
	if n < 0 {
		have := 0
		for _, m := range c.Members {
			if a.machineOf(m.NodeID) != nil {
				have++
			}
		}
		if have < -n {
			return fmt.Errorf("%d member(s) fewer, but %d of the members are this Controller's machines - the others aren't taken out by scaling", -n, have)
		}
		return nil
	}
	if n == 0 {
		return nil
	}
	model, mach := a.newestMachine(c)
	if mach == nil {
		return errors.New("no member is one of this Controller's machines to make another like")
	}
	spec, err := cloneSpec(mach.Spec, a.recycleName(c), "")
	if err != nil {
		return fmt.Errorf("a machine like %s: %v", model.Name, err)
	}
	if err := a.underCeiling(c, spec, n); err != nil {
		return err
	}
	if _, _, err := a.placeMore(ctx, c, "", spec, n); err != nil {
		return err
	}
	return nil
}

// scaleIn takes cluster id's newest member out: drained, removed, its
// machine destroyed.
func (a *app) scaleIn(ctx context.Context, id, actor string) error {
	c, ok := a.clusters.Store.Get(id)
	if !ok {
		return clusters.ErrNotFound
	}
	m, mach := a.newestMachine(c)
	if mach == nil {
		return errors.New("no member is one of this Controller's machines to take out")
	}
	if len(c.Members) <= 1 {
		return errors.New("the cluster's last member: delete the cluster instead")
	}
	err := a.clusters.RemoveMember(ctx, id, m.Name, false, actor)
	if cur, ok := a.clusters.Store.Get(id); ok && cur.Member(m.Name) != nil {
		return err
	}
	a.destroyMachine(mach.ID)
	a.clusters.Journal(id, actor, "scale", m.Name, 0, "a member less: %s out, its machine destroyed", m.Name)
	return err // what followed its departure, if anything went wrong
}

// scaleTo brings cluster id to members, one step at a time.
func (a *app) scaleTo(ctx context.Context, id string, members int, actor string) error {
	for {
		c, ok := a.clusters.Store.Get(id)
		if !ok {
			return clusters.ErrNotFound
		}
		var err error
		switch {
		case len(c.Members) < members:
			err = a.scaleOut(ctx, id, actor)
		case len(c.Members) > members:
			err = a.scaleIn(ctx, id, actor)
		default:
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (a *app) handleClusterScale(w http.ResponseWriter, r *http.Request) {
	c, ok := a.clusterOr404(w, r)
	if !ok {
		return
	}
	var req struct {
		Members int `json:"members"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if c.Placement == nil {
		writeError(w, http.StatusBadRequest, "scaling makes and destroys machines: a cluster made on hypervisors only (New cluster, New machines)")
		return
	}
	if req.Members < 1 || req.Members > 16 {
		writeError(w, http.StatusBadRequest, "members: 1 to 16")
		return
	}
	if err := a.scaleCheck(r.Context(), c, req.Members); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	actor := requestPrincipal(r).User
	if as := c.Autoscale; as != nil && as.Mode == clusters.ScaleOn && as.PausedBy == "" {
		if _, err := a.clusters.Store.Update(c.ID, func(c *clusters.Cluster) error {
			if c.Autoscale != nil {
				c.Autoscale.PausedBy = actor
			}
			return nil
		}); err == nil {
			a.clusters.Journal(c.ID, actor, "autoscale", "", 0, "auto-scaling paused: %s set the members to %d - set the policy again to resume it", actor, req.Members)
		}
	}
	a.runCluster(w, c, fmt.Sprintf("scaling to %d", req.Members), actor, func(ctx context.Context) error { return a.scaleTo(ctx, c.ID, req.Members, actor) })
}

func (a *app) handleClusterAutoscale(w http.ResponseWriter, r *http.Request) {
	c, ok := a.clusterOr404(w, r)
	if !ok {
		return
	}
	var p clusters.Autoscale
	if !decodeBody(w, r, &p) {
		return
	}
	if err := p.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if p.Mode != clusters.ScaleOff {
		if c.Placement == nil {
			writeError(w, http.StatusBadRequest, "auto-scaling makes and destroys machines: a cluster made on hypervisors only")
			return
		}
		if slices.Contains(c.Managed, "vrrp") {
			writeError(w, http.StatusBadRequest, "a VRRP cluster: one member holds each virtual IP, a member more adds no capacity - grow the members instead (machines' vertical auto-scaling)")
			return
		}
	}
	p.PausedBy = ""
	c, err := a.clusters.Store.Update(c.ID, func(c *clusters.Cluster) error {
		c.Autoscale = &p
		return nil
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	actor := requestPrincipal(r).User
	a.clusters.Journal(c.ID, actor, "autoscale", "", 0, "auto-scaling %s, %d to %d members", p.Mode, p.MinMembers, p.MaxMembers)
	// A new policy starts over: no simulated count, no steps of the old.
	_ = a.clusters.Store.UpdateState(c.ID, func(st *clusters.State) { st.Scale, st.ScaleStatus = clusters.ScaleState{}, "" })
	a.clusterScaler.forget(c.ID)
	writeJSON(w, http.StatusOK, a.clusterView(c))
}

// --- the engine ---

type clusterScaler struct {
	a  *app
	mu sync.Mutex
	c  map[string]*scaledCluster
	// act carries a step out; the tests replace it.
	act func(ctx context.Context, id, action string) error
}

// scaledCluster is what the engine keeps of a cluster between ticks.
type scaledCluster struct {
	samples []clusters.ScaleSample
	prev    map[string]*nodeproxy.Load // by member
	last    *clusters.ScaleSample
	said    string // the decision journaled last, its figures masked
	busy    bool
}

func newClusterScaler(a *app) *clusterScaler {
	s := &clusterScaler{a: a, c: map[string]*scaledCluster{}}
	s.act = func(ctx context.Context, id, action string) error {
		if action == clusters.ScaleOut {
			return a.scaleOut(ctx, id, clusters.Actor)
		}
		return a.scaleIn(ctx, id, clusters.Actor)
	}
	return s
}

// forget drops what the engine kept of cluster id: its policy changed.
func (s *clusterScaler) forget(id string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.c, id)
}

// load is cluster id's members' last reading.
func (s *clusterScaler) load(id string) *clusters.ScaleSample {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if sc := s.c[id]; sc != nil {
		return sc.last
	}
	return nil
}

func (s *clusterScaler) run(ctx context.Context) {
	t := time.NewTicker(clusters.ScaleSampleEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.tick(ctx, time.Now())
	}
}

// tick reads every auto-scaled cluster's members and decides for each -
// not one an operation runs on: its member count is changing.
func (s *clusterScaler) tick(ctx context.Context, now time.Time) {
	var wg sync.WaitGroup
	for _, c := range s.a.clusters.Store.List() {
		if c.Autoscale == nil || c.Autoscale.Mode == clusters.ScaleOff {
			s.forget(c.ID)
			continue
		}
		if s.a.clusters.isBusy(c.ID) {
			continue
		}
		s.mu.Lock()
		sc := s.c[c.ID]
		if sc == nil {
			sc = &scaledCluster{prev: map[string]*nodeproxy.Load{}}
			s.c[c.ID] = sc
		}
		if sc.busy {
			s.mu.Unlock()
			continue
		}
		sc.busy = true
		s.mu.Unlock()
		wg.Go(func() {
			defer func() {
				s.mu.Lock()
				sc.busy = false
				s.mu.Unlock()
			}()
			sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			s.step(sctx, c, sc, now)
		})
	}
	wg.Wait()
}

// read is the members' aggregate now: HAProxy's idle and the CPUs'
// busy and steal shares averaged, the connections' share of maxconn
// the highest - over the members that answered.
func (s *clusterScaler) read(ctx context.Context, c *clusters.Cluster, sc *scaledCluster, now time.Time) clusters.ScaleSample {
	type reading struct {
		name string
		l    *nodeproxy.Load
	}
	out := make(chan reading, len(c.Members))
	var wg sync.WaitGroup
	for _, m := range c.Members {
		node, ok := s.a.store.Get(m.NodeID)
		if !ok {
			continue
		}
		wg.Go(func() {
			if l, err := nodes.Load(ctx, node); err == nil {
				out <- reading{m.Name, l}
			}
		})
	}
	wg.Wait()
	close(out)
	agg := clusters.ScaleSample{At: now, IdlePercent: -1, BusyPercent: -1, StealPercent: -1, ConnPercent: -1}
	var idle, busy, steal []float64
	for r := range out {
		agg.Members++
		x := sampleOf(r.l, sc.prev[r.name], now)
		sc.prev[r.name] = r.l
		if x.IdlePercent >= 0 {
			idle = append(idle, x.IdlePercent)
		}
		if x.BusyPercent >= 0 {
			busy, steal = append(busy, x.BusyPercent), append(steal, x.StealPercent)
		}
		if x.MaxConn > 0 {
			agg.ConnPercent = max(agg.ConnPercent, 100*float64(x.CurrConns)/float64(x.MaxConn))
		}
	}
	avg := func(v []float64) float64 {
		if len(v) == 0 {
			return -1
		}
		sum := 0.0
		for _, x := range v {
			sum += x
		}
		return sum / float64(len(v))
	}
	agg.IdlePercent, agg.BusyPercent, agg.StealPercent = avg(idle), avg(busy), avg(steal)
	return agg
}

// step reads cluster c's members, decides, journals what changed in the
// decision, and - on, not paused - starts the step.
func (s *clusterScaler) step(ctx context.Context, c *clusters.Cluster, sc *scaledCluster, now time.Time) {
	p := *c.Autoscale
	sample := s.read(ctx, c, sc, now)
	t := p.WithDefaults()
	keep := max(t.OutAfter, t.InAfter) + time.Minute
	s.mu.Lock()
	if sample.Members > 0 {
		sc.samples = append(sc.samples, sample)
	}
	for len(sc.samples) > 0 && now.Sub(sc.samples[0].At) > keep {
		sc.samples = sc.samples[1:]
	}
	sc.last = &sample
	samples := slices.Clone(sc.samples)
	s.mu.Unlock()

	st := s.a.clusters.Store.State(c.ID)
	scale := st.Scale
	if p.Mode == clusters.ScaleOn || scale.Members == 0 {
		scale.Members = len(c.Members) // on: judged at what it has; observing: simulated from there
	}
	if slices.Contains(c.Managed, "vrrp") {
		// Refused on, refused brought in - a cluster managed before those
		// refusals existed: say so, never step.
		status := "refused: a VRRP cluster can't scale - set auto-scaling off"
		if status != st.ScaleStatus {
			s.a.clusters.Journal(c.ID, clusters.Actor, "autoscale", "", 0, "%s", status)
			_ = s.a.clusters.Store.UpdateState(c.ID, func(st *clusters.State) { st.ScaleStatus = status })
		}
		return
	}
	d := clusters.Decide(p, scale, samples, now)
	status, said := scaleStatus(p, d, scale.Members), ""
	switch {
	case d.Action == clusters.ScaleNone:
		if d.Waiting != "" {
			said = d.Waiting
		}
	case p.Mode == clusters.ScaleObserve:
		said = fmt.Sprintf("would %s (%d → %d): %s - observing", stepWords(d.Action), d.From, d.To, d.Reason)
		scale = scale.Apply(d, now)
	case p.PausedBy != "":
		said = fmt.Sprintf("would %s (%d → %d): %s - paused since %s set the members", stepWords(d.Action), d.From, d.To, d.Reason, p.PausedBy)
	default:
		action := d.Action
		if s.a.clusters.start(c.ID, "scaling "+action, clusters.Actor, func(ctx context.Context) error { return s.act(ctx, c.ID, action) }) {
			said = fmt.Sprintf("%s (%d → %d): %s", stepWordsNow(d.Action), d.From, d.To, d.Reason)
			scale = scale.Apply(d, now)
			s.mu.Lock()
			sc.samples = nil // the load changes with the count: start over
			s.mu.Unlock()
		}
	}
	for _, w := range d.Warnings {
		said += " - " + w
	}
	if key := refusalKey(said); key != sc.said {
		sc.said = key
		if said != "" {
			s.a.clusters.Journal(c.ID, clusters.Actor, "autoscale", "", 0, "%s", said)
		}
	}
	if !slices.Equal(scale.Steps, st.Scale.Steps) || scale.Members != st.Scale.Members || status != st.ScaleStatus {
		if err := s.a.clusters.Store.UpdateState(c.ID, func(st *clusters.State) { st.Scale, st.ScaleStatus = scale, status }); err != nil {
			log.Printf("cluster %s: auto-scaling state: %v", c.Name, err)
		}
	}
}

func stepWords(action string) string {
	if action == clusters.ScaleOut {
		return "add a member"
	}
	return "take a member out"
}

func stepWordsNow(action string) string {
	if action == clusters.ScaleOut {
		return "adding a member"
	}
	return "taking a member out"
}

// scaleStatus is what the engine says now, for the page.
func scaleStatus(p clusters.Autoscale, d clusters.ScaleDecision, judged int) string {
	var s string
	switch {
	case d.Action != clusters.ScaleNone:
		s = fmt.Sprintf("%s (%d → %d)", stepWords(d.Action), d.From, d.To)
	case d.Waiting != "":
		s = "waiting: " + d.Waiting
	default:
		s = fmt.Sprintf("%d members carry the load", judged)
	}
	switch {
	case p.Mode == clusters.ScaleObserve:
		s = "observing - " + s
	case p.PausedBy != "":
		s = "paused (" + p.PausedBy + ") - " + s
	}
	return strings.TrimSpace(s)
}
