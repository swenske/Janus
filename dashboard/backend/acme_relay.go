package main

import (
	"crypto/x509"
	"log"
	"net"
	"net/http"
	"path/filepath"

	"github.com/swenske/Janus/dashboard/backend/internal/acmerelay"
	"github.com/swenske/Janus/dashboard/backend/internal/auth"
	"github.com/swenske/Janus/dashboard/backend/internal/nodeproxy"
	"github.com/swenske/Janus/internal/acme"
)

// The relay for the nodes that reach Let's Encrypt through the
// Controller (internal/acmerelay, docs/guide/letsencrypt-controller.md):
// off until an admin turns it on on the Let's Encrypt tab, its settings
// in <data>/acme-relay.json. It serves the Controller's own identity -
// what the nodes register against - and knows a node by a certificate
// of the node's CA, the one the Controller pins.

const acmeRelayFile = "acme-relay.json"

func (a *app) startACMERelay() {
	a.relay = &acmerelay.Server{
		Identity: a.identity.Certificate,
		NodeOf:   a.relayNode,
	}
	cfg, err := acmerelay.Load(filepath.Join(a.dataDir, acmeRelayFile))
	if err != nil {
		log.Printf("acme relay: %v", err)
	}
	a.relay.Boot(cfg)
	nodeproxy.ACMERelay = a.relayInfo
}

// relayNode is the approved node whose CA issued chain[0], a client
// certificate.
func (a *app) relayNode(chain []*x509.Certificate) (acmerelay.Node, bool) {
	if len(chain) == 0 {
		return acmerelay.Node{}, false
	}
	inter := x509.NewCertPool()
	for _, c := range chain[1:] {
		inter.AddCert(c)
	}
	for _, n := range a.store.List() {
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(n.CA()) {
			continue
		}
		opts := x509.VerifyOptions{Roots: roots, Intermediates: inter, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
		if _, err := chain[0].Verify(opts); err == nil {
			return acmerelay.Node{ID: n.ID, Name: n.Name}, true
		}
	}
	return acmerelay.Node{}, false
}

// relayAddress is where the nodes reach the relay: the admin's, else the
// host they're told to register against, with the port it listens on
// (listening: its actual one).
func (a *app) relayAddress(st acmerelay.Status) string {
	cfg := st.Config.Normalized()
	if cfg.Address != "" {
		return cfg.Address
	}
	listen := cfg.Listen
	if st.Listening != "" {
		listen = st.Listening
	}
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return ""
	}
	host, _, err := net.SplitHostPort(a.suggestedRegisterAddr)
	if err != nil || host == "" {
		return ""
	}
	return net.JoinHostPort(host, port)
}

// relayView is the relay as the Let's Encrypt tab shows it.
type relayView struct {
	acmerelay.Status
	// URL is what a node is given (ACMEPath.proxy_url), CACertPEM the
	// Controller's identity it checks the relay with (proxy_ca).
	URL       string `json:"url,omitempty"`
	CACertPEM string `json:"ca_cert_pem"`
	// NotThroughRelay are the DNS providers it can never carry, and why.
	NotThroughRelay map[string]string `json:"not_through_relay"`
}

func (a *app) relayView() relayView {
	st := a.relay.Status()
	v := relayView{Status: st, CACertPEM: a.identityPEM(), NotThroughRelay: acme.NotThroughRelay}
	if addr := a.relayAddress(st); addr != "" {
		v.URL = "https://" + addr
	}
	return v
}

// identityPEM is what nodes check the Controller's own identity with:
// its CA (identity.TrustPEM).
func (a *app) identityPEM() string { return string(a.identity.TrustPEM()) }

// relayInfo is what a node page needs to send its node through the
// relay.
func (a *app) relayInfo() nodeproxy.RelayInfo {
	v := a.relayView()
	return nodeproxy.RelayInfo{
		Enabled: v.Config.Enabled, Listening: v.Listening != "", URL: v.URL, CACertPEM: v.CACertPEM,
		Error: v.Error, BuiltIn: v.BuiltIn, Allowed: v.Config.Allowed, NotThroughRelay: v.NotThroughRelay,
	}
}

func (a *app) registerACMERelayRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/acme-relay", a.gate(auth.Admin, auth.Admin, a.handleACMERelay))
	mux.HandleFunc("PUT /api/acme-relay", a.gate(auth.Admin, auth.Admin, a.handleACMERelaySet))
}

func (a *app) handleACMERelay(w http.ResponseWriter, r *http.Request) {
	if a.relay == nil {
		writeError(w, http.StatusServiceUnavailable, "the relay isn't running")
		return
	}
	writeJSON(w, http.StatusOK, a.relayView())
}

// handleACMERelaySet applies and saves the relay's settings: refused,
// nothing changed, when they're wrong or its address can't be listened
// on.
func (a *app) handleACMERelaySet(w http.ResponseWriter, r *http.Request) {
	if a.relay == nil {
		writeError(w, http.StatusServiceUnavailable, "the relay isn't running")
		return
	}
	var cfg acmerelay.Config
	if !decodeBody(w, r, &cfg) {
		return
	}
	if err := a.relay.Apply(cfg); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := acmerelay.Save(filepath.Join(a.dataDir, acmeRelayFile), cfg); err != nil {
		writeError(w, http.StatusInternalServerError, "save: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a.relayView())
}
