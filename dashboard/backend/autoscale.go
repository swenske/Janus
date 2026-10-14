package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/http"
	"slices"
	"sync"
	"time"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/haproxy"

	"github.com/swenske/Janus/dashboard/backend/internal/autoscale"
	"github.com/swenske/Janus/dashboard/backend/internal/machines"
	"github.com/swenske/Janus/dashboard/backend/internal/nodeproxy"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
)

// Auto-scaling (docs/guide/auto-scaling.md): every autoscale.SampleEvery,
// each ready machine with a policy that isn't off has its node's load
// read - HAProxy's idle time, the CPUs, the memory, OOM kills -, and
// internal/autoscale decides. Observing, the step it would take is
// written in the machine's journal and simulated, nothing changed.
//
// What it does lives in the machine's record (AutoscaleState: the size it
// judges, its last steps, its last word) and journal; the samples only in
// memory - a restarted Controller samples again before deciding.

// readThreadsEvery is how often a machine's HAProxy configuration is read
// for what fixes its threads.
const readThreadsEvery = 5 * time.Minute

// statusEvery: a machine's record is written when auto-scaling's status
// changes - or at least this often, for its time.
const statusEvery = 5 * time.Minute

type autoscaler struct {
	a  *app
	mu sync.Mutex
	m  map[string]*scaledMachine // by machine ID
}

// scaledMachine is what the loop keeps of a machine between two ticks.
type scaledMachine struct {
	samples  []autoscale.Sample
	prev     *nodeproxy.Load // the reading before, for the CPUs' busy share
	threads  string          // what fixes HAProxy's threads, "" when nothing
	threadAt time.Time
	busy     bool // a step under way
	resizing bool // its size moving (autoscale_act.go)
	// lastRefusal: the last step refused (the host's room...), journaled
	// once.
	lastRefusal string
	// sysctlAt: when the node's suggestions were last looked at;
	// sysctlSeen what observing journaled of them.
	sysctlAt   time.Time
	sysctlSeen map[string]bool
}

func newAutoscaler(a *app) *autoscaler {
	return &autoscaler{a: a, m: map[string]*scaledMachine{}}
}

// run samples and decides until ctx ends.
func (s *autoscaler) run(ctx context.Context) {
	t := time.NewTicker(autoscale.SampleEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.tick(ctx)
	}
}

// eligible reports whether m is auto-scaled now - paused ones too: one
// resized goes back to its size.
func eligible(m *machines.Machine) bool {
	return m.Spec.Autoscale != nil && m.Phase == machines.PhaseReady && m.NodeID != ""
}

// tick takes one sample of every auto-scaled machine, in parallel, and
// decides for each.
func (s *autoscaler) tick(ctx context.Context) {
	seen := map[string]bool{}
	for _, m := range s.a.machines.List() {
		if !eligible(m) || s.a.runner.busy(m.ID) {
			continue
		}
		seen[m.ID] = true
		s.mu.Lock()
		sm := s.m[m.ID]
		if sm == nil {
			sm = &scaledMachine{}
			s.m[m.ID] = sm
		}
		if sm.busy {
			s.mu.Unlock()
			continue
		}
		sm.busy = true
		s.mu.Unlock()
		// Not waited for: a step resizing a machine takes minutes, the
		// others sample meanwhile - busy keeps a machine's steps apart.
		go func() {
			defer func() {
				s.mu.Lock()
				sm.busy = false
				s.mu.Unlock()
			}()
			sctx, cancel := context.WithTimeout(ctx, resizeTimeout+2*autoscale.SampleEvery+sysctlTimeout)
			defer cancel()
			if err := s.step(sctx, m, sm, time.Now()); err != nil {
				log.Printf("machine %s: auto-scaling: %v", m.ID, err)
			}
		}()
	}
	// Machines no longer auto-scaled - or under a change of the runner's:
	// their samples are another size's, they start over.
	s.mu.Lock()
	for id := range s.m {
		if !seen[id] {
			delete(s.m, id)
		}
	}
	s.mu.Unlock()
}

// step samples m's node and decides - and, its mode "on", acts.
func (s *autoscaler) step(ctx context.Context, m *machines.Machine, sm *scaledMachine, now time.Time) error {
	node, ok := s.a.store.Get(m.NodeID)
	if !ok {
		return nil
	}
	pol := m.Spec.Autoscale
	nominal := autoscale.Size{VCPUs: m.Spec.VCPUs, MemoryMiB: m.Spec.MemoryMiB}
	r := report{state: m.AutoscaleState}
	if r.state == nil || r.state.Mode != pol.Mode {
		r.state = &machines.AutoscaleState{Mode: pol.Mode, State: autoscale.State{Size: nominal}}
	}

	// Not resizing - observing, or paused: a machine auto-scaling
	// resized goes back to its size first.
	if pol.Mode != machines.AutoscaleOn && m.Size != nil && (m.Size.VCPUs != nominal.VCPUs || m.Size.MemoryMiB != nominal.MemoryMiB) {
		reason := "auto-scaling paused: back to its size"
		if pol.Mode == machines.AutoscaleObserve {
			reason = "auto-scaling observes: back to its size"
		}
		from := autoscale.Size{VCPUs: m.Size.VCPUs, MemoryMiB: m.Size.MemoryMiB}
		d := autoscale.Decision{Action: autoscale.Down, Resource: "size", From: from, To: nominal, Reason: reason}
		if _, err := s.resize(ctx, m, node, sm, d, now); err != nil {
			r.status = fmt.Sprintf("couldn't go back to its size: %v", err)
			return s.write(m, now, r)
		}
	}
	if pol.Mode == machines.AutoscaleOff {
		r.status = "paused"
		return s.write(m, now, r)
	}

	lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	load, err := nodes.Load(lctx, node)
	cancel()
	if err != nil {
		r.status = fmt.Sprintf("the node doesn't answer (%v): nothing decided until it does", err)
		return s.write(m, now, r)
	}
	sm.samples = append(sm.samples, sampleOf(load, sm.prev, now))
	sm.prev = load
	keep := now.Add(-maxWindow(pol.Thresholds()) - time.Minute)
	i := 0
	for i < len(sm.samples) && sm.samples[i].At.Before(keep) {
		i++
	}
	sm.samples = slices.Clip(sm.samples[i:])

	if now.Sub(sm.threadAt) > readThreadsEvery {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if cfg, err := nodes.HAProxyConfig(cctx, node); err == nil {
			sm.threads, sm.threadAt = haproxy.PinnedThreads(cfg), now
		}
		cancel()
	}

	p := autoscale.Policy{
		Nominal:       nominal,
		Max:           autoscale.Size{VCPUs: pol.MaxVCPUs, MemoryMiB: pol.MaxMemoryMiB},
		ThreadsPinned: sm.threads,
		Thresholds:    pol.Thresholds(),
	}
	if sz := m.Size; sz != nil {
		// The virtual machine's own room: a policy changed but not yet
		// applied (its restart) can't reach beyond it.
		p.Max.VCPUs = min(p.Max.VCPUs, sz.MaxVCPUs)
		if sz.MaxMemoryMiB > 0 {
			p.Max.MemoryMiB = min(p.Max.MemoryMiB, sz.MaxMemoryMiB)
		}
		p.MemoryStepMiB = sz.MemoryStepMiB
	}

	st := r.state.State
	if pol.Mode == machines.AutoscaleOn {
		// Resizing: the size judged is the one the machine has.
		if m.Size == nil {
			r.status = "its size isn't known yet: nothing decided until its record is read"
			return s.write(m, now, r)
		}
		st.Size = autoscale.Size{VCPUs: m.Size.VCPUs, MemoryMiB: m.Size.MemoryMiB}
	}
	d := autoscale.Decide(p, st, sm.samples, now)
	r.warnings = d.Warnings
	switch {
	case d.Action == autoscale.None:
	case pol.Mode == machines.AutoscaleOn:
		got, err := s.resize(ctx, m, node, sm, d, now)
		if err != nil && got == d.From {
			// Nothing changed: no step taken - its state stays, the
			// status says why.
			st.OOMKills, st.OOMCounted = d.OOMKills, true
			r.state.State = st
			r.status = fmt.Sprintf("couldn't %s to %s: %v", map[autoscale.Action]string{autoscale.Up: "grow", autoscale.Down: "shrink"}[d.Action], d.To, err)
			return s.write(m, now, r)
		}
		d.To = got
	default:
		e := machines.JournalEntry{Time: now.UTC(), Mode: pol.Mode, Action: string(d.Action), Resource: d.Resource, From: d.From, To: d.To, Reason: d.Reason}
		if err := s.a.machines.AppendJournal(m.ID, e); err != nil {
			return err
		}
	}
	next := st.Apply(d, now)
	r.state.State = next
	r.status = statusText(d, sm.samples, next.Size, pol.Mode)
	if pol.Sysctl {
		memoryStep := d.Action != autoscale.None && d.From.MemoryMiB != d.To.MemoryMiB
		if msg := s.sysctl(ctx, m, node, sm, r.state, memoryStep, now); msg != "" {
			r.status = msg
		}
	}
	return s.write(m, now, r)
}

// report is what step writes on the machine: auto-scaling's state, its
// word, its warnings.
type report struct {
	state    *machines.AutoscaleState
	status   string
	warnings []string
}

// write records r on m - when it changed, or once in a while for its
// time.
func (s *autoscaler) write(m *machines.Machine, now time.Time, r report) error {
	next := *r.state
	next.Mode, next.Status, next.Warnings = m.Spec.Autoscale.Mode, r.status, r.warnings
	cur := m.AutoscaleState
	if cur != nil && cur.Mode == next.Mode && cur.Status == next.Status && slices.Equal(cur.Warnings, next.Warnings) &&
		stateEqual(cur.State, next.State) && maps.Equal(cur.Sysctl, next.Sysctl) && now.Sub(cur.At) < statusEvery {
		return nil
	}
	next.At = now.UTC()
	_, err := s.a.machines.Update(m.ID, func(m *machines.Machine) error {
		if m.Spec.Autoscale == nil || m.Spec.Autoscale.Mode != next.Mode {
			return errSkip // changed meanwhile: the next tick starts over
		}
		m.AutoscaleState = &next
		return nil
	})
	if errors.Is(err, errSkip) {
		return nil
	}
	return err
}

func stateEqual(a, b autoscale.State) bool {
	return a.Size == b.Size && a.LastUp.Equal(b.LastUp) && slices.EqualFunc(a.Steps, b.Steps, time.Time.Equal) &&
		a.OOMKills == b.OOMKills && a.OOMCounted == b.OOMCounted
}

// statusText is auto-scaling's word for the machine: what it just did,
// why it waits, or what it watches.
func statusText(d autoscale.Decision, samples []autoscale.Sample, size autoscale.Size, mode string) string {
	verb := map[autoscale.Action]string{autoscale.Up: "grew", autoscale.Down: "shrank"}[d.Action]
	if mode == machines.AutoscaleObserve {
		verb = map[autoscale.Action]string{autoscale.Up: "would grow", autoscale.Down: "would shrink"}[d.Action]
	}
	switch {
	case d.Action != autoscale.None:
		return fmt.Sprintf("%s to %s: %s", verb, d.To, d.Reason)
	case d.Waiting != "":
		return d.Waiting
	}
	last := samples[len(samples)-1]
	what := fmt.Sprintf("watching at %s", size)
	if mode == machines.AutoscaleObserve {
		what = fmt.Sprintf("observing - its size would be %s", size)
	}
	if last.IdlePercent >= 0 {
		what += fmt.Sprintf(" - HAProxy %.0f %% idle", last.IdlePercent)
	}
	if last.MemTotalMiB > 0 {
		what += fmt.Sprintf(", %d %% of the memory available", last.MemAvailableMiB*100/last.MemTotalMiB)
	}
	return what
}

// sampleOf turns a reading into a sample - the CPUs' busy and steal
// shares since the reading before, when there's one of the same CPUs.
// Steal isn't busy: it is the time the vCPUs waited for the host.
func sampleOf(l, prev *nodeproxy.Load, now time.Time) autoscale.Sample {
	s := autoscale.Sample{
		At: now, CPUs: l.CPUs, IdlePercent: l.IdlePercent, BusyPercent: -1, StealPercent: -1,
		MemTotalMiB: int(l.MemTotal >> 20), MemAvailableMiB: int(l.MemAvailable >> 20),
		OOMKills: l.OOMKills, CurrConns: l.CurrConns, MaxConn: l.MaxConn,
	}
	if prev != nil && l.CPUTotalTicks > prev.CPUTotalTicks && l.CPUIdleTicks >= prev.CPUIdleTicks && l.CPUStealTicks >= prev.CPUStealTicks {
		total, idle, steal := l.CPUTotalTicks-prev.CPUTotalTicks, l.CPUIdleTicks-prev.CPUIdleTicks, l.CPUStealTicks-prev.CPUStealTicks
		if idle+steal <= total {
			s.BusyPercent = 100 * float64(total-idle-steal) / float64(total)
			s.StealPercent = 100 * float64(steal) / float64(total)
		}
	}
	return s
}

// maxWindow is the longest window the thresholds look at.
func maxWindow(t autoscale.Thresholds) time.Duration {
	return max(t.CPUUpAfter, t.MemoryUpAfter, t.ReturnAfter)
}

// autoscaleView is GET /api/machines/{id}/autoscale.
type autoscaleView struct {
	Policy   *machines.Autoscale      `json:"policy"`
	State    *machines.AutoscaleState `json:"state,omitempty"`
	Size     *machines.Size           `json:"size,omitempty"`
	Journal  []machines.JournalEntry  `json:"journal"`
	Defaults autoscaleDefaults        `json:"defaults"`
}

// autoscaleDefaults are the thresholds a policy's zeros take, as a
// policy gives them.
type autoscaleDefaults struct {
	CPUUpIdlePercent           int `json:"cpu_up_idle_percent"`
	CPUUpBusyPercent           int `json:"cpu_up_busy_percent"`
	CPUUpAfterSeconds          int `json:"cpu_up_after_seconds"`
	CPUDownIdlePercent         int `json:"cpu_down_idle_percent"`
	MemoryUpAvailablePercent   int `json:"memory_up_available_percent"`
	MemoryUpAfterSeconds       int `json:"memory_up_after_seconds"`
	MemoryDownAvailablePercent int `json:"memory_down_available_percent"`
	ReturnAfterSeconds         int `json:"return_after_seconds"`
	CooldownSeconds            int `json:"cooldown_seconds"`
	MaxStepsPerHour            int `json:"max_steps_per_hour"`
}

func defaultsView() autoscaleDefaults {
	d := autoscale.Defaults
	return autoscaleDefaults{
		CPUUpIdlePercent: d.CPUUpIdlePercent, CPUUpBusyPercent: d.CPUUpBusyPercent, CPUUpAfterSeconds: int(d.CPUUpAfter.Seconds()),
		CPUDownIdlePercent: d.CPUDownIdlePercent, MemoryUpAvailablePercent: d.MemoryUpAvailablePercent,
		MemoryUpAfterSeconds: int(d.MemoryUpAfter.Seconds()), MemoryDownAvailablePercent: d.MemoryDownAvailablePercent,
		ReturnAfterSeconds: int(d.ReturnAfter.Seconds()), CooldownSeconds: int(d.Cooldown.Seconds()), MaxStepsPerHour: d.MaxStepsPerHour,
	}
}

// handleMachineAutoscale answers a machine's auto-scaling: its policy,
// what it remembers, its journal (?n=, 200 by default) - and the
// defaults, for a form.
func (a *app) handleMachineAutoscale(w http.ResponseWriter, r *http.Request) {
	m, ok := a.getMachine(w, r)
	if !ok {
		return
	}
	n := 200
	if v := r.URL.Query().Get("n"); v != "" {
		if _, err := fmt.Sscan(v, &n); err != nil || n < 1 || n > 1000 {
			writeError(w, http.StatusBadRequest, "n: 1 to 1000")
			return
		}
	}
	journal, err := a.machines.Journal(m.ID, n)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, autoscaleView{Policy: m.Spec.Autoscale, State: m.AutoscaleState, Size: m.Size, Journal: journal, Defaults: defaultsView()})
}

// What auto-scaling reads and changes, on the real nodes.
func (realNodeAPI) Load(ctx context.Context, node *store.Node) (*nodeproxy.Load, error) {
	return nodeproxy.ReadLoad(ctx, node)
}

func (realNodeAPI) HAProxyConfig(ctx context.Context, node *store.Node) ([]byte, error) {
	return nodeproxy.HAProxyConfig(ctx, node)
}

func (realNodeAPI) SysctlList(ctx context.Context, node *store.Node) (*janusv1alpha1.SysctlListResponse, error) {
	return nodeproxy.SysctlList(ctx, node)
}

func (realNodeAPI) SysctlHistory(ctx context.Context, node *store.Node, limit uint32) ([]*janusv1alpha1.SysctlHistoryEntry, error) {
	return nodeproxy.SysctlHistory(ctx, node, limit)
}

func (realNodeAPI) SysctlApply(ctx context.Context, node *store.Node, req *janusv1alpha1.SysctlApplyRequest) (*janusv1alpha1.SysctlApplyResponse, error) {
	return nodeproxy.SysctlApply(ctx, node, req)
}

func (realNodeAPI) SysctlConfirm(ctx context.Context, node *store.Node) error {
	return nodeproxy.SysctlConfirm(ctx, node)
}
