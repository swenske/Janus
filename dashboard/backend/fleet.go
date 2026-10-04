package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/swenske/Janus/dashboard/backend/internal/fleet"
	"github.com/swenske/Janus/dashboard/backend/internal/nodeproxy"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
)

// The fleet (internal/fleet): set up from the main page, then every node
// is brought to trust it - the Controller then reaches each with its
// fleet certificate, and deletes the service credential it kept for it.

// Node trust states, as GET /api/fleet reports them.
const (
	trustWaiting     = "waiting"     // not tried yet
	trustTrusted     = "trusted"     // trusts the fleet; reached with the fleet certificate
	trustUnsupported = "unsupported" // too old to trust a fleet: update it
	trustOtherFleet  = "other-fleet" // trusts another fleet
	trustError       = "error"
)

type nodeTrust struct {
	State   string    `json:"state"`
	Error   string    `json:"error,omitempty"`
	Checked time.Time `json:"checked,omitzero"`
}

// trustTracker runs the nodes' trust: once at start, each time something
// changes (the fleet is confirmed, a node is admitted), and every ten
// minutes - a node updated meanwhile, a new bundle.
type trustTracker struct {
	mu    sync.Mutex
	nodes map[string]nodeTrust
	kick  chan struct{}
}

func newTrustTracker() *trustTracker {
	return &trustTracker{nodes: map[string]nodeTrust{}, kick: make(chan struct{}, 1)}
}

// Kick makes the trust loop run now (nothing without a tracker: a test's
// app).
func (t *trustTracker) Kick() {
	if t == nil {
		return
	}
	select {
	case t.kick <- struct{}{}:
	default:
	}
}

func (t *trustTracker) set(id string, nt nodeTrust) {
	t.mu.Lock()
	defer t.mu.Unlock()
	nt.Checked = time.Now().UTC()
	t.nodes[id] = nt
}

func (t *trustTracker) get(id string) nodeTrust {
	t.mu.Lock()
	defer t.mu.Unlock()
	if nt, ok := t.nodes[id]; ok {
		return nt
	}
	return nodeTrust{State: trustWaiting}
}

func (a *app) trustLoop(ctx context.Context) {
	tick := time.NewTicker(10 * time.Minute)
	defer tick.Stop()
	for {
		a.trustAll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-a.trust.kick:
		}
	}
}

func (a *app) trustAll(ctx context.Context) {
	if a.fleet.Status().State != fleet.StateReady {
		return
	}
	for _, n := range a.store.List() {
		nctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		nt := a.ensureTrust(nctx, n)
		cancel()
		a.trust.set(n.ID, nt)
		if nt.State == trustTrusted {
			// A keyless node approved meanwhile fetched its trust.
			a.pending.RemoveApproved(n.ID)
		}
	}
}

// ensureTrust brings node to trust the fleet's latest bundle. A node not
// trusting it yet is given it with the service credential the Controller
// kept for it; once the fleet certificate is seen to let the Controller
// in, the node is reached with it, and that credential deleted.
func (a *app) ensureTrust(ctx context.Context, n *store.Node) nodeTrust {
	rootPEM, bundle, version, ok := a.fleet.Trust()
	if !ok {
		return nodeTrust{State: trustWaiting}
	}
	if n.TrustsFleet() {
		st, err := nodeproxy.TrustGet(ctx, n)
		if err != nil {
			return trustFailure(err)
		}
		if st.GetBundleVersion() < version {
			if _, err := nodeproxy.TrustSet(ctx, n, rootPEM, bundle); err != nil {
				return trustFailure(err)
			}
			log.Printf("node %s (%s): fleet bundle version %d applied", n.Name, n.ID, version)
		}
		return nodeTrust{State: trustTrusted}
	}

	if _, err := nodeproxy.TrustSet(ctx, n, rootPEM, bundle); err != nil {
		return trustFailure(err)
	}
	if err := nodeproxy.ProbeFleet(ctx, n); err != nil {
		return nodeTrust{State: trustError, Error: "the node took the fleet, but its certificate doesn't let the Controller in: " + status.Convert(err).Message()}
	}
	if err := a.store.SetFleet(n.ID); err != nil {
		return nodeTrust{State: trustError, Error: "record the node's trust: " + err.Error()}
	}
	nodeproxy.Reconnect(n.ID)
	log.Printf("node %s (%s): trusts the fleet - reached with the fleet certificate, its service credential deleted", n.Name, n.ID)
	return nodeTrust{State: trustTrusted}
}

func trustFailure(err error) nodeTrust {
	switch status.Code(err) {
	case codes.Unimplemented:
		return nodeTrust{State: trustUnsupported, Error: "this node's release can't trust a fleet: update it"}
	case codes.FailedPrecondition:
		return nodeTrust{State: trustOtherFleet, Error: status.Convert(err).Message()}
	}
	return nodeTrust{State: trustError, Error: status.Convert(err).Message()}
}

// --- API ---

func (a *app) registerFleetRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/fleet", a.requireAuth(a.handleFleetStatus))
	mux.HandleFunc("POST /api/fleet/setup", a.requireSession(a.handleFleetSetup))
	mux.HandleFunc("GET /api/fleet/recovery-kit", a.requireSession(a.handleFleetKit))
	mux.HandleFunc("POST /api/fleet/confirm", a.requireSession(a.handleFleetConfirm))
}

func (a *app) handleFleetStatus(w http.ResponseWriter, _ *http.Request) {
	nodes := map[string]nodeTrust{}
	for _, n := range a.store.List() {
		nt := a.trust.get(n.ID)
		if n.TrustsFleet() && nt.State == trustWaiting {
			nt.State = trustTrusted
		}
		nodes[n.ID] = nt
	}
	writeJSON(w, http.StatusOK, struct {
		fleet.Status
		Nodes map[string]nodeTrust `json:"nodes"`
	}{a.fleet.Status(), nodes})
}

// handleFleetSetup makes the fleet and its recovery kit; the passphrase
// is in this answer only.
func (a *app) handleFleetSetup(w http.ResponseWriter, _ *http.Request) {
	pass, err := a.fleet.Setup()
	if errors.Is(err, fleet.ErrState) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Printf("fleet: set up - its recovery kit waits to be confirmed")
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"passphrase": pass, "kit_file": a.fleet.KitFileName()})
}

func (a *app) handleFleetKit(w http.ResponseWriter, _ *http.Request) {
	kit, err := a.fleet.RecoveryKit()
	if errors.Is(err, fleet.ErrState) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+a.fleet.KitFileName()+`"`)
	_, _ = w.Write(kit)
}

// handleFleetConfirm takes the kit back with its passphrase - a JSON
// body {"kit": "...", "passphrase": "..."} - and, once both check, the
// root's key leaves the Controller and the nodes are brought to trust the
// fleet.
func (a *app) handleFleetConfirm(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kit        string `json:"kit"`
		Passphrase string `json:"passphrase"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "a JSON body {kit, passphrase}: "+err.Error())
		return
	}
	err := a.fleet.Confirm([]byte(req.Kit), req.Passphrase)
	switch {
	case errors.Is(err, fleet.ErrState):
		writeError(w, http.StatusConflict, err.Error())
		return
	case errors.Is(err, fleet.ErrKit):
		writeError(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Printf("fleet: the recovery kit is confirmed - the root's key is gone from the Controller; bringing the nodes to trust the fleet")
	a.trust.Kick()
	a.handleFleetStatus(w, r)
}
