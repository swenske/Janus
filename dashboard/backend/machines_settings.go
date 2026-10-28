package main

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/hypervisor"
	"github.com/swenske/Janus/dashboard/backend/internal/machines"
)

// A machine's hypervisor settings beyond its size and interfaces - its
// CPU model, its guest agent's option (hypervisor.Settings, Drift) - are
// set when it's created. A machine an older Controller created, or one
// changed on the hypervisor, may not have today's: Proxmox VE machines
// got the kvm64 CPU model, without AES-NI, until v2026.10.28 (#372), and
// never the guest agent's option (#373). The sync reports the difference
// (Machine.HardwareDrift) and never adopts it; it's applied only when an
// admin asks (POST /api/machines/{id}/apply-settings): it takes a stop
// and a start of the virtual machine - a reboot from inside keeps the
// same QEMU process -, and a Controller update must never restart every
// older machine by itself (kluster-prod).

// machineDrift is how a virtual machine's settings, as its hypervisor
// reports them, differ from what the Controller gives a machine of spec
// today.
func machineDrift(st *hypervisor.MachineStatus, spec machines.Spec) []hypervisor.Drift {
	out := slices.Clone(st.Drift)
	if want := spec.GuestAgent(); st.GuestAgent != nil && *st.GuestAgent != want {
		out = append(out, hypervisor.Drift{Setting: "guest-agent", Have: onOff(*st.GuestAgent), Want: onOff(want)})
	}
	return out
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// driftText is drift as a machine's history says it.
func driftText(drift []hypervisor.Drift) string {
	parts := make([]string, len(drift))
	for i, d := range drift {
		parts[i] = d.String()
	}
	return strings.Join(parts, "; ")
}

// syncDrift records what st says of m's settings, saying so in its
// history when that changes - once, not at every sync.
func syncDrift(m *machines.Machine, st *hypervisor.MachineStatus) {
	drift := machineDrift(st, m.Spec)
	if slices.Equal(drift, m.HardwareDrift) {
		return
	}
	switch {
	case len(drift) > 0:
		m.Log("its hypervisor settings aren't the ones the Controller gives machines today (%s): applying them restarts it cold - its page's Apply settings, once a cluster member is drained", driftText(drift))
	default:
		m.Log("its hypervisor settings are the ones the Controller gives machines today")
	}
	m.HardwareDrift = drift
}

// undrainedMember says why m's node mustn't be stopped now - it's a
// cluster member in service - or "".
func (a *app) undrainedMember(ctx context.Context, m *machines.Machine) string {
	if a.clusters == nil || m.NodeID == "" {
		return ""
	}
	id := a.clusters.Store.ClusterOf(m.NodeID)
	c, ok := a.clusters.Store.Get(id)
	if id == "" || !ok {
		return ""
	}
	mem := c.MemberOf(m.NodeID)
	if mem == nil {
		return ""
	}
	drained := a.clusters.Store.State(id).Members[mem.Name].Drained
	// The node's own word when it answers: a reboot put a drained member
	// back in service (#356).
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	if info, err := a.clusters.Nodes.ClusterGet(cctx, *mem); err == nil {
		drained = info.GetDrained()
	}
	cancel()
	if drained {
		return ""
	}
	return fmt.Sprintf("%s is %s, a member of cluster %s in service: drain it first (its cluster's page, or POST /api/clusters/%s/members/%s/drain), apply its settings, then undrain it", m.Spec.Name, mem.Name, c.Name, c.ID, mem.Name)
}

// handleMachineApplySettings gives a ready machine the hypervisor
// settings its sync found different (Machine.HardwareDrift): its node
// shuts down cleanly, the virtual machine gets them and starts again, at
// its nominal size - the update job's hardware step. 200 with nothing to
// do, 202 once started; refused for a cluster member in service.
func (a *app) handleMachineApplySettings(w http.ResponseWriter, r *http.Request) {
	m, ok := a.getMachine(w, r)
	if !ok {
		return
	}
	if m.Phase != machines.PhaseReady || m.NodeID == "" || m.Ref == nil || m.Ref.UUID == "" {
		writeError(w, http.StatusConflict, fmt.Sprintf("the machine is %s: only a ready machine with its node can be restarted with its settings", m.Phase))
		return
	}
	if _, ok := a.store.Get(m.NodeID); !ok {
		writeError(w, http.StatusConflict, "the machine's node is gone")
		return
	}
	if a.runner.busy(m.ID) {
		writeError(w, http.StatusConflict, "the machine is busy")
		return
	}
	// What the hypervisor says now, not a minute ago.
	a.hvStatus.forget(m.Spec.HypervisorID)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	_ = a.runner.sync(ctx, m.ID)
	cancel()
	m, _ = a.machines.Get(m.ID)
	if len(m.HardwareDrift) == 0 {
		writeJSON(w, http.StatusOK, a.machineView(m))
		return
	}
	if msg := a.undrainedMember(r.Context(), m); msg != "" {
		writeError(w, http.StatusConflict, msg)
		return
	}
	id := m.ID
	p := &updatePlan{next: m.Spec, version: m.Version, image: m.Spec.Schematic(), settings: true}
	if err := a.runner.phase(id, machines.PhaseUpdating, "applying its hypervisor settings (%s)", driftText(m.HardwareDrift)); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !a.runner.start(id, func(ctx context.Context) error { return a.runner.update(ctx, id, p) }) {
		writeError(w, http.StatusConflict, "the machine is busy")
		return
	}
	m, _ = a.machines.Get(id)
	writeJSON(w, http.StatusAccepted, a.machineView(m))
}
