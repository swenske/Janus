package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/autoscale"
	"github.com/swenske/Janus/dashboard/backend/internal/hypervisor"
	"github.com/swenske/Janus/dashboard/backend/internal/machines"
	"github.com/swenske/Janus/dashboard/backend/internal/nodeproxy"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
)

// Auto-scaling's steps, its mode "on": the hypervisor plugs vCPUs or
// memory into the running machine, or takes them back
// (hypervisor.Driver.Resize), the node onlines them (internal/hotplug)
// - and auto-scaling waits until it sees them, then journals what the
// machine really has.

// resizeTimeout bounds a step: Proxmox VE takes its memory modules out
// one every 3 s.
const resizeTimeout = 3 * time.Minute

// settleTimeout is how long the node may take to show its new size.
const settleTimeout = 90 * time.Second

// hostReserveMiB is the memory a host keeps free whatever auto-scaling
// does: a twentieth of it, 512 MiB at least.
func hostReserveMiB(totalMiB int) int { return max(512, totalMiB/20) }

// resize takes step d on m: checks the host has room for the memory it
// adds, resizes, waits until the node shows it, journals the step with
// what the machine has after it - and reads its record again. It returns
// what the machine has (d.From when nothing changed).
func (s *autoscaler) resize(ctx context.Context, m *machines.Machine, node *store.Node, sm *scaledMachine, d autoscale.Decision, now time.Time) (autoscale.Size, error) {
	journal := func(got autoscale.Size, err error) {
		e := machines.JournalEntry{Time: now.UTC(), Mode: m.Spec.Autoscale.Mode, Action: string(d.Action), Resource: d.Resource, From: d.From, To: got, Reason: d.Reason}
		if err != nil {
			e.Error = err.Error()
			if got == d.From && e.Error == sm.lastRefusal {
				return // the same refusal as last time: once is enough
			}
			sm.lastRefusal = e.Error
		} else {
			sm.lastRefusal = ""
		}
		if jerr := s.a.machines.AppendJournal(m.ID, e); jerr != nil {
			log.Printf("machine %s: auto-scaling journal: %v", m.ID, jerr)
		}
	}
	h, ok := s.a.hypervisors.Get(m.Spec.HypervisorID)
	if !ok || m.Ref == nil || m.Ref.UUID == "" {
		err := errors.New("its hypervisor or virtual machine is gone")
		journal(d.From, err)
		return d.From, err
	}
	if grow := d.To.MemoryMiB - d.From.MemoryMiB; grow > 0 {
		if err := s.hostRoom(ctx, h, grow); err != nil {
			journal(d.From, err)
			return d.From, err
		}
	}
	drv, err := newDriver(h, s.a.controllerID)
	if err != nil {
		journal(d.From, err)
		return d.From, err
	}
	defer drv.Close()

	s.mu.Lock()
	if s.m[m.ID] == nil {
		s.m[m.ID] = sm // what resizing looks at
	}
	sm.resizing = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		sm.resizing = false
		s.mu.Unlock()
	}()

	rctx, cancel := context.WithTimeout(ctx, resizeTimeout)
	defer cancel()
	// What the node shows before: the memory moves from it.
	before, err := nodes.Load(rctx, node)
	if err == nil {
		err = drv.Resize(rctx, *m.Ref, d.To.VCPUs, d.To.MemoryMiB)
	}
	if err == nil {
		err = settled(rctx, node, before, d)
	}
	got := d.From
	if st, serr := drv.MachineStatus(rctx, *m.Ref); serr == nil {
		got = autoscale.Size{VCPUs: st.VCPUs, MemoryMiB: st.MemoryMiB}
	} else if err == nil {
		got = d.To
	}
	journal(got, err)

	s.a.hvStatus.forget(h.ID)
	sctx, scancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer scancel()
	_ = s.a.runner.syncAs(sctx, m.ID, "changed outside the Controller")
	return got, err
}

// settled waits until node shows d's size: its CPUs online, its memory
// moved by what d adds or takes - from what it showed before.
func settled(ctx context.Context, node *store.Node, before *nodeproxy.Load, d autoscale.Decision) error {
	delta := d.To.MemoryMiB - d.From.MemoryMiB
	want := int(before.MemTotal>>20) + delta
	slack := max(16, abs(delta)/10)
	deadline := time.Now().Add(settleTimeout)
	var last *nodeproxy.Load
	for {
		l, err := nodes.Load(ctx, node)
		if err == nil {
			last = l
			if l.CPUs == d.To.VCPUs && abs(int(l.MemTotal>>20)-want) <= slack {
				return nil
			}
		}
		if time.Now().After(deadline) {
			if last == nil {
				return fmt.Errorf("the node didn't answer after the resize: %v", err)
			}
			return fmt.Errorf("after %s the node shows %d vCPU and %d MiB, not %d vCPU and about %d MiB", settleTimeout, last.CPUs, last.MemTotal>>20, d.To.VCPUs, want)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(settlePoll):
		}
	}
}

// settlePoll: how often settled reads the node - a variable for tests.
var settlePoll = 2 * time.Second

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// hostRoom says why the host can't give growMiB more to a machine: what
// it has free, minus what it keeps free.
func (s *autoscaler) hostRoom(ctx context.Context, h *hypervisor.Hypervisor, growMiB int) error {
	st := s.a.hypervisorStatus(ctx, h)
	if st.Host == nil || st.Host.MemoryTotal == 0 {
		return fmt.Errorf("the host's free memory isn't known (%s): no memory added", st.Error)
	}
	free, total := int(st.Host.MemoryFree>>20), int(st.Host.MemoryTotal>>20)
	if keep := hostReserveMiB(total); free-growMiB < keep {
		return fmt.Errorf("the host has %d MiB free: %d more would leave less than the %d MiB it keeps free", free, growMiB, keep)
	}
	return nil
}

// resizing reports whether a step is under way on machine id: its size
// is moving, the sync doesn't take it for a change made elsewhere.
func (s *autoscaler) resizing(id string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sm := s.m[id]
	return sm != nil && sm.resizing
}
