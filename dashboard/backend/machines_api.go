package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/hypervisor"
	"github.com/swenske/Janus/dashboard/backend/internal/machines"
	"github.com/swenske/Janus/dashboard/backend/internal/nodeproxy"
)

// The machines API is resource-shaped on purpose - what a Terraform
// provider (docs/hypervisors.md) needs: POST answers 202 with the
// machine, whose phase is then polled with GET until "ready" or
// "failed"; GET gives back the spec as created (MAC addresses filled in)
// with the observed state; DELETE answers 202 and the machine
// disappears (404) once destroyed.

func (a *app) registerMachineRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/machines", a.requireAuth(a.handleMachineList))
	mux.HandleFunc("POST /api/machines", a.requireAuth(a.handleMachineCreate))
	mux.HandleFunc("GET /api/machines/{id}", a.requireAuth(a.handleMachineGet))
	mux.HandleFunc("PATCH /api/machines/{id}", a.requireAuth(a.handleMachineUpdate))
	mux.HandleFunc("DELETE /api/machines/{id}", a.requireAuth(a.handleMachineDelete))
	mux.HandleFunc("POST /api/machines/{id}/retry", a.requireAuth(a.handleMachineRetry))
	mux.HandleFunc("POST /api/machines/{id}/power", a.requireAuth(a.handleMachinePower))
	mux.HandleFunc("GET /api/machines/{id}/console", a.requireAuth(a.handleMachineConsole))
	mux.HandleFunc("GET /api/catalog", a.requireAuth(a.handleCatalog))
}

type machineView struct {
	ID             string           `json:"id"`
	Spec           machines.Spec    `json:"spec"`
	HypervisorName string           `json:"hypervisor_name"`
	Phase          machines.Phase   `json:"phase"`
	Error          string           `json:"error,omitempty"`
	Version        string           `json:"version,omitempty"`
	Schematic      string           `json:"schematic,omitempty"`
	VMName         string           `json:"vm_name,omitempty"`
	VMUUID         string           `json:"vm_uuid,omitempty"`
	NodeID         string           `json:"node_id,omitempty"`
	NodeAddress    string           `json:"node_address,omitempty"`
	NodeHostname   string           `json:"node_hostname,omitempty"`
	SyncedAt       time.Time        `json:"synced_at,omitzero"`
	SyncError      string           `json:"sync_error,omitempty"`
	Warning        string           `json:"warning,omitempty"`
	Events         []machines.Event `json:"events"`
	CreatedAt      time.Time        `json:"created_at"`
	UpdatedAt      time.Time        `json:"updated_at"`
}

func (a *app) machineView(m *machines.Machine) machineView {
	v := machineView{
		ID: m.ID, Spec: m.Spec, Phase: m.Phase, Error: m.Error, Version: m.Version, Schematic: m.Schematic,
		NodeID: m.NodeID, Events: m.Events, CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt,
		NodeHostname: m.NodeHostname, SyncedAt: m.SyncedAt, SyncError: m.SyncError, Warning: m.Warning,
	}
	if h, ok := a.hypervisors.Get(m.Spec.HypervisorID); ok {
		v.HypervisorName = h.Name
	}
	if m.Ref != nil {
		v.VMName, v.VMUUID = m.Ref.Name, m.Ref.UUID
	}
	if n, ok := a.store.Get(m.NodeID); ok {
		v.NodeAddress = n.Addr()
	}
	return v
}

func (a *app) handleMachineList(w http.ResponseWriter, _ *http.Request) {
	out := []machineView{}
	for _, m := range a.machines.List() {
		out = append(out, a.machineView(m))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *app) getMachine(w http.ResponseWriter, r *http.Request) (*machines.Machine, bool) {
	m, ok := a.machines.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "no such machine")
	}
	return m, ok
}

// handleMachineGet answers a machine; ?refresh=true reads it from its
// node and hypervisor first, unless it was just read - what the
// Terraform provider asks before a plan, to see changes made elsewhere.
func (a *app) handleMachineGet(w http.ResponseWriter, r *http.Request) {
	m, ok := a.getMachine(w, r)
	if !ok {
		return
	}
	if r.URL.Query().Get("refresh") == "true" && time.Since(m.SyncedAt) > syncFresh {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		_ = a.runner.sync(ctx, m.ID)
		cancel()
		m, _ = a.machines.Get(m.ID)
	}
	writeJSON(w, http.StatusOK, a.machineView(m))
}

func (a *app) handleMachineCreate(w http.ResponseWriter, r *http.Request) {
	var spec machines.Spec
	if !decodeBody(w, r, &spec) {
		return
	}
	a.createMu.Lock() // checkSpec's name check and Add, together
	defer a.createMu.Unlock()
	if err := a.checkSpec(&spec); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	m := &machines.Machine{Spec: spec, Phase: machines.PhasePending}
	m.Log("requested: %d vCPU, %d MiB on %s", spec.VCPUs, spec.MemoryMiB, spec.HypervisorID)
	if err := a.machines.Add(m); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.runner.start(m.ID, func(ctx context.Context) error { return a.runner.create(ctx, m.ID) })
	writeJSON(w, http.StatusAccepted, a.machineView(m))
}

// handleMachineRetry starts a failed creation over: whatever the failed
// attempt left on the hypervisor is removed first, and the machine gets
// a fresh token and NoCloud volume.
func (a *app) handleMachineRetry(w http.ResponseWriter, r *http.Request) {
	m, ok := a.getMachine(w, r)
	if !ok {
		return
	}
	if m.Phase != machines.PhaseFailed || m.NodeID != "" {
		writeError(w, http.StatusConflict, "only a machine whose creation failed can be retried")
		return
	}
	id := m.ID
	if err := a.runner.phase(id, machines.PhasePending, "retrying"); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	started := a.runner.start(id, func(ctx context.Context) error {
		if m.Ref != nil {
			h, ok := a.hypervisors.Get(m.Spec.HypervisorID)
			if !ok {
				return errors.New("its hypervisor no longer exists")
			}
			drv, err := newDriver(h, a.controllerID)
			if err != nil {
				return err
			}
			err = drv.DestroyMachine(ctx, *m.Ref)
			drv.Close()
			if err != nil {
				return err
			}
			if _, err := a.machines.Update(id, func(m *machines.Machine) error { m.Ref = nil; return nil }); err != nil {
				return err
			}
		}
		return a.runner.create(ctx, id)
	})
	if !started {
		writeError(w, http.StatusConflict, "the machine is busy")
		return
	}
	m, _ = a.machines.Get(id)
	writeJSON(w, http.StatusAccepted, a.machineView(m))
}

// handleMachineDelete destroys a machine; ?forget=true only drops the
// Controller's records of it (a hypervisor gone for good).
func (a *app) handleMachineDelete(w http.ResponseWriter, r *http.Request) {
	m, ok := a.getMachine(w, r)
	if !ok {
		return
	}
	if lockedFromPage(w, r, m) {
		return
	}
	id := m.ID
	forget := r.URL.Query().Get("forget") == "true"
	// Whatever is under way (a creation) stops first.
	a.runner.stop(id)
	if forget {
		if err := a.runner.forget(r.Context(), id); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := a.runner.phase(id, machines.PhaseDestroying, "destroying"); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.runner.start(id, func(ctx context.Context) error { return a.runner.destroy(ctx, id) })
	m, _ = a.machines.Get(id)
	writeJSON(w, http.StatusAccepted, a.machineView(m))
}

func (a *app) handleMachinePower(w http.ResponseWriter, r *http.Request) {
	m, ok := a.getMachine(w, r)
	if !ok {
		return
	}
	var req struct {
		Action hypervisor.PowerAction `json:"action"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if !req.Action.Valid() {
		writeError(w, http.StatusBadRequest, "action: start, force-off or reset")
		return
	}
	if m.Ref == nil || m.Ref.UUID == "" || m.Phase == machines.PhaseDestroying {
		writeError(w, http.StatusConflict, "the machine has no virtual machine to act on")
		return
	}
	h, ok := a.hypervisors.Get(m.Spec.HypervisorID)
	if !ok {
		writeError(w, http.StatusConflict, "its hypervisor no longer exists")
		return
	}
	drv, err := newDriver(h, a.controllerID)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer drv.Close()
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := drv.Power(ctx, *m.Ref, req.Action); err != nil {
		code := http.StatusBadGateway
		if errors.Is(err, hypervisor.ErrNotOwned) {
			code = http.StatusForbidden
		}
		writeError(w, code, err.Error())
		return
	}
	a.runner.logEvent(m.ID, "power: %s", req.Action)
	a.hvStatus.forget(h.ID)
	w.WriteHeader(http.StatusNoContent)
}

// handleMachineConsole streams the machine's serial console as SSE: one
// event per chunk, JSON-encoded (a console writes \r and escape
// sequences, which SSE's line format can't carry raw). Private keys are
// hidden - a node's first boot prints its root admin credential there,
// which the Controller must never hold. The console is shared with every
// other reader (consoleHub).
func (a *app) handleMachineConsole(w http.ResponseWriter, r *http.Request) {
	m, ok := a.getMachine(w, r)
	if !ok {
		return
	}
	open, err := a.machineConsole(m)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	sse, ok := nodeproxy.NewSSE(w, r)
	if !ok {
		return
	}
	_ = sse.Comment("console")
	out := &utf8Chunks{send: func(text string) error {
		chunk, _ := json.Marshal(text)
		return sse.Send("", string(chunk))
	}}
	batch := newCoalescer(out, 100*time.Millisecond)
	sub := a.consoles.subscribe(m.ID, open, batch)
	select {
	case <-r.Context().Done():
		a.consoles.unsubscribe(sub)
	case err = <-sub.done:
	}
	_ = batch.Close()
	_ = out.Flush()
	if r.Context().Err() != nil {
		return // the browser left
	}
	msg := "the console closed"
	if err != nil {
		msg = err.Error()
	}
	_ = sse.Send("failure", msg)
}

// handleCatalog is what the "create a node" form offers: the newest
// release and the extensions the image factory builds.
func (a *app) handleCatalog(w http.ResponseWriter, r *http.Request) {
	out := struct {
		Latest       string `json:"latest,omitempty"`
		LatestError  string `json:"latest_error,omitempty"`
		Extensions   any    `json:"extensions"`
		CatalogError string `json:"catalog_error,omitempty"`
	}{Extensions: []any{}}
	if rel, err := nodeproxy.LatestRelease(r.Context()); err != nil {
		out.LatestError = err.Error()
	} else {
		out.Latest = rel.TagName
	}
	if exts, err := nodeproxy.FactoryExtensions(r.Context()); err != nil {
		out.CatalogError = err.Error()
	} else {
		out.Extensions = exts
	}
	writeJSON(w, http.StatusOK, out)
}
