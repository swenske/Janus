package main

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/auth"
	"github.com/swenske/Janus/dashboard/backend/internal/fleet"
)

// janusctl's side of the Controller (/api/cli/*): a short certificate of
// the fleet for an account - signed for janusctl's own key, never one
// the Controller makes -, and the nodes to use it with. janusctl then
// talks to the nodes directly; each one checks the certificate's role
// itself.

// Certificate lifetimes: a working day from a sign-in, an hour from an
// API token (a CI job).
const (
	cliSessionTTL = 12 * time.Hour
	cliTokenTTL   = time.Hour
)

func (a *app) registerCLIRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/cli/certificate", a.gate(auth.Reader, auth.Reader, a.handleCLICertificate))
	mux.HandleFunc("GET /api/cli/inventory", a.gate(auth.Reader, auth.Reader, a.handleCLIInventory))
}

type cliCertificate struct {
	// CertificatePEM is the certificate, followed by the fleet's issuing
	// CA.
	CertificatePEM string    `json:"certificate_pem"`
	ExpiresAt      time.Time `json:"expires_at"`
	User           string    `json:"user"`
	// Role is the node role it carries.
	Role string `json:"role"`
}

// handleCLICertificate signs the request's CSR (its key janusctl's own)
// for the account, with the request's role: an hour from an API token,
// twelve from a session.
func (a *app) handleCLICertificate(w http.ResponseWriter, r *http.Request) {
	p, _ := principalOf(r)
	var req struct {
		CSRPEM string `json:"csr_pem"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	csr, err := parseCSR([]byte(req.CSRPEM))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ttl := cliSessionTTL
	if p.Token != "" {
		ttl = cliTokenTTL
	}
	if a.fleet == nil {
		writeError(w, http.StatusConflict, "set up the fleet first: janusctl's certificates are the fleet's")
		return
	}
	role := nodeRole(p.Role)
	chain, notAfter, err := a.fleet.IssueUser(csr.PublicKey, p.User, role, ttl)
	if errors.Is(err, fleet.ErrState) {
		writeError(w, http.StatusConflict, "set up the fleet first: janusctl's certificates are the fleet's")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Printf("issued a janusctl certificate to %s (%s, %s) until %s", p.User, role, p.via(), notAfter.Format(time.RFC3339))
	writeJSON(w, http.StatusOK, cliCertificate{CertificatePEM: string(chain), ExpiresAt: notAfter, User: p.User, Role: role})
}

// parseCSR reads a CSR, checks it's signed by its own key - the caller
// holds it - and that the key is one a node takes.
func parseCSR(p []byte) (*x509.CertificateRequest, error) {
	block, _ := pem.Decode(p)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, errors.New("csr_pem: a PEM certificate request is required")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, err
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, errors.New("the certificate request isn't signed by its own key")
	}
	switch k := csr.PublicKey.(type) {
	case ed25519.PublicKey:
	case *ecdsa.PublicKey:
		if k.Curve != elliptic.P256() && k.Curve != elliptic.P384() {
			return nil, errors.New("an ECDSA key must be P-256 or P-384")
		}
	case *rsa.PublicKey:
		if k.N.BitLen() < 2048 {
			return nil, errors.New("an RSA key must be at least 2048 bits")
		}
	default:
		return nil, errors.New("the key must be Ed25519, ECDSA or RSA")
	}
	return csr, nil
}

type cliNode struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Address string `json:"address"`
	// CAPEM is the node's own CA: what its server certificate is checked
	// against.
	CAPEM string `json:"ca_pem"`
	// Fleet: the node trusts the fleet - a janusctl certificate opens it.
	Fleet bool `json:"fleet"`
}

// handleCLIInventory lists the nodes janusctl reaches, and how.
func (a *app) handleCLIInventory(w http.ResponseWriter, _ *http.Request) {
	out := struct {
		Nodes []cliNode `json:"nodes"`
	}{Nodes: []cliNode{}}
	for _, n := range a.store.List() {
		out.Nodes = append(out.Nodes, cliNode{ID: n.ID, Name: n.Name, Address: n.Addr(), CAPEM: string(n.CA()), Fleet: n.TrustsFleet()})
	}
	writeJSON(w, http.StatusOK, out)
}
