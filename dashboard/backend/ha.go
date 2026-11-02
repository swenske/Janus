package main

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/auth"
	"github.com/swenske/Janus/dashboard/backend/internal/ha"
	"github.com/swenske/Janus/internal/calver"
)

// The Controller's warm standby (internal/ha, docs/guide/controller-
// standby.md), the active's side: the routes its standby calls on the
// registration port - pairing, its state, a copy, stepping down -, the
// admins' on the main port, and the loop that watches the peer's epoch
// and the witness (witness.go). The standby's side is standby.go.

// haCheckEvery is how often the active asks its standby where it stands:
// a standby promoted while this one wasn't looking makes it step down.
const haCheckEvery = 30 * time.Second

// haPeerView is the active's last look at its standby.
type haPeerView struct {
	mu      sync.Mutex
	checked time.Time
	state   *ha.PeerState
	err     string
}

func (v *haPeerView) set(st *ha.PeerState, err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.checked, v.state, v.err = time.Now(), st, ""
	if err != nil {
		v.state, v.err = nil, err.Error()
	}
}

func (v *haPeerView) view() map[string]any {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.checked.IsZero() {
		return nil
	}
	out := map[string]any{"checked": v.checked}
	if v.state != nil {
		out["role"], out["epoch"], out["version"] = v.state.Role, v.state.Epoch, v.state.Version
	}
	if v.err != "" {
		out["error"] = v.err
	}
	return out
}

// haName is what a Controller calls itself to its peer.
func haName() string {
	if h, err := os.Hostname(); err == nil {
		return h
	}
	return "controller"
}

// --- the registration port: the peer's routes ---

func (a *app) registerHAPeerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /ha/pair", a.handleHAPair)
	mux.HandleFunc("GET /ha/state", a.haPeer(a.handleHAState))
	mux.HandleFunc("GET /ha/replica", a.haPeer(a.handleHAReplica))
	mux.HandleFunc("POST /ha/stepdown", a.haPeer(a.handleHAStepDown))
	mux.HandleFunc("POST /ha/promote", a.haPeer(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusConflict, "this Controller is the active: promote its standby")
	}))
}

// haPeer lets in a request carrying the peers' secret only.
func (a *app) haPeer(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.ha == nil || !a.ha.Authorized(bearerOf(r)) {
			writeError(w, http.StatusUnauthorized, "not this Controller's peer")
			return
		}
		next(w, r)
	}
}

func bearerOf(r *http.Request) string {
	v, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return ""
	}
	return strings.TrimSpace(v)
}

// haPairRequest is what `dashboardd standby -pair` sends.
type haPairRequest struct {
	Secret  string `json:"secret"`
	Name    string `json:"name"`
	Address string `json:"address"`
	CertPEM string `json:"cert_pem"`
	Version string `json:"version"`
}

// haPairResponse is the active's answer: the secret the two share from
// now on.
type haPairResponse struct {
	Secret  string `json:"secret"`
	Name    string `json:"name"`
	Epoch   uint64 `json:"epoch"`
	Version string `json:"version"`
}

func (a *app) handleHAPair(w http.ResponseWriter, r *http.Request) {
	if a.ha == nil {
		http.NotFound(w, r)
		return
	}
	if a.haPairLimiter != nil {
		if ok, wait := a.haPairLimiter.Allow(clientAddr(r)); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
			writeError(w, http.StatusTooManyRequests, "too many pairing attempts")
			return
		}
	}
	var req haPairRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	st := a.ha.State()
	if st.Role != ha.Active {
		writeError(w, http.StatusConflict, "this Controller is a standby: pair with the active")
		return
	}
	if st.Peer != nil {
		writeError(w, http.StatusConflict, "this Controller has a standby already ("+st.Peer.Name+", "+st.Peer.Address+"): remove it first")
		return
	}
	block, _ := pem.Decode([]byte(req.CertPEM))
	if block == nil {
		writeError(w, http.StatusBadRequest, "cert_pem: no certificate")
		return
	}
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		writeError(w, http.StatusBadRequest, "cert_pem: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Address) == "" || strings.TrimSpace(req.Name) == "" {
		writeError(w, http.StatusBadRequest, "name and address are required")
		return
	}
	if err := a.ha.UsePairing(req.Secret, time.Now()); err != nil {
		log.Printf("ha: a pairing from %s refused: %v", clientAddr(r), err)
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	secret, err := ha.NewSecret()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	peer := ha.Peer{Name: req.Name, Address: req.Address, CertSHA256: ha.Fingerprint(block.Bytes), PairedAt: time.Now().UTC()}
	if err := a.ha.SetPeer(peer, secret, ha.Active, st.Epoch); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Printf("ha: paired with the standby %s (%s, version %s) from %s - epoch %d", peer.Name, peer.Address, req.Version, clientAddr(r), st.Epoch)
	writeJSON(w, http.StatusOK, haPairResponse{Secret: base64.RawURLEncoding.EncodeToString(secret), Name: haName(), Epoch: st.Epoch, Version: version})
}

func (a *app) handleHAState(w http.ResponseWriter, _ *http.Request) {
	st := a.ha.State()
	role := st.Role
	if a.restored.Load() {
		role = ha.Standby // stepping down
	}
	writeJSON(w, http.StatusOK, ha.PeerState{Role: role, Epoch: st.Epoch, Version: version, Name: haName()})
}

// handleHAReplica is the standby's copy: the data directory and the
// master key - not the nodes' configurations, which a promotion doesn't
// need (the nodes keep them).
func (a *app) handleHAReplica(w http.ResponseWriter, r *http.Request) {
	st := a.ha.State()
	if st.Role != ha.Active || a.restored.Load() {
		writeError(w, http.StatusConflict, "this Controller isn't the active")
		return
	}
	archive, m, err := a.buildArchive(r.Context(), false)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeReplica(w, archive, st.Epoch, m.Created)
	if err := a.ha.NoteSeen(ha.Seen{At: time.Now().UTC(), Version: r.Header.Get(ha.HeaderVersion), From: clientAddr(r)}); err != nil {
		log.Printf("ha: %v", err)
	}
}

func writeReplica(w http.ResponseWriter, archive []byte, epoch uint64, created time.Time) {
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set(ha.HeaderEpoch, strconv.FormatUint(epoch, 10))
	w.Header().Set(ha.HeaderVersion, version)
	w.Header().Set(ha.HeaderCreated, created.Format(time.RFC3339Nano))
	w.Header().Set("Content-Length", strconv.Itoa(len(archive)))
	_, _ = w.Write(archive)
}

// handleHAStepDown is the switchover: the active hands its standby a last
// copy, becomes a standby - durably, before the copy leaves - and starts
// again as one; nothing it does after this copy is lost.
func (a *app) handleHAStepDown(w http.ResponseWriter, r *http.Request) {
	st := a.ha.State()
	if st.Role != ha.Active || !a.haStepping.CompareAndSwap(false, true) {
		writeError(w, http.StatusConflict, "this Controller isn't the active")
		return
	}
	// The main port answers no more from now on: no change after the
	// last copy.
	a.restored.Store(true)
	archive, m, err := a.buildArchive(r.Context(), false)
	if err == nil {
		err = a.ha.SetRole(ha.Standby, st.Epoch)
	}
	if err != nil {
		a.restored.Store(false)
		a.haStepping.Store(false)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Printf("ha: stepping down for the standby %s at epoch %d - starting again as its standby", peerName(st), st.Epoch)
	writeReplica(w, archive, st.Epoch, m.Created)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	go restartSelf()
}

func peerName(st ha.State) string {
	if st.Peer == nil {
		return "?"
	}
	return st.Peer.Name + " (" + st.Peer.Address + ")"
}

// --- the main port: the admins' routes ---

func (a *app) registerHARoutes(mux *http.ServeMux) {
	// What a keepalived track_script asks: 200 on the active only.
	mux.HandleFunc("GET /api/ha/active", func(w http.ResponseWriter, _ *http.Request) {
		if a.ha == nil {
			writeJSON(w, http.StatusOK, map[string]any{"role": ha.Active})
			return
		}
		st := a.ha.State()
		if st.Role != ha.Active || a.restored.Load() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"role": ha.Standby, "epoch": st.Epoch})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"role": ha.Active, "epoch": st.Epoch})
	})
	mux.HandleFunc("GET /api/ha", a.gate(auth.Admin, auth.Admin, a.handleHAStatus))
	mux.HandleFunc("POST /api/ha/pairing", a.sessionGate(auth.Admin, auth.Admin, a.handleHAPairing))
	mux.HandleFunc("DELETE /api/ha/standby", a.gate(auth.Admin, auth.Admin, a.handleHAUnpair))
	mux.HandleFunc("POST /api/ha/switchover", a.sessionGate(auth.Admin, auth.Admin, a.handleHASwitchover))
}

func (a *app) handleHAStatus(w http.ResponseWriter, _ *http.Request) {
	if a.ha == nil {
		writeError(w, http.StatusNotFound, "no high availability on this Controller")
		return
	}
	st := a.ha.State()
	out := map[string]any{"role": st.Role, "epoch": st.Epoch, "version": version, "name": haName(), "register_address": a.suggestedRegisterAddr}
	if st.Peer != nil {
		out["standby"] = map[string]any{"name": st.Peer.Name, "address": st.Peer.Address, "paired_at": st.Peer.PairedAt, "cert_sha256": st.Peer.CertSHA256}
		if seen := a.ha.Seen(); !seen.At.IsZero() {
			out["last_copy"] = seen
		}
		if v := a.haView.view(); v != nil {
			out["peer"] = v
		}
		if v := a.haWitness.view(); v != nil {
			out["witness"] = v
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleHAPairing makes a pairing token, good an hour, once - shown once.
func (a *app) handleHAPairing(w http.ResponseWriter, r *http.Request) {
	if a.ha == nil {
		writeError(w, http.StatusNotFound, "no high availability on this Controller")
		return
	}
	if st := a.ha.State(); st.Peer != nil {
		writeError(w, http.StatusConflict, "this Controller has a standby already ("+st.Peer.Name+"): remove it first")
		return
	}
	token, exp, err := a.ha.NewPairing(time.Now())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	noteAudit(r, "", "ha pairing token")
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "expires": exp, "register_address": a.suggestedRegisterAddr})
}

func (a *app) handleHAUnpair(w http.ResponseWriter, _ *http.Request) {
	if a.ha == nil || a.ha.State().Peer == nil {
		writeError(w, http.StatusNotFound, "no standby")
		return
	}
	st := a.ha.State()
	if err := a.ha.Unpair(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Printf("ha: the standby %s is forgotten - it can't copy this Controller any more", peerName(st))
	w.WriteHeader(http.StatusNoContent)
}

// handleHASwitchover asks the standby to take over: it asks this
// Controller to step down, takes its last copy and starts as the active.
func (a *app) handleHASwitchover(w http.ResponseWriter, r *http.Request) {
	c := a.haClient()
	if c == nil {
		writeError(w, http.StatusConflict, "no standby to switch over to")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if _, _, err := c.Do(ctx, http.MethodPost, "/ha/promote", nil, 1<<16); err != nil {
		writeError(w, http.StatusBadGateway, "the standby: "+err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"message": "the standby is taking over: this Controller steps down in a moment"})
}

func (a *app) haClient() *ha.Client {
	if a.ha == nil {
		return nil
	}
	return a.ha.Client()
}

// --- the active's loop ---

// haLoop asks the standby where it stands every haCheckEvery, and reads
// the witness: one promoted above this Controller's epoch - while this
// one was cut off from it - makes it step down.
func (a *app) haLoop(ctx context.Context) {
	t := time.NewTicker(haCheckEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		c := a.haClient()
		if c == nil || a.restored.Load() {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		st, err := c.State(cctx)
		cancel()
		a.haView.set(&st, err)
		if err == nil && ha.StepDown(a.ha.State(), st.Role, st.Epoch) {
			a.stepDownFor(st, "")
			return
		}
		wctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		above := a.checkWitness(wctx)
		cancel()
		if above != nil {
			a.stepDownFor(*above, "the witness says ")
			return
		}
	}
}

// stepDownFor makes this Controller its peer's standby: the peer was
// promoted above it - it says so, or the witness does. What it did since
// its last copy is set aside at its next start (standby.go).
func (a *app) stepDownFor(peer ha.PeerState, how string) {
	log.Printf("ha: %sthe peer %s is the active at epoch %d, above this Controller's %d - stepping down, starting again as its standby", how, peer.Name, peer.Epoch, a.ha.State().Epoch)
	a.restored.Store(true)
	if err := a.ha.SetRole(ha.Standby, peer.Epoch); err != nil {
		log.Fatalf("ha: stepping down: %v - stop this Controller", err)
	}
	go restartSelf()
}

// fencedAtStart reports whether a Controller starting as the active finds
// its peer active above its epoch - it then starts as the standby, before
// any loop runs. A peer that doesn't answer leaves it the active, unless
// the witness - witness() - says the peer holds an epoch above it.
func fencedAtStart(s *ha.Store, witness func() (ha.Witness, error)) (ha.PeerState, bool) {
	st := s.State()
	c := s.Client()
	if st.Role != ha.Active || c == nil {
		return ha.PeerState{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	peer, err := c.State(ctx)
	if err == nil {
		return peer, ha.StepDown(st, peer.Role, peer.Epoch)
	}
	w, werr := witness()
	if werr != nil {
		log.Printf("ha: the peer %s doesn't answer (%v), and %v - starting as the active, epoch %d", peerName(st), err, werr, st.Epoch)
		return ha.PeerState{}, false
	}
	wctx, wcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer wcancel()
	above, werr := witnessFenced(wctx, s, w)
	switch {
	case werr != nil:
		log.Printf("ha: the peer %s doesn't answer (%v), nor the witness %s (%v) - starting as the active, epoch %d", peerName(st), err, w.Where(), werr, st.Epoch)
	case above != nil:
		log.Printf("ha: the peer %s doesn't answer (%v), but the witness %s says it holds epoch %d", peerName(st), err, w.Where(), above.Epoch)
		return *above, true
	default:
		log.Printf("ha: the peer %s doesn't answer (%v) - the witness %s has none above this Controller: starting as the active, epoch %d", peerName(st), err, w.Where(), st.Epoch)
	}
	return ha.PeerState{}, false
}

// newerThanThis reports whether v is a release newer than this
// Controller: a copy it couldn't be sure to read.
func newerThanThis(v string) bool {
	c, known := calver.Compare(v, version)
	return known && c > 0
}
