package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

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
		{"mode": "on", "max_vcpus": 4, "max_memory_mib": 4096},                                   // not yet
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
