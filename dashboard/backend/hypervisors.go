package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/swenske/Janus/dashboard/backend/internal/hypervisor"
	"github.com/swenske/Janus/dashboard/backend/internal/hypervisor/libvirt"
	"github.com/swenske/Janus/dashboard/backend/internal/hypervisor/proxmox"
	"github.com/swenske/Janus/dashboard/backend/internal/machines"
	"github.com/swenske/Janus/dashboard/backend/internal/auth"
)

// The hypervisors the Controller creates its own nodes on
// (docs/hypervisors.md). Adding one is a three-step handshake, so the
// Controller only ever logs in to a host the operator vouched for:
// POST /api/hypervisors gives the public key to authorize on the host,
// .../probe reads the host's SSH key, and .../trust pins it once the
// operator confirmed its fingerprint on the host itself.

func (a *app) registerHypervisorRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/hypervisors", a.gate(auth.Reader, auth.Admin, a.handleHypervisorList))
	mux.HandleFunc("POST /api/hypervisors", a.gate(auth.Reader, auth.Admin, a.handleHypervisorCreate))
	mux.HandleFunc("GET /api/hypervisors/{id}", a.gate(auth.Reader, auth.Admin, a.handleHypervisorGet))
	mux.HandleFunc("PATCH /api/hypervisors/{id}", a.gate(auth.Reader, auth.Admin, a.handleHypervisorUpdate))
	mux.HandleFunc("DELETE /api/hypervisors/{id}", a.gate(auth.Reader, auth.Admin, a.handleHypervisorDelete))
	mux.HandleFunc("POST /api/hypervisors/{id}/probe", a.gate(auth.Reader, auth.Admin, a.handleHypervisorProbe))
	mux.HandleFunc("POST /api/hypervisors/{id}/trust", a.gate(auth.Reader, auth.Admin, a.handleHypervisorTrust))
	mux.HandleFunc("GET /api/hypervisors/{id}/status", a.gate(auth.Reader, auth.Admin, a.handleHypervisorStatus))
	mux.HandleFunc("POST /api/hypervisors/preparation", a.gate(auth.Reader, auth.Admin, a.handleHypervisorPreparation))
}

// newDriver connects the Controller to a hypervisor - a variable so
// tests can give a fake one.
var newDriver = func(h *hypervisor.Hypervisor, controllerID string) (hypervisor.Driver, error) {
	switch h.Kind {
	case hypervisor.KindLibvirt:
		return libvirt.New(h, controllerID)
	case hypervisor.KindProxmox:
		return proxmox.New(h, controllerID)
	}
	return nil, fmt.Errorf("unknown hypervisor kind %q", h.Kind)
}

type hypervisorView struct {
	ID                string                    `json:"id"`
	Name              string                    `json:"name"`
	Kind              hypervisor.Kind           `json:"kind"`
	ControllerAddress string                    `json:"controller_address,omitempty"`
	Libvirt           *hypervisor.LibvirtConfig `json:"libvirt,omitempty"`
	Proxmox           *hypervisor.ProxmoxConfig `json:"proxmox,omitempty"`
	// AuthorizedKey is the line to add to the hypervisor user's
	// ~/.ssh/authorized_keys (libvirt).
	AuthorizedKey string `json:"authorized_key,omitempty"`
	Trusted       bool   `json:"trusted"`
	// HostKeyFingerprint is the pinned SSH host key's (libvirt) or API
	// certificate's (Proxmox) fingerprint; CASubject the CA trusted
	// instead (Proxmox).
	HostKeyFingerprint string `json:"host_key_fingerprint,omitempty"`
	CASubject          string `json:"ca_subject,omitempty"`
	// HasTokenSecret: a Proxmox token's secret is kept (never shown).
	HasTokenSecret bool      `json:"has_token_secret,omitempty"`
	Machines       int       `json:"machines"`
	CreatedAt      time.Time `json:"created_at"`
}

func (a *app) hypervisorView(h *hypervisor.Hypervisor) hypervisorView {
	v := hypervisorView{
		ID: h.ID, Name: h.Name, Kind: h.Kind, ControllerAddress: h.ControllerAddress, Libvirt: h.Libvirt, Proxmox: h.Proxmox,
		CreatedAt: h.CreatedAt, Trusted: h.Trusted(),
	}
	if h.Libvirt != nil {
		v.AuthorizedKey = h.AuthorizedKey()
		if key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(h.Libvirt.HostKey)); err == nil {
			v.HostKeyFingerprint = ssh.FingerprintSHA256(key)
		}
	}
	if p := h.Proxmox; p != nil {
		v.HostKeyFingerprint, v.HasTokenSecret = p.Fingerprint, h.TokenSecret != ""
		if ca, err := hypervisor.ParseCACert(p.CACert); err == nil {
			v.CASubject = ca.Subject.String()
		}
	}
	for _, m := range a.machines.List() {
		if m.Spec.HypervisorID == h.ID {
			v.Machines++
		}
	}
	return v
}

func (a *app) handleHypervisorList(w http.ResponseWriter, _ *http.Request) {
	out := []hypervisorView{}
	for _, h := range a.hypervisors.List() {
		out = append(out, a.hypervisorView(h))
	}
	writeJSON(w, http.StatusOK, out)
}

// hypervisorRequest is what can be set on a hypervisor - never its key,
// nor its host key or certificate (that's .../trust's job). A Proxmox
// token's secret can only be written.
type hypervisorRequest struct {
	Name              string                    `json:"name"`
	Kind              hypervisor.Kind           `json:"kind"`
	ControllerAddress string                    `json:"controller_address"`
	Libvirt           *hypervisor.LibvirtConfig `json:"libvirt"`
	Proxmox           *hypervisor.ProxmoxConfig `json:"proxmox"`
	TokenSecret       string                    `json:"token_secret,omitempty"`
}

// hypervisor is the request's hypervisor: its kind (from its settings
// when unset), nothing the operator didn't vouch for through .../trust.
func (req *hypervisorRequest) hypervisor() *hypervisor.Hypervisor {
	if req.Kind == "" {
		req.Kind = hypervisor.KindLibvirt
		if req.Proxmox != nil {
			req.Kind = hypervisor.KindProxmox
		}
	}
	h := &hypervisor.Hypervisor{Name: strings.TrimSpace(req.Name), Kind: req.Kind, ControllerAddress: req.ControllerAddress, TokenSecret: req.TokenSecret}
	if req.Libvirt != nil {
		l := *req.Libvirt
		l.HostKey = ""
		h.Libvirt = &l
	}
	if req.Proxmox != nil {
		p := *req.Proxmox
		p.Fingerprint = ""
		h.Proxmox = &p
	}
	return h
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "decode request: "+err.Error())
		return false
	}
	return true
}

// writeError is the API's error shape for these endpoints: {"error"},
// which a client (the future Terraform provider) can show as is.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (a *app) handleHypervisorCreate(w http.ResponseWriter, r *http.Request) {
	var req hypervisorRequest
	if !decodeBody(w, r, &req) {
		return
	}
	h := req.hypervisor()
	if err := a.hypervisors.Add(h); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, a.hypervisorView(h))
}

func (a *app) getHypervisor(w http.ResponseWriter, r *http.Request) (*hypervisor.Hypervisor, bool) {
	h, ok := a.hypervisors.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "no such hypervisor")
	}
	return h, ok
}

func (a *app) handleHypervisorGet(w http.ResponseWriter, r *http.Request) {
	if h, ok := a.getHypervisor(w, r); ok {
		writeJSON(w, http.StatusOK, a.hypervisorView(h))
	}
}

func (a *app) handleHypervisorUpdate(w http.ResponseWriter, r *http.Request) {
	h, ok := a.getHypervisor(w, r)
	if !ok {
		return
	}
	var req hypervisorRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Kind != "" && req.Kind != h.Kind {
		writeError(w, http.StatusBadRequest, "a hypervisor's kind can't change")
		return
	}
	next := *h
	next.Name, next.ControllerAddress, next.TokenSecret = strings.TrimSpace(req.Name), req.ControllerAddress, req.TokenSecret
	if req.Proxmox != nil && h.Proxmox != nil {
		p := *req.Proxmox
		// The pinned certificate belongs to the API's address: another
		// one must be trusted again.
		p.Fingerprint = h.Proxmox.Fingerprint
		if p.Address() != h.Proxmox.Address() {
			p.Fingerprint = ""
		}
		next.Proxmox = &p
	}
	if req.Libvirt != nil && h.Libvirt != nil {
		l := *req.Libvirt
		// The pinned host key belongs to the host: another host must be
		// trusted again.
		l.HostKey = h.Libvirt.HostKey
		if l.Host != h.Libvirt.Host {
			l.HostKey = ""
		}
		next.Libvirt = &l
	}
	if err := a.hypervisors.Update(&next); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	a.hvStatus.forget(h.ID)
	h, _ = a.hypervisors.Get(h.ID)
	writeJSON(w, http.StatusOK, a.hypervisorView(h))
}

func (a *app) handleHypervisorDelete(w http.ResponseWriter, r *http.Request) {
	h, ok := a.getHypervisor(w, r)
	if !ok {
		return
	}
	for _, m := range a.machines.List() {
		if m.Spec.HypervisorID == h.ID {
			writeError(w, http.StatusConflict, fmt.Sprintf("machine %s is still on this hypervisor - destroy its machines first", m.Spec.Name))
			return
		}
	}
	if err := a.hypervisors.Remove(h.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.hvStatus.forget(h.ID)
	w.WriteHeader(http.StatusNoContent)
}

// preparationRequest is a hypervisor's settings - the form's, before
// it's added or saved - and, for one already added, its ID: its key then
// goes in the preparation too.
type preparationRequest struct {
	hypervisorRequest
	ID string `json:"id,omitempty"`
}

type preparationView struct {
	Steps []hypervisor.PrepStep `json:"steps"`
	// Script is every step in one script.
	Script string `json:"script"`
	// HasKey: the Controller's key is in it.
	HasKey bool `json:"has_key"`
}

// handleHypervisorPreparation gives what to run on the host, as root,
// for these settings (docs/hypervisors.md: preparing a libvirt host).
func (a *app) handleHypervisorPreparation(w http.ResponseWriter, r *http.Request) {
	var req preparationRequest
	if !decodeBody(w, r, &req) {
		return
	}
	h := req.hypervisor()
	if err := h.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	key := ""
	if req.ID != "" {
		saved, ok := a.hypervisors.Get(req.ID)
		if !ok {
			writeError(w, http.StatusNotFound, "no such hypervisor")
			return
		}
		key = saved.AuthorizedKey()
	}
	var steps []hypervisor.PrepStep
	switch h.Kind {
	case hypervisor.KindProxmox:
		steps, key = proxmox.HostPreparation(h.Name, h.Proxmox), ""
	default:
		steps = libvirt.HostPreparation(h.Name, h.Libvirt, key)
	}
	writeJSON(w, http.StatusOK, preparationView{Steps: steps, Script: hypervisor.PreparationScript(h.Name, steps), HasKey: key != ""})
}

// hostKeyView is what a hypervisor presents: its SSH host key
// (libvirt), or its API's certificate (Proxmox: Subject, Issuer).
type hostKeyView struct {
	HostKey     string `json:"host_key,omitempty"`
	Fingerprint string `json:"fingerprint"`
	Subject     string `json:"subject,omitempty"`
	Issuer      string `json:"issuer,omitempty"`
}

// probe reads what the hypervisor presents - trusting nothing yet; pin
// records it as trusted on a hypervisor.
func probe(ctx context.Context, h *hypervisor.Hypervisor) (hostKeyView, func(*hypervisor.Hypervisor), error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	switch {
	case h.Libvirt != nil:
		key, err := libvirt.ProbeHostKey(ctx, h.Libvirt.SSHAddress())
		if err != nil {
			return hostKeyView{}, nil, err
		}
		line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
		return hostKeyView{HostKey: line, Fingerprint: ssh.FingerprintSHA256(key)}, func(h *hypervisor.Hypervisor) { h.Libvirt.HostKey = line }, nil
	case h.Proxmox != nil:
		cert, err := proxmox.ProbeCertificate(ctx, h.Proxmox)
		if err != nil {
			return hostKeyView{}, nil, err
		}
		fp := proxmox.Fingerprint(cert.Raw)
		return hostKeyView{Fingerprint: fp, Subject: cert.Subject.String(), Issuer: cert.Issuer.String()}, func(h *hypervisor.Hypervisor) { h.Proxmox.Fingerprint = fp }, nil
	}
	return hostKeyView{}, nil, errors.New("nothing to read from this hypervisor")
}

// handleHypervisorProbe reads the host key or certificate the
// hypervisor presents - trusting nothing yet.
func (a *app) handleHypervisorProbe(w http.ResponseWriter, r *http.Request) {
	h, ok := a.getHypervisor(w, r)
	if !ok {
		return
	}
	v, _, err := probe(r.Context(), h)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// handleHypervisorTrust pins the host key or certificate - only the one
// the host presents now, and only if it's the one whose fingerprint the
// operator confirmed.
func (a *app) handleHypervisorTrust(w http.ResponseWriter, r *http.Request) {
	h, ok := a.getHypervisor(w, r)
	if !ok {
		return
	}
	var req struct {
		Fingerprint string `json:"fingerprint"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	v, pin, err := probe(r.Context(), h)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	confirmed := strings.TrimSpace(req.Fingerprint)
	if h.Proxmox != nil {
		confirmed = hypervisor.NormalizeFingerprint(confirmed)
	}
	if v.Fingerprint != confirmed {
		writeError(w, http.StatusConflict, fmt.Sprintf("the host now presents %s, not the confirmed %s - nothing was trusted", v.Fingerprint, req.Fingerprint))
		return
	}
	pin(h)
	if err := a.hypervisors.Update(h); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.hvStatus.forget(h.ID)
	writeJSON(w, http.StatusOK, a.hypervisorView(h))
}

// hypervisorStatus is a hypervisor's live state: the host, and the
// state of each machine the Controller has there.
type hypervisorStatus struct {
	Host *hypervisor.HostInfo `json:"host,omitempty"`
	// CPUPercent is the host's CPU use since the previous status, -1
	// for the first one.
	CPUPercent float64                              `json:"cpu_percent"`
	Machines   map[string]*hypervisor.MachineStatus `json:"machines"`
	// MachineErrors are machines whose state couldn't be read.
	MachineErrors map[string]string `json:"machine_errors,omitempty"`
	Error         string            `json:"error,omitempty"`
	CheckedAt     time.Time         `json:"checked_at"`
}

// statusCache keeps each hypervisor's status a few seconds (several open
// pages poll it) and the previous CPU counters for the usage.
type statusCache struct {
	mu      sync.Mutex
	entries map[string]*statusEntry
}

type statusEntry struct {
	mu          sync.Mutex
	st          *hypervisorStatus
	busy, total uint64
}

const statusTTL = 5 * time.Second

func (c *statusCache) entry(id string) *statusEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]*statusEntry{}
	}
	e, ok := c.entries[id]
	if !ok {
		e = &statusEntry{}
		c.entries[id] = e
	}
	return e
}

func (c *statusCache) forget(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, id)
}

func (a *app) handleHypervisorStatus(w http.ResponseWriter, r *http.Request) {
	h, ok := a.getHypervisor(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, a.hypervisorStatus(r.Context(), h))
}

func (a *app) hypervisorStatus(ctx context.Context, h *hypervisor.Hypervisor) *hypervisorStatus {
	e := a.hvStatus.entry(h.ID)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.st != nil && time.Since(e.st.CheckedAt) < statusTTL {
		return e.st
	}
	// Shared by every poller: not tied to the request that triggered it.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	st := &hypervisorStatus{CPUPercent: -1, Machines: map[string]*hypervisor.MachineStatus{}, CheckedAt: time.Now()}
	defer func() { e.st = st }()

	drv, err := newDriver(h, a.controllerID)
	if err != nil {
		st.Error = err.Error()
		return st
	}
	defer drv.Close()
	host, err := drv.HostInfo(ctx)
	if err != nil {
		st.Error = err.Error()
		return st
	}
	st.Host = host
	if host.CPUUsage != nil {
		st.CPUPercent = 100 * *host.CPUUsage
	} else if e.total > 0 && host.CPUTotalNs > e.total && host.CPUBusyNs >= e.busy {
		st.CPUPercent = 100 * float64(host.CPUBusyNs-e.busy) / float64(host.CPUTotalNs-e.total)
	}
	e.busy, e.total = host.CPUBusyNs, host.CPUTotalNs

	for _, m := range a.machines.List() {
		if m.Spec.HypervisorID != h.ID || m.Ref == nil || m.Ref.UUID == "" || m.Phase == machines.PhaseDestroying {
			continue
		}
		ms, err := drv.MachineStatus(ctx, *m.Ref)
		if err != nil {
			if st.MachineErrors == nil {
				st.MachineErrors = map[string]string{}
			}
			st.MachineErrors[m.ID] = err.Error()
			continue
		}
		st.Machines[m.ID] = ms
	}
	return st
}
