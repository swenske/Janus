package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"

	"github.com/swenske/Janus/dashboard/backend/internal/autoscale"
	"github.com/swenske/Janus/dashboard/backend/internal/hypervisor"
	"github.com/swenske/Janus/dashboard/backend/internal/machines"
	"github.com/swenske/Janus/dashboard/backend/internal/nodeproxy"
)

var observePolicy = map[string]any{"mode": "observe", "max_vcpus": 4, "max_memory_mib": 4096}

// A policy's maxima restart the machine once, with room for them; its
// mode and thresholds are just the record; removing it restarts it fixed.
func TestAutoscalePolicy(t *testing.T) {
	a, fake := newTestApp(t)
	m, f := readyMachine(t, a, addTrustedHypervisor(t, a), fake)

	for _, bad := range []map[string]any{
		{"mode": "observe", "max_vcpus": 1, "max_memory_mib": 4096},                              // below its vCPUs
		{"mode": "observe", "max_vcpus": 2, "max_memory_mib": 1024},                              // nothing to grow to
		{"mode": "observe", "max_vcpus": 4, "max_memory_mib": 4096, "cpu_down_idle_percent": 20}, // shrinks into its up threshold
		{"mode": "observe", "max_vcpus": 4, "max_memory_mib": 4097},                              // virtio-mem blocks
		{"mode": "sometimes", "max_vcpus": 4, "max_memory_mib": 4096},
	} {
		if code, body := patch(t, a, m.ID, map[string]any{"autoscale": bad}); code != http.StatusBadRequest {
			t.Errorf("%v: %d %s", bad, code, body)
		}
	}

	if code, body := patch(t, a, m.ID, map[string]any{"autoscale": observePolicy}); code != http.StatusAccepted {
		t.Fatalf("PATCH: %d %s", code, body)
	}
	got := waitReady(t, a, m.ID)
	want := hypervisor.Hardware{VCPUs: 2, MemoryMiB: 1024, MaxVCPUs: 4, MaxMemoryMiB: 4096}
	if got.Error != "" || got.Spec.Autoscale == nil || got.Spec.Autoscale.Mode != "observe" || len(fake.hardware) != 1 || fake.hardware[0] != want {
		t.Fatalf("after the policy: %+v, reconfigured %v", got, fake.hardware)
	}
	if f.shutdowns != 1 || !strings.Contains(eventsOf(got), "auto-scaling observing") || !strings.Contains(eventsOf(got), "room for 4 vCPU, 4096 MiB") {
		t.Errorf("shutdowns %d, events:\n%s", f.shutdowns, eventsOf(got))
	}
	if got.Spec.VCPUs != 2 || got.Size == nil || got.Size.MaxVCPUs != 4 || got.Size.MaxMemoryMiB != 4096 {
		t.Errorf("its spec or size after the restart: %+v %+v", got.Spec, got.Size)
	}

	// Thresholds only: the record, no restart.
	next := map[string]any{"mode": "observe", "max_vcpus": 4, "max_memory_mib": 4096, "cpu_up_idle_percent": 25, "return_after_seconds": 600}
	if code, body := patch(t, a, m.ID, map[string]any{"autoscale": next}); code != http.StatusOK {
		t.Fatalf("thresholds: %d %s", code, body)
	}
	got, _ = a.machines.Get(m.ID)
	if got.Spec.Autoscale.CPUUpIdlePercent != 25 || len(fake.hardware) != 1 || f.shutdowns != 1 {
		t.Errorf("thresholds: %+v, reconfigured %d times", got.Spec.Autoscale, len(fake.hardware))
	}
	// The same again: nothing.
	if code, _ := patch(t, a, m.ID, map[string]any{"autoscale": next}); code != http.StatusOK || len(fake.hardware) != 1 {
		t.Errorf("the same policy again: %d, reconfigured %d times", code, len(fake.hardware))
	}

	// Removed: started again without room to grow.
	if code, body := patch(t, a, m.ID, map[string]any{"autoscale": nil}); code != http.StatusAccepted {
		t.Fatalf("removing it: %d %s", code, body)
	}
	got = waitReady(t, a, m.ID)
	if got.Spec.Autoscale != nil || len(fake.hardware) != 2 || fake.hardware[1] != (hypervisor.Hardware{VCPUs: 2, MemoryMiB: 1024}) {
		t.Errorf("removed: %+v, reconfigured %v", got.Spec.Autoscale, fake.hardware)
	}
}

// scalableMachine is readyMachine with an observing policy, started with
// its room to grow.
func scalableMachine(t *testing.T) (*app, *fakeDriver, *machines.Machine, *fakeNodes) {
	t.Helper()
	a, fake := newTestApp(t)
	m, f := readyMachine(t, a, addTrustedHypervisor(t, a), fake)
	if code, body := patch(t, a, m.ID, map[string]any{"autoscale": observePolicy}); code != http.StatusAccepted {
		t.Fatalf("PATCH: %d %s", code, body)
	}
	return a, fake, waitReady(t, a, m.ID), f
}

// Observing: the node's load read, the step it would take journaled and
// simulated - nothing resized.
func TestAutoscaleObserves(t *testing.T) {
	a, fake, m, f := scalableMachine(t)
	f.load = &nodeproxy.Load{CPUs: 2, MemTotal: 1024 << 20, MemAvailable: 600 << 20, IdlePercent: 10, MaxConn: 1000}
	s := newAutoscaler(a)
	a.autoscaler = s
	sm := &scaledMachine{}
	t0 := time.Now()
	var at time.Time
	for i := 0; i <= int(6*time.Minute/autoscale.SampleEvery); i++ {
		at = t0.Add(time.Duration(i) * autoscale.SampleEvery)
		m, _ = a.machines.Get(m.ID)
		if err := s.step(context.Background(), m, sm, at); err != nil {
			t.Fatal(err)
		}
	}
	m, _ = a.machines.Get(m.ID)
	journal, _ := a.machines.Journal(m.ID, 10)
	if len(journal) != 1 || journal[0].Mode != "observe" || journal[0].Action != "up" || journal[0].Resource != "cpu" ||
		journal[0].To != (autoscale.Size{VCPUs: 3, MemoryMiB: 1024}) || !strings.Contains(journal[0].Reason, "HAProxy idle 10 %") {
		t.Fatalf("journal %+v", journal)
	}
	st := m.AutoscaleState
	if st == nil || st.Mode != "observe" || st.Size.VCPUs != 3 {
		t.Fatalf("state %+v", st)
	}
	if len(fake.live) != 0 || m.Spec.VCPUs != 2 {
		t.Errorf("observing resized the machine: %v, spec %d vCPU", fake.live, m.Spec.VCPUs)
	}

	// 3 vCPUs would have been enough (HAProxy 40 % idle with them): it
	// stopped there. Busier - HAProxy 5 % idle on 3 -, but its threads
	// fixed: vCPUs can't help - said, not journaled.
	if !strings.Contains(m.AutoscaleState.Status, "its size would be 3 vCPU") {
		t.Errorf("status %q", m.AutoscaleState.Status)
	}
	f.mu.Lock()
	f.load.CPUs, f.load.IdlePercent = 3, 5
	f.mu.Unlock()
	f.haproxyCfg = []byte("global\n  nbthread 2\n")
	sm.threadAt = time.Time{}
	at = at.Add(10 * time.Minute) // past the cooldown
	for range 25 {
		at = at.Add(autoscale.SampleEvery)
		m, _ = a.machines.Get(m.ID)
		if err := s.step(context.Background(), m, sm, at); err != nil {
			t.Fatal(err)
		}
	}
	m, _ = a.machines.Get(m.ID)
	if !strings.Contains(m.AutoscaleState.Status, "nbthread") {
		t.Errorf("status %q", m.AutoscaleState.Status)
	}
	if journal, _ := a.machines.Journal(m.ID, 10); len(journal) != 1 {
		t.Errorf("journal %+v", journal)
	}

	// A node that doesn't answer: said, nothing decided.
	f.loadErr = context.DeadlineExceeded
	m, _ = a.machines.Get(m.ID)
	if err := s.step(context.Background(), m, sm, at.Add(autoscale.SampleEvery)); err != nil {
		t.Fatal(err)
	}
	m, _ = a.machines.Get(m.ID)
	if !strings.Contains(m.AutoscaleState.Status, "doesn't answer") || m.AutoscaleState.Size.VCPUs != 3 {
		t.Errorf("unanswered: %+v", m.AutoscaleState)
	}

	// GET /api/machines/{id}/autoscale: the policy, the state, the
	// journal and the defaults.
	rec := call(t, a.handleMachineAutoscale, "GET", "/api/machines/"+m.ID+"/autoscale", "GET /api/machines/{id}/autoscale", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"cpu_up_idle_percent":30`) || !strings.Contains(rec.Body.String(), `"action":"up"`) {
		t.Errorf("GET autoscale: %d %s", rec.Code, rec.Body.String())
	}
}

// A scalable machine's spec is its nominal size: a size its auto-scaling
// didn't give it is noted, not adopted.
func TestSyncKeepsTheNominalSize(t *testing.T) {
	a, fake, m, _ := scalableMachine(t)
	fake.mu.Lock()
	fake.hw.VCPUs = 3
	fake.mu.Unlock()
	a.hvStatus.forget(m.Spec.HypervisorID)
	if err := a.runner.sync(context.Background(), m.ID); err != nil {
		t.Fatal(err)
	}
	m, _ = a.machines.Get(m.ID)
	if m.Spec.VCPUs != 2 || m.Size.VCPUs != 3 || !strings.Contains(eventsOf(m), "not by its auto-scaling") {
		t.Errorf("spec %d vCPU, size %+v, events:\n%s", m.Spec.VCPUs, m.Size, eventsOf(m))
	}
	// Noted once.
	a.hvStatus.forget(m.Spec.HypervisorID)
	_ = a.runner.sync(context.Background(), m.ID)
	m, _ = a.machines.Get(m.ID)
	if n := strings.Count(eventsOf(m), "not by its auto-scaling"); n != 1 {
		t.Errorf("noted %d times", n)
	}
}

// run steps sm through d of samples from t0, every SampleEvery, and
// returns when it stopped.
func run(t *testing.T, a *app, s *autoscaler, sm *scaledMachine, id string, t0 time.Time, d time.Duration) time.Time {
	t.Helper()
	at := t0
	for ; !at.After(t0.Add(d)); at = at.Add(autoscale.SampleEvery) {
		m, _ := a.machines.Get(id)
		if err := s.step(context.Background(), m, sm, at); err != nil {
			t.Fatal(err)
		}
	}
	return at
}

func journalOf(t *testing.T, a *app, id string) []machines.JournalEntry {
	t.Helper()
	j, err := a.machines.Journal(id, 100)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// Its mode "on": the machine resized for real - memory refused when the
// host hasn't the room -, its record following; paused, back to its
// size.
func TestAutoscaleResizes(t *testing.T) {
	a, fake, m, f := scalableMachine(t)
	prev := settlePoll
	settlePoll = time.Millisecond
	t.Cleanup(func() { settlePoll = prev })
	on := map[string]any{"mode": "on", "max_vcpus": 4, "max_memory_mib": 4096}
	if code, body := patch(t, a, m.ID, map[string]any{"autoscale": on}); code != http.StatusOK {
		t.Fatalf("mode on: %d %s", code, body)
	}
	f.mu.Lock()
	f.followSize = true
	f.load = &nodeproxy.Load{CPUs: 2, MemTotal: 924 << 20, MemAvailable: 600 << 20, IdlePercent: 10, MaxConn: 1000}
	f.mu.Unlock()
	s := newAutoscaler(a)
	a.autoscaler = s
	sm := &scaledMachine{}
	at := run(t, a, s, sm, m.ID, time.Now(), 6*time.Minute)

	m, _ = a.machines.Get(m.ID)
	j := journalOf(t, a, m.ID)
	if len(fake.live) != 1 || fake.live[0] != [2]int{3, 1024} || len(j) != 1 || j[0].Mode != "on" || j[0].To != (autoscale.Size{VCPUs: 3, MemoryMiB: 1024}) || j[0].Error != "" {
		t.Fatalf("resized %v, journal %+v", fake.live, j)
	}
	if m.Spec.VCPUs != 2 || m.Size.VCPUs != 3 || m.AutoscaleState.Size.VCPUs != 3 || strings.Contains(eventsOf(m), "not by its auto-scaling") {
		t.Errorf("spec %d, size %+v, state %+v, events:\n%s", m.Spec.VCPUs, m.Size, m.AutoscaleState, eventsOf(m))
	}

	// Memory short: memory first - but the host hasn't the room: refused,
	// journaled once, nothing resized.
	fake.mu.Lock()
	fake.hostFree = 600 << 20
	fake.mu.Unlock()
	a.hvStatus.forget(m.Spec.HypervisorID)
	f.mu.Lock()
	f.load.MemAvailable, f.load.IdlePercent = 50<<20, 80
	f.mu.Unlock()
	at = run(t, a, s, sm, m.ID, at.Add(10*time.Minute), 3*time.Minute)
	j = journalOf(t, a, m.ID)
	if len(fake.live) != 1 || len(j) != 2 || !strings.Contains(j[1].Error, "the host has 600 MiB free") || j[1].To != j[1].From {
		t.Fatalf("no room: resized %v, journal %+v", fake.live, j)
	}
	// The host's free memory moving a little: the same refusal, not
	// journaled again (the lab's journal had one every 15 s).
	fake.mu.Lock()
	fake.hostFree = 590 << 20
	fake.mu.Unlock()
	a.hvStatus.forget(m.Spec.HypervisorID)
	at = run(t, a, s, sm, m.ID, at, time.Minute)
	if j = journalOf(t, a, m.ID); len(j) != 2 {
		t.Fatalf("the same refusal journaled again: %+v", j)
	}
	m, _ = a.machines.Get(m.ID)
	if !strings.Contains(m.AutoscaleState.Status, "couldn't grow") {
		t.Errorf("status %q", m.AutoscaleState.Status)
	}
	// With the room: 1024 to 1280 - the hypervisor's reading a moment
	// late, the journal says what the machine got.
	fake.mu.Lock()
	fake.hostFree, fake.statusLag = 0, 2
	fake.mu.Unlock()
	a.hvStatus.forget(m.Spec.HypervisorID)
	at = run(t, a, s, sm, m.ID, at, time.Minute)
	if len(fake.live) != 2 || fake.live[1] != [2]int{3, 1280} {
		t.Fatalf("with room: resized %v, journal %+v", fake.live, journalOf(t, a, m.ID))
	}
	if j = journalOf(t, a, m.ID); j[len(j)-1].To != (autoscale.Size{VCPUs: 3, MemoryMiB: 1280}) {
		t.Errorf("journaled %+v, the machine got 3 vCPU, 1280 MiB", j[len(j)-1])
	}
	fake.mu.Lock()
	fake.statusLag = 0
	fake.mu.Unlock()

	// Paused: back to its size at once.
	off := map[string]any{"mode": "off", "max_vcpus": 4, "max_memory_mib": 4096}
	if code, body := patch(t, a, m.ID, map[string]any{"autoscale": off}); code != http.StatusOK {
		t.Fatalf("pause: %d %s", code, body)
	}
	run(t, a, s, sm, m.ID, at, 0)
	m, _ = a.machines.Get(m.ID)
	j = journalOf(t, a, m.ID)
	if len(fake.live) != 3 || fake.live[2] != [2]int{2, 1024} || !strings.Contains(j[len(j)-1].Reason, "paused: back to its size") || m.Size.VCPUs != 2 {
		t.Errorf("paused: resized %v, journal %+v, size %+v", fake.live, j, m.Size)
	}
	if m.AutoscaleState.Status != "paused" {
		t.Errorf("status %q", m.AutoscaleState.Status)
	}
}

func param(name, value, recValue, rule string) *janusv1alpha1.SysctlParameter {
	p := &janusv1alpha1.SysctlParameter{Name: name, Class: janusv1alpha1.SysctlClass_SYSCTL_CLASS_EDITABLE, Value: value}
	if recValue != "" {
		p.Recommendation = &janusv1alpha1.SysctlRecommendation{Value: recValue, RuleId: rule}
	}
	return p
}

// The node's suggestions applied on trial and confirmed - never a
// parameter someone else confirmed, a key at most every 6 h but for the
// memory, back at its size; observing, journaled once.
func TestAutoscaleSysctl(t *testing.T) {
	a, _, m, f := scalableMachine(t)
	f.mu.Lock()
	f.load = &nodeproxy.Load{CPUs: 2, MemTotal: 924 << 20, MemAvailable: 600 << 20, IdlePercent: 80}
	f.sysctl = &janusv1alpha1.SysctlListResponse{Managed: true, Parameters: []*janusv1alpha1.SysctlParameter{
		param("net.ipv4.tcp_mem", "10872 14497 21744", "35238 46988 70477", "tcp_mem.follow_memory"),
		param("net.core.somaxconn", "60000", "16384", "somaxconn.memory"),
		param("net.ipv4.ip_local_port_range", "20000 30000", "10240 65023", "port_range.usage"),
	}}
	f.history = []*janusv1alpha1.SysctlHistoryEntry{{Action: "confirm", Actor: &janusv1alpha1.SysctlActor{Name: "admin"},
		Changes: []*janusv1alpha1.SysctlChange{{Name: "net.ipv4.ip_local_port_range"}}}}
	f.mu.Unlock()
	s := newAutoscaler(a)
	a.autoscaler = s
	sm := &scaledMachine{}
	now := time.Now()

	// Observing: journaled once, nothing applied.
	m, _ = a.machines.Get(m.ID)
	st := &machines.AutoscaleState{State: autoscale.State{Size: autoscale.Size{VCPUs: 2, MemoryMiB: 2048}}} // grown
	s.sysctl(context.Background(), m, nil, sm, st, true, now)
	s.sysctl(context.Background(), m, nil, sm, st, true, now)
	j := journalOf(t, a, m.ID)
	if len(j) != 1 || j[0].Action != "sysctl" || !strings.Contains(j[0].Reason, "net.ipv4.tcp_mem 10872 14497 21744 → 35238 46988 70477 (tcp_mem.follow_memory)") ||
		strings.Contains(j[0].Reason, "port_range") || len(f.trials) != 0 {
		t.Fatalf("observing: journal %+v, trials %d", j, len(f.trials))
	}

	// On: applied - the operator's port range left alone -, confirmed.
	m.Spec.Autoscale.Mode = machines.AutoscaleOn
	msg := s.sysctl(context.Background(), m, nil, sm, st, true, now)
	if len(f.trials) != 1 || len(f.trials[0].Changes) != 2 || f.confirms != 1 || !strings.HasPrefix(msg, "kernel parameters applied") {
		t.Fatalf("on: %d trials (%v), %d confirmations, %q", len(f.trials), f.trials, f.confirms, msg)
	}
	if !f.trials[0].ReloadHaproxy || st.Sysctl["net.ipv4.tcp_mem"].Before != "10872 14497 21744" || st.Sysctl["net.core.somaxconn"].After != "16384" {
		t.Errorf("recorded %+v", st.Sysctl)
	}

	// Ten minutes later, somaxconn suggested back (another rule): not
	// within 6 h. tcp_mem following the memory again: applied.
	f.mu.Lock()
	f.sysctl.Parameters[0].Recommendation = &janusv1alpha1.SysctlRecommendation{Value: "70476 93976 140954", RuleId: "tcp_mem.follow_memory"}
	f.sysctl.Parameters[1].Recommendation = &janusv1alpha1.SysctlRecommendation{Value: "32768", RuleId: "somaxconn.overflows"}
	f.mu.Unlock()
	s.sysctl(context.Background(), m, nil, sm, st, false, now.Add(11*time.Minute))
	if len(f.trials) != 2 || len(f.trials[1].Changes) != 1 || f.trials[1].Changes[0].GetName() != "net.ipv4.tcp_mem" {
		t.Fatalf("second round: %v", f.trials)
	}
	if st.Sysctl["net.ipv4.tcp_mem"].Before != "10872 14497 21744" {
		t.Errorf("the value before auto-scaling lost: %+v", st.Sysctl["net.ipv4.tcp_mem"])
	}

	// Back at its size: what followed the memory goes back; somaxconn
	// (another rule) stays.
	st.Size.MemoryMiB = m.Spec.MemoryMiB
	f.mu.Lock()
	f.sysctl.Parameters[1].Recommendation = nil
	f.mu.Unlock()
	s.sysctl(context.Background(), m, nil, sm, st, true, now.Add(12*time.Minute))
	last := f.trials[len(f.trials)-1]
	if len(f.trials) != 3 || len(last.Changes) != 1 || last.Changes[0].GetName() != "net.ipv4.tcp_mem" || last.Changes[0].GetValue() != "10872 14497 21744" {
		t.Fatalf("back at its size: %v", f.trials)
	}
	if back := st.Sysctl["net.ipv4.tcp_mem"]; back.Before != back.After || back.After != "10872 14497 21744" || st.Sysctl["net.core.somaxconn"].After != "16384" {
		t.Errorf("after going back: %+v", st.Sysctl)
	}

	// Someone sets file-max by hand: it's theirs from then on - not put
	// back, not changed again.
	f.mu.Lock()
	f.history = append([]*janusv1alpha1.SysctlHistoryEntry{{Action: "confirm", Actor: &janusv1alpha1.SysctlActor{Name: "admin"},
		Changes: []*janusv1alpha1.SysctlChange{{Name: "net.core.somaxconn", NewValue: "50000"}}}}, f.history...)
	f.sysctl.Parameters[1].Value = "50000"
	f.sysctl.Parameters[1].Recommendation = &janusv1alpha1.SysctlRecommendation{Value: "16384", RuleId: "somaxconn.memory"}
	f.mu.Unlock()
	trials := len(f.trials)
	s.sysctl(context.Background(), m, nil, sm, st, true, now.Add(7*time.Hour))
	if len(f.trials) != trials {
		t.Errorf("a parameter someone set was changed: %v", f.trials[len(f.trials)-1])
	}

	// Refused by the node: journaled, nothing recorded.
	f.mu.Lock()
	f.refuse = "out of bounds"
	f.sysctl.Parameters[0].Recommendation = &janusv1alpha1.SysctlRecommendation{Value: "1 2 3", RuleId: "tcp_mem.follow_memory"}
	f.mu.Unlock()
	msg = s.sysctl(context.Background(), m, nil, sm, st, true, now.Add(13*time.Minute))
	j = journalOf(t, a, m.ID)
	if !strings.Contains(msg, "refused them: out of bounds") || !strings.Contains(j[len(j)-1].Error, "out of bounds") {
		t.Errorf("refused: %q, %+v", msg, j[len(j)-1])
	}
}

// TestSampleSteal: the time the vCPUs waited for the host (steal) isn't
// busy time - it is told apart, so a host short of CPU doesn't ask for a
// vCPU more.
func TestSampleSteal(t *testing.T) {
	now := time.Now()
	prev := &nodeproxy.Load{CPUs: 2, CPUTotalTicks: 1000, CPUIdleTicks: 300, CPUStealTicks: 100}
	// The lab's: 20 % idle, 50 % steal, 30 % at work.
	l := &nodeproxy.Load{CPUs: 2, CPUTotalTicks: 2000, CPUIdleTicks: 500, CPUStealTicks: 600}
	s := sampleOf(l, prev, now)
	if s.BusyPercent != 30 || s.StealPercent != 50 {
		t.Errorf("busy %v, steal %v; want 30, 50", s.BusyPercent, s.StealPercent)
	}
	// A node before v2026.10.14-2 says no steal: busy as before.
	prev.CPUStealTicks, l.CPUStealTicks = 0, 0
	if s := sampleOf(l, prev, now); s.BusyPercent != 80 || s.StealPercent != 0 {
		t.Errorf("without steal: busy %v, steal %v; want 80, 0", s.BusyPercent, s.StealPercent)
	}
	// The first reading: unknown.
	if s := sampleOf(l, nil, now); s.BusyPercent != -1 || s.StealPercent != -1 {
		t.Errorf("first reading: busy %v, steal %v; want -1, -1", s.BusyPercent, s.StealPercent)
	}
}

// What followed the memory goes back when auto-scaling stops resizing:
// paused - the machine back at its size, tcp_mem put back too
// (v2026.10.14-3's lab left it sized for 1280 MiB on 1024) -, and before
// the restart that removes the policy.
func TestAutoscalePutsBack(t *testing.T) {
	a, _, m, f := scalableMachine(t)
	raised := func() {
		f.mu.Lock()
		f.load = &nodeproxy.Load{CPUs: 2, MemTotal: 924 << 20, MemAvailable: 600 << 20, IdlePercent: 80}
		f.sysctl = &janusv1alpha1.SysctlListResponse{Managed: true, Parameters: []*janusv1alpha1.SysctlParameter{
			param("net.ipv4.tcp_mem", "13914 18554 27829", "", ""),
			param("net.core.somaxconn", "4096", "", ""),
		}}
		// Auto-scaling's own confirmation of the raise, the newest.
		f.history = append([]*janusv1alpha1.SysctlHistoryEntry{{Action: "confirm", Actor: &janusv1alpha1.SysctlActor{Name: "controller-service"},
			Changes: []*janusv1alpha1.SysctlChange{{Name: "net.ipv4.tcp_mem", NewValue: "13914 18554 27829"}}}}, f.history...)
		f.mu.Unlock()
		if _, err := a.machines.Update(m.ID, func(m *machines.Machine) error {
			m.AutoscaleState = &machines.AutoscaleState{Mode: m.Spec.Autoscale.Mode, Sysctl: map[string]machines.SysctlChange{
				"net.ipv4.tcp_mem":   {Before: "10869 14493 21738", After: "13914 18554 27829", Rule: "tcp_mem.follow_memory"},
				"net.core.somaxconn": {Before: "60000", After: "4096", Rule: "somaxconn.memory"},
			}}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	raised()
	s := newAutoscaler(a)
	a.autoscaler = s

	// Paused: a new mode, the record kept - tcp_mem goes back, somaxconn
	// (another rule) stays.
	off := map[string]any{"mode": "off", "max_vcpus": 4, "max_memory_mib": 4096, "sysctl": true}
	if code, body := patch(t, a, m.ID, map[string]any{"autoscale": off}); code != http.StatusOK {
		t.Fatalf("pause: %d %s", code, body)
	}
	m, _ = a.machines.Get(m.ID)
	if err := s.step(context.Background(), m, &scaledMachine{}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(f.trials) != 1 || len(f.trials[0].Changes) != 1 || f.trials[0].Changes[0].GetName() != "net.ipv4.tcp_mem" ||
		f.trials[0].Changes[0].GetValue() != "10869 14493 21738" || f.confirms != 1 {
		t.Fatalf("paused: trials %v, %d confirmations", f.trials, f.confirms)
	}
	m, _ = a.machines.Get(m.ID)
	if st := m.AutoscaleState; st == nil || !strings.Contains(st.Status, "paused - kernel parameters applied") ||
		st.Sysctl["net.ipv4.tcp_mem"].After != "10869 14493 21738" || st.Sysctl["net.core.somaxconn"].After != "4096" {
		t.Errorf("paused: %+v", m.AutoscaleState)
	}
	// Once put back, nothing more.
	if err := s.step(context.Background(), m, &scaledMachine{}, time.Now()); err != nil || len(f.trials) != 1 {
		t.Errorf("again: %v, %d trials", err, len(f.trials))
	}

	// Removed: put back before the restart - nobody would afterwards.
	raised()
	if code, body := patch(t, a, m.ID, map[string]any{"autoscale": nil}); code != http.StatusAccepted {
		t.Fatalf("removing it: %d %s", code, body)
	}
	got := waitReady(t, a, m.ID)
	if len(f.trials) != 2 || f.trials[1].Changes[0].GetValue() != "10869 14493 21738" || !strings.Contains(eventsOf(got), "auto-scaling: kernel parameters applied") {
		t.Errorf("removed: trials %v, events:\n%s", f.trials, eventsOf(got))
	}
}
