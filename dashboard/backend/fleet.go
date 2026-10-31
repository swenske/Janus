package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/swenske/Janus/dashboard/backend/internal/auth"
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
	// trustNeedsKit: the node's bundle lists no issuing CA the Controller
	// can still sign with - a renewal it missed, an issuing CA expired:
	// the recovery kit reaches it (POST /api/fleet/rescue).
	trustNeedsKit = "needs-kit"
	trustError    = "error"
)

type nodeTrust struct {
	State   string    `json:"state"`
	Error   string    `json:"error,omitempty"`
	Checked time.Time `json:"checked,omitzero"`
	// BundleVersion is the fleet bundle the node was last seen with.
	BundleVersion uint64 `json:"bundle_version,omitempty"`
}

// trustTracker runs the nodes' trust: once at start, each time something
// changes (the fleet is confirmed or renewed, a node is admitted), and
// every ten minutes - a node updated meanwhile, a new bundle.
type trustTracker struct {
	mu    sync.Mutex
	nodes map[string]nodeTrust
	kick  chan struct{}
	// warned is when the issuing CA's expiry was last logged.
	warned time.Time

	// The loop's passes: whether it runs, how many began and ended, and
	// a channel closed (then replaced) at each pass's end.
	looping           bool
	started, finished uint64
	passDone          chan struct{}
}

func newTrustTracker() *trustTracker {
	return &trustTracker{nodes: map[string]nodeTrust{}, kick: make(chan struct{}, 1), passDone: make(chan struct{})}
}

func (t *trustTracker) beginPass() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.started++
}

func (t *trustTracker) endPass() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.finished++
	close(t.passDone)
	t.passDone = make(chan struct{})
}

// KickAndWait makes the trust loop run now, and waits - wait at most -
// for a pass begun after this call to end: what changed (a renewal) has
// reached the nodes it can reach, and the answer says so. Nothing to wait
// for without a running loop (a test's app).
func (t *trustTracker) KickAndWait(wait time.Duration) {
	if t == nil {
		return
	}
	t.mu.Lock()
	looping, target := t.looping, t.started+1
	t.mu.Unlock()
	t.Kick()
	if !looping {
		return
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		t.mu.Lock()
		done, ch := t.finished >= target, t.passDone
		t.mu.Unlock()
		if done {
			return
		}
		select {
		case <-ch:
		case <-deadline.C:
			return
		}
	}
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
	a.trust.mu.Lock()
	a.trust.looping = true
	a.trust.mu.Unlock()
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

// trustParallel is how many nodes the trust loop brings along at once.
const trustParallel = 8

func (a *app) trustAll(ctx context.Context) {
	a.trust.beginPass()
	defer a.trust.endPass()
	st := a.fleet.Status()
	if st.State != fleet.StateReady {
		return
	}
	a.warnIssuingExpiry(st)
	nodes := a.store.List()
	sem := make(chan struct{}, trustParallel)
	var wg sync.WaitGroup
	for _, n := range nodes {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			nctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			nt := a.ensureTrust(nctx, n)
			cancel()
			a.trust.set(n.ID, nt)
			if nt.State == trustTrusted {
				// A keyless node approved meanwhile fetched its trust.
				a.pending.RemoveApproved(n.ID)
			}
		}()
	}
	wg.Wait()
	behind := a.nodesBehind()
	switched, err := a.fleet.Switch(len(behind) == 0)
	switch {
	case err != nil:
		log.Printf("fleet: switch janusctl's certificates to the new issuing CA: %v", err)
	case switched:
		now := a.fleet.Status()
		if len(behind) == 0 {
			log.Printf("fleet: every node has the renewal's bundle - janusctl's certificates now come from the issuing CA %q (until %s)", now.IssuingName, now.IssuingNotAfter.UTC().Format(time.DateOnly))
		} else {
			log.Printf("fleet: janusctl's certificates now come from the issuing CA %q (until %s), the CA before ending soon - still on an older bundle: %s", now.IssuingName, now.IssuingNotAfter.UTC().Format(time.DateOnly), strings.Join(behind, ", "))
		}
	}
}

// nodesBehind are the nodes trusting the fleet whose bundle doesn't list
// the newest issuing CA yet.
func (a *app) nodesBehind() []string {
	renewed := a.fleet.RenewedVersion()
	var out []string
	if renewed == 0 {
		return out
	}
	for _, n := range a.store.List() {
		if n.TrustsFleet() && n.Bundle() < renewed {
			out = append(out, n.Name)
		}
	}
	return out
}

// warnIssuingExpiry says once a day, in the last 180 days of the issuing
// CA, that it needs renewing.
func (a *app) warnIssuingExpiry(st fleet.Status) {
	if st.IssuingNotAfter.IsZero() || time.Now().Before(st.RenewFrom) {
		return
	}
	a.trust.mu.Lock()
	due := time.Since(a.trust.warned) >= 24*time.Hour
	if due {
		a.trust.warned = time.Now()
	}
	a.trust.mu.Unlock()
	if !due {
		return
	}
	if left := time.Until(st.IssuingNotAfter); left > 0 {
		log.Printf("fleet: the issuing CA expires on %s (%d days) - renew it on the main page (Fleet) with the recovery kit and its passphrase", st.IssuingNotAfter.UTC().Format(time.DateOnly), int(left.Hours()/24))
	} else {
		log.Printf("fleet: the issuing CA expired on %s - the Controller reaches no node through the fleet: renew it on the main page (Fleet) with the recovery kit and its passphrase", st.IssuingNotAfter.UTC().Format(time.DateOnly))
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
			return a.trustOtherwise(ctx, n, err, rootPEM, bundle, version)
		}
		have := st.GetBundleVersion()
		a.seenBundle(n, have)
		if have < version {
			if _, err := nodeproxy.TrustSet(ctx, n, rootPEM, bundle); err != nil {
				return trustFailure(err)
			}
			log.Printf("node %s (%s): fleet bundle version %d applied", n.Name, n.ID, version)
			a.seenBundle(n, version)
			have = version
		}
		return nodeTrust{State: trustTrusted, BundleVersion: have}
	}

	if _, err := nodeproxy.TrustSet(ctx, n, rootPEM, bundle); err != nil {
		return trustFailure(err)
	}
	a.seenBundle(n, version)
	if err := nodeproxy.ProbeFleet(ctx, n); err != nil {
		return nodeTrust{State: trustError, Error: "the node took the fleet, but its certificate doesn't let the Controller in: " + status.Convert(err).Message()}
	}
	if err := a.store.SetFleet(n.ID); err != nil {
		return nodeTrust{State: trustError, Error: "record the node's trust: " + err.Error()}
	}
	nodeproxy.Reconnect(n.ID)
	log.Printf("node %s (%s): trusts the fleet - reached with the fleet certificate, its service credential deleted", n.Name, n.ID)
	return nodeTrust{State: trustTrusted, BundleVersion: version}
}

// seenBundle records the bundle version node was seen with - and, when
// it crossed the last renewal's, drops its connection: the next one is
// made with the new issuing CA's certificate (nodeproxy.FleetIdentity).
func (a *app) seenBundle(n *store.Node, version uint64) {
	before := n.Bundle()
	if before == version {
		return
	}
	if err := a.store.SetFleetBundle(n.ID, version); err != nil {
		log.Printf("node %s (%s): record its fleet bundle: %v", n.Name, n.ID, err)
		return
	}
	if renewed := a.fleet.RenewedVersion(); renewed != 0 && (before < renewed) != (version < renewed) {
		nodeproxy.Reconnect(n.ID)
	}
}

// trustOtherwise is a node trusting the fleet that refused the
// Controller's usual certificate for it (failed): the bundle version
// recorded for it may be wrong (a node reset, or reinstalled at its
// address), or no certificate it lets in is left (a renewal it missed,
// an issuing CA expired). Another try, over a connection of its own,
// with the certificate for a node on the oldest bundle - the CA before
// the last renewal, or the kit's - brings it to the bundle.
func (a *app) trustOtherwise(ctx context.Context, n *store.Node, failed error, rootPEM, bundle []byte, version uint64) nodeTrust {
	if !nodeproxy.RefusedCertificate(failed) {
		return trustFailure(failed)
	}
	var tries []*tls.Certificate
	if cert, err := a.fleet.ClientCertificateFor(0); err == nil {
		tries = append(tries, cert)
	}
	if rescue := a.fleet.RescueCertificate(); rescue != nil && (len(tries) == 0 || tries[0] != rescue) {
		tries = append(tries, rescue)
	}
	var have uint64
	err := failed
	for _, cert := range tries {
		if have, err = nodeproxy.TrustSyncWith(ctx, n, cert, rootPEM, bundle, version); err == nil || !nodeproxy.RefusedCertificate(err) {
			break
		}
	}
	if err != nil {
		if nodeproxy.RefusedCertificate(err) {
			return nodeTrust{State: trustNeedsKit, BundleVersion: n.Bundle(), Error: "the node's bundle lets no certificate the Controller can make in - a renewal of the issuing CA it missed, or one that expired: give the recovery kit to reach it"}
		}
		return trustFailure(err)
	}
	log.Printf("node %s (%s): reached with another fleet certificate - its bundle is version %d", n.Name, n.ID, have)
	a.seenBundle(n, have)
	nodeproxy.Reconnect(n.ID)
	return nodeTrust{State: trustTrusted, BundleVersion: have}
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
	mux.HandleFunc("GET /api/fleet", a.gate(auth.Reader, auth.Admin, a.handleFleetStatus))
	mux.HandleFunc("POST /api/fleet/setup", a.sessionGate(auth.Admin, auth.Admin, a.handleFleetSetup))
	mux.HandleFunc("GET /api/fleet/recovery-kit", a.sessionGate(auth.Admin, auth.Admin, a.handleFleetKit))
	mux.HandleFunc("POST /api/fleet/confirm", a.sessionGate(auth.Admin, auth.Admin, a.handleFleetConfirm))
	mux.HandleFunc("POST /api/fleet/renew", a.sessionGate(auth.Admin, auth.Admin, a.handleFleetRenew))
	mux.HandleFunc("POST /api/fleet/rescue", a.sessionGate(auth.Admin, auth.Admin, a.handleFleetRescue))
}

func (a *app) handleFleetStatus(w http.ResponseWriter, _ *http.Request) {
	nodes := map[string]nodeTrust{}
	for _, n := range a.store.List() {
		nt := a.trust.get(n.ID)
		if n.TrustsFleet() && nt.State == trustWaiting {
			nt.State = trustTrusted
		}
		if nt.BundleVersion == 0 {
			nt.BundleVersion = n.Bundle()
		}
		nodes[n.ID] = nt
	}
	writeJSON(w, http.StatusOK, struct {
		fleet.Status
		Nodes map[string]nodeTrust `json:"nodes"`
		// NodesBehind are the nodes whose bundle doesn't list the newest
		// issuing CA yet, after a renewal.
		NodesBehind []string `json:"nodes_behind"`
	}{a.fleet.Status(), nodes, nonNil(a.nodesBehind())})
}

// kitRequest is the recovery kit and its passphrase - Confirm, Renew,
// Rescue.
type kitRequest struct {
	Kit        string `json:"kit"`
	Passphrase string `json:"passphrase"`
}

func decodeKit(w http.ResponseWriter, r *http.Request) (kitRequest, bool) {
	var req kitRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "a JSON body {kit, passphrase}: "+err.Error())
		return req, false
	}
	return req, true
}

// fleetError answers a fleet step's error: out of order or busy 409, a
// kit that doesn't open 400.
func fleetError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, fleet.ErrState), errors.Is(err, fleet.ErrRenewing):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, fleet.ErrKit):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

// renewWait is how long a renewal or a rescue waits for the trust loop's
// pass before it answers.
const renewWait = 15 * time.Second

// handleFleetRenew renews the issuing CA with the kit and its passphrase
// - a JSON body {"kit": "...", "passphrase": "..."}: a new issuing CA, a
// bundle listing it and the current one, brought to the nodes at once.
// Refused while nodes are still on the bundle before the last renewal
// and its CA before still lasts: they'd lose the CA they're reached
// with.
func (a *app) handleFleetRenew(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeKit(w, r)
	if !ok {
		return
	}
	if behind := a.nodesBehind(); len(behind) > 0 {
		if st := a.fleet.Status(); st.Renewal != nil && time.Now().Before(st.Renewal.PreviousNotAfter) {
			writeError(w, http.StatusConflict, "nodes still on the bundle before the last renewal: "+strings.Join(behind, ", ")+" - bring them back (or remove them) first")
			return
		}
	}
	var above uint64
	for _, n := range a.store.List() {
		above = max(above, n.Bundle())
	}
	if err := a.fleet.Renew([]byte(req.Kit), req.Passphrase, above); err != nil {
		fleetError(w, err)
		return
	}
	st := a.fleet.Status()
	log.Printf("fleet: the issuing CA renewed with the recovery kit - %q until %s, bundle version %d; bringing the nodes to it", st.IssuingName, st.IssuingNotAfter.UTC().Format(time.DateOnly), st.BundleVersion)
	// The answer says where the nodes stand once the loop brought those it
	// reaches to the bundle - not the moment before.
	a.trust.KickAndWait(renewWait)
	a.handleFleetStatus(w, r)
}

// handleFleetRescue opens the kit for an hour's certificate the root
// signs: the nodes no issuing CA the Controller has reaches any more are
// brought to the fleet's bundle with it.
func (a *app) handleFleetRescue(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeKit(w, r)
	if !ok {
		return
	}
	if err := a.fleet.Rescue([]byte(req.Kit), req.Passphrase); err != nil {
		fleetError(w, err)
		return
	}
	log.Printf("fleet: the recovery kit opened for an hour - the nodes no issuing CA reaches are brought to the bundle with a certificate of the root")
	a.trust.KickAndWait(renewWait)
	a.handleFleetStatus(w, r)
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
	req, ok := decodeKit(w, r)
	if !ok {
		return
	}
	if err := a.fleet.Confirm([]byte(req.Kit), req.Passphrase); err != nil {
		fleetError(w, err)
		return
	}
	log.Printf("fleet: the recovery kit is confirmed - the root's key is gone from the Controller; bringing the nodes to trust the fleet")
	a.trust.Kick()
	a.handleFleetStatus(w, r)
}
