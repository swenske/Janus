package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/auth"
	"github.com/swenske/Janus/dashboard/backend/internal/enroll"
	"github.com/swenske/Janus/dashboard/backend/internal/pending"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
	"github.com/swenske/Janus/internal/pki"
)

// startRegistrationListener serves the node self-registration endpoint
// on its own dedicated TLS port - deliberately separate from :8080
// (plain HTTP, human-facing, now password-gated - see internal/auth)
// and the per-node listener pool (9500-9599, mTLS-gated per *approved*
// node): a self-announcing node doesn't have an approved identity yet,
// so there's nothing to gate this behind except "is this really the
// Controller", which the node itself checks via the same CA cert it
// was given at provisioning time (see internal/pending's own doc
// comment, and the node-side half of this design, not yet built).
// tls.NoClientCert on purpose: the whole point of this endpoint is to
// accept an announcement from a node the Controller has never seen
// before, so there's no client certificate to require yet.
func (a *app) startRegistrationListener(addr string) error {
	// A node provisioned with the fleet's root asks for the fleet's name
	// (SNI) and gets the fleet certificate; any other - provisioned with
	// this Controller's own certificate - gets that one.
	tlsConfig := &tls.Config{
		ClientAuth: tls.NoClientCert,
		MinVersion: tls.VersionTLS13,
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			if hello.ServerName == pki.FleetControllerName && a.fleet != nil {
				if c, err := a.fleet.ServerCertificate(); err == nil {
					return c, nil
				}
			}
			return &a.serverCert, nil
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/register", a.handleRegister)
	mux.HandleFunc("GET /register/{id}", a.handleRegisterPoll)

	ln, err := tls.Listen("tcp", addr, tlsConfig)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	srv := &http.Server{Handler: mux}
	go func() {
		// ErrServerClosed is the expected outcome of a graceful shutdown,
		// not a real failure - dashboardd has no such shutdown path today
		// (it just runs until killed), but the check costs nothing and
		// matches nodeproxy.Listener's own pattern.
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("registration listener: %v", err)
		}
	}()
	log.Printf("dashboardd registration endpoint listening on %s", addr)
	return nil
}

// registerRequest is what a self-registering node posts. ServiceCertPEM/
// ServiceKeyPEM are already the credential the node generated for
// itself - see internal/pending's own doc comment for why this handler
// doesn't need to exchange a bootstrap credential for anything, unlike
// the human-driven add-node flow.
type registerRequest struct {
	Name           string `json:"name"`
	Address        string `json:"address"`
	CACertPEM      string `json:"ca_cert_pem"`
	ServiceCertPEM string `json:"service_cert_pem"`
	ServiceKeyPEM  string `json:"service_key_pem"`
	// RegistrationToken is set by a node the Controller created itself
	// (machines.go): the one-time token from its NoCloud volume.
	RegistrationToken string `json:"registration_token,omitempty"`
	// Protocol 2: no service credential - the node polls with
	// PollSecret, and gets the fleet's trust once admitted.
	Protocol   int    `json:"protocol,omitempty"`
	PollSecret string `json:"poll_secret,omitempty"`
}

// keylessTrust is what an admitted keyless node gets: the fleet's root
// and the bundle it signed (internal/selfregister.Trust).
type keylessTrust struct {
	RootCert string `json:"root_cert"`
	Bundle   []byte `json:"bundle"`
}

func (a *app) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req registerRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "decode request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Protocol >= 2 {
		a.registerKeyless(w, req)
		return
	}
	if req.Name == "" || req.Address == "" || req.CACertPEM == "" || req.ServiceCertPEM == "" || req.ServiceKeyPEM == "" {
		http.Error(w, "name, address, ca_cert_pem, service_cert_pem, and service_key_pem are all required", http.StatusBadRequest)
		return
	}
	// Catch an obviously malformed cert/key pair now, with a real
	// error, rather than only failing later when an operator approves
	// it and the per-node listener fails to start.
	if _, err := tls.X509KeyPair([]byte(req.ServiceCertPEM), []byte(req.ServiceKeyPEM)); err != nil {
		http.Error(w, "service_cert_pem/service_key_pem don't form a valid certificate: "+err.Error(), http.StatusBadRequest)
		return
	}

	node := &pending.Node{
		Name:           req.Name,
		Address:        req.Address,
		CACertPEM:      []byte(req.CACertPEM),
		ServiceCertPEM: []byte(req.ServiceCertPEM),
		ServiceKeyPEM:  []byte(req.ServiceKeyPEM),
	}

	// A machine this Controller created presents the token it was given:
	// creating it was the approval, so it's admitted directly. Anything
	// else - no token, an unknown, used or expired one - joins the queue
	// like every other registration.
	if admitted, ok := a.admitEnrolled(node, req.RegistrationToken); ok {
		writeJSON(w, http.StatusCreated, struct {
			ID       string `json:"id"`
			Admitted bool   `json:"admitted"`
		}{ID: admitted.ID, Admitted: true})
		return
	}
	if req.RegistrationToken != "" && !enroll.Is(req.RegistrationToken) {
		if m, ok := a.machines.ClaimToken(req.RegistrationToken); ok {
			admitted, err := a.admit(node, m.ID)
			if err == nil {
				a.runner.registered(m.ID, admitted)
				log.Printf("node self-registered: %s (%s), admitted as machine %s", admitted.Name, admitted.Address, m.ID)
				writeJSON(w, http.StatusCreated, struct {
					ID       string `json:"id"`
					Admitted bool   `json:"admitted"`
				}{ID: admitted.ID, Admitted: true})
				return
			}
			log.Printf("machine %s: admitting its node failed, queueing it for approval instead: %v", m.ID, err)
			a.runner.logEvent(m.ID, "admitting the node failed (%v): it waits for approval instead", err)
		} else {
			log.Printf("node self-registered with a registration token no machine is waiting for: %s (%s)", req.Name, req.Address)
		}
	}

	if err := a.pending.Add(node); err != nil {
		http.Error(w, "record registration: "+err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("node self-registered: %s (%s), awaiting approval", node.Name, node.Address)
	writeJSON(w, http.StatusCreated, struct {
		ID string `json:"id"`
	}{ID: node.ID})
}

// handlePendingList is a read-only GET /api/pending, auth-gated like
// everything else under /api/.
func (a *app) handlePendingList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if requestPrincipal(r).Role == auth.None {
		writeJSON(w, http.StatusOK, []struct{}{})
		return
	}
	type pendingView struct {
		ID          string    `json:"id"`
		Name        string    `json:"name"`
		Address     string    `json:"address"`
		AnnouncedAt time.Time `json:"announced_at"`
		// CAFingerprint is the node's CA, as its console says it when it
		// announces itself - to compare before approving.
		CAFingerprint string `json:"ca_fingerprint,omitempty"`
		Keyless       bool   `json:"keyless,omitempty"`
	}
	var out []pendingView
	for _, n := range a.pending.List() {
		v := pendingView{ID: n.ID, Name: n.Name, Address: n.Address, AnnouncedAt: n.AnnouncedAt, Keyless: n.Keyless()}
		if c, err := parseCertPEM(n.CACertPEM); err == nil {
			v.CAFingerprint = pki.Fingerprint(c.Raw)
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

// handlePendingAction dispatches POST /api/pending/{id}/approve and
// POST /api/pending/{id}/reject - the two actions a human takes on the
// "pending" queue, both auth-gated like everything under /api/.
func (a *app) handlePendingAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/pending/")
	id, action, ok := strings.Cut(rest, "/")
	if !ok || id == "" || action == "" {
		http.Error(w, "usage: POST /api/pending/{id}/approve or /reject", http.StatusBadRequest)
		return
	}

	switch action {
	case "approve":
		a.approvePending(w, id)
	case "reject":
		a.rejectPending(w, id)
	default:
		http.Error(w, fmt.Sprintf("unknown action %q (want approve or reject)", action), http.StatusBadRequest)
	}
}

// approvePending moves a pending entry into the real node registry and
// starts its listener - the credential itself was already generated by
// the node at announcement time (see internal/pending's own doc
// comment), so unlike the human-driven add-node flow there's no
// GenerateClientConfiguration round trip here: the data just moves from
// one store to the other.
func (a *app) approvePending(w http.ResponseWriter, id string) {
	p, ok := a.pending.Get(id)
	if !ok {
		http.Error(w, fmt.Sprintf("no pending node %q", id), http.StatusNotFound)
		return
	}

	// A node of a machine this Controller created, from an image too old
	// to present its token: once approved, it's that machine's (the
	// approval is the operator's, the name only says which machine).
	machineID := a.runner.waitingFor(p.Name)
	node, err := a.admit(p, machineID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Only remove the pending entry once the node is genuinely live -
	// a failed approval above leaves it in the queue so it can be
	// retried, rather than silently losing the announcement. A keyless
	// one stays, unlisted, until its node fetched its trust.
	if p.Keyless() {
		if err := a.pending.MarkApproved(id, node.ID); err != nil {
			log.Printf("pending node %s: approved but failed to record it: %v", id, err)
		}
	} else if err := a.pending.Remove(id); err != nil {
		log.Printf("pending node %s: approved but failed to remove from the pending queue: %v", id, err)
	}
	if machineID != "" {
		a.runner.registered(machineID, node)
	}
	log.Printf("node approved: %s (%s), its page at /nodes/%s/", node.Name, node.Address, node.ID)
	writeJSON(w, http.StatusCreated, struct {
		ID string `json:"id"`
	}{ID: node.ID})
}

// admit turns a registration into a node - a store entry, its page at
// /nodes/<id>/ - for an approved pending entry, or straight away for a
// machine this Controller created (machineID). The registration itself
// already carries the node's credential (see internal/pending's doc
// comment): nothing is exchanged with the node.
func (a *app) admit(p *pending.Node, machineID string) (*store.Node, error) {
	return a.admitLabelled(p, machineID, nil)
}

// admitLabelled is admit, the node given labels.
func (a *app) admitLabelled(p *pending.Node, machineID string, l map[string]string) (*store.Node, error) {
	node := &store.Node{
		Name:           p.Name,
		Address:        p.Address,
		MachineID:      machineID,
		CACertPEM:      p.CACertPEM,
		ServiceCertPEM: p.ServiceCertPEM,
		ServiceKeyPEM:  p.ServiceKeyPEM,
		// A keyless node takes the fleet's trust when admitted: the
		// Controller reaches it with its fleet certificate from the start.
		Fleet:  p.Keyless(),
		Labels: l,
	}
	if err := a.store.Add(node); err != nil {
		return nil, fmt.Errorf("persist node: %w", err)
	}
	a.trust.Kick() // brought to trust the fleet at once
	return node, nil
}

// admitEnrolled admits node on an enrollment token (internal/enroll) - a
// use of it - with the token's labels: false when the token admits
// nobody (unknown, used up, expired, revoked), and the node waits for
// approval like any other.
func (a *app) admitEnrolled(node *pending.Node, token string) (*store.Node, bool) {
	if a.enroll == nil || !enroll.Is(token) {
		return nil, false
	}
	t, ok := a.enroll.Claim(token)
	if !ok {
		log.Printf("node self-registered with an enrollment token that admits nobody (unknown, used up, expired or revoked): %s (%s) - awaiting approval", node.Name, node.Address)
		return nil, false
	}
	admitted, err := a.admitLabelled(node, "", t.Labels)
	if err != nil {
		log.Printf("enrollment token %q: admitting %s failed, queueing it for approval instead: %v", t.Name, node.Name, err)
		return nil, false
	}
	a.enroll.Admitted(t.ID, admitted.ID)
	log.Printf("node self-registered: %s (%s), admitted on enrollment token %q (use %d of %d)", admitted.Name, admitted.Address, t.Name, t.Uses, t.MaxUses)
	return admitted, true
}

// rejectPending just discards the announcement - the node itself isn't
// notified (there's no channel to notify it over; it'll either be
// re-provisioned with a corrected controller address/CA, or a human
// investigates why it showed up at all).
func (a *app) rejectPending(w http.ResponseWriter, id string) {
	if err := a.pending.Remove(id); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	log.Printf("pending node %s rejected", id)
	w.WriteHeader(http.StatusNoContent)
}

// registerKeyless takes a protocol 2 announcement: no key at all - the
// node's CA (public), its name and address, a token if it has one, the
// secret it polls with. Admitted at once for a token this Controller
// made, else queued for approval; either way, the node gets the fleet's
// trust and is reached with the fleet certificate. Without a fleet, 409:
// the node announces with a service credential instead.
func (a *app) registerKeyless(w http.ResponseWriter, req registerRequest) {
	if req.Name == "" || req.Address == "" || req.CACertPEM == "" || len(req.PollSecret) < 32 {
		http.Error(w, "name, address, ca_cert_pem and poll_secret (32 characters or more) are all required", http.StatusBadRequest)
		return
	}
	ca, err := parseCertPEM([]byte(req.CACertPEM))
	if err != nil || !ca.IsCA {
		http.Error(w, "ca_cert_pem isn't a CA certificate", http.StatusBadRequest)
		return
	}
	var rootPEM, bundle []byte
	ok := false
	if a.fleet != nil { // nil only in tests
		rootPEM, bundle, _, ok = a.fleet.Trust()
	}
	if !ok {
		http.Error(w, "this Controller's fleet isn't set up yet: announce with a service credential", http.StatusConflict)
		return
	}
	sum := sha256.Sum256([]byte(req.PollSecret))
	node := &pending.Node{Name: req.Name, Address: req.Address, CACertPEM: []byte(req.CACertPEM), PollSecretHash: sum[:]}
	fingerprint := pki.Fingerprint(ca.Raw)

	if admitted, ok := a.admitEnrolled(node, req.RegistrationToken); ok {
		writeJSON(w, http.StatusCreated, struct {
			ID       string        `json:"id"`
			Admitted bool          `json:"admitted"`
			Trust    *keylessTrust `json:"trust"`
		}{admitted.ID, true, &keylessTrust{RootCert: string(rootPEM), Bundle: bundle}})
		return
	}
	if req.RegistrationToken != "" && !enroll.Is(req.RegistrationToken) {
		if m, ok := a.machines.ClaimToken(req.RegistrationToken); ok {
			admitted, err := a.admit(node, m.ID)
			if err == nil {
				a.runner.registered(m.ID, admitted)
				log.Printf("node self-registered with no key: %s (%s), admitted as machine %s", admitted.Name, admitted.Address, m.ID)
				writeJSON(w, http.StatusCreated, struct {
					ID       string        `json:"id"`
					Admitted bool          `json:"admitted"`
					Trust    *keylessTrust `json:"trust"`
				}{admitted.ID, true, &keylessTrust{RootCert: string(rootPEM), Bundle: bundle}})
				return
			}
			log.Printf("machine %s: admitting its node failed, queueing it for approval instead: %v", m.ID, err)
			a.runner.logEvent(m.ID, "admitting the node failed (%v): it waits for approval instead", err)
		} else {
			log.Printf("node self-registered with a registration token no machine is waiting for: %s (%s)", req.Name, req.Address)
		}
	}
	if err := a.pending.Add(node); err != nil {
		http.Error(w, "record registration: "+err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("node self-registered with no key: %s (%s), its CA SHA-256 %s - awaiting approval", node.Name, node.Address, fingerprint)
	writeJSON(w, http.StatusCreated, struct {
		ID string `json:"id"`
	}{node.ID})
}

// handleRegisterPoll answers a keyless node asking whether it was
// approved - with the secret it announced itself with: 202 while it
// waits, the fleet's trust once approved, 404 when rejected (or
// unknown).
func (a *app) handleRegisterPoll(w http.ResponseWriter, r *http.Request) {
	p, ok := a.pending.Get(r.PathValue("id"))
	if !ok || !p.Keyless() {
		http.NotFound(w, r)
		return
	}
	sum := sha256.Sum256([]byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")))
	if subtle.ConstantTimeCompare(sum[:], p.PollSecretHash) != 1 {
		http.Error(w, "not this enrollment's secret", http.StatusForbidden)
		return
	}
	if !p.Approved {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	rootPEM, bundle, _, ok := a.fleet.Trust()
	if !ok {
		http.Error(w, "this Controller has no fleet any more", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, keylessTrust{RootCert: string(rootPEM), Bundle: bundle})
	log.Printf("node %s (%s) fetched the fleet's trust", p.Name, p.NodeID)
	// It applies it now: check in a moment, rather than in ten minutes.
	time.AfterFunc(5*time.Second, a.trust.Kick)
}

func parseCertPEM(p []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(p)
	if block == nil {
		return nil, errors.New("no PEM certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}
