package main

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/swenske/Janus/internal/sshsig"

	"github.com/swenske/Janus/dashboard/backend/internal/auth"
	"github.com/swenske/Janus/dashboard/backend/internal/fleet"
)

// janusctl's side of the Controller (/api/cli/*): a short certificate of
// the fleet for an account - signed for janusctl's own key, never one
// the Controller makes -, and the nodes to use it with. janusctl then
// talks to the nodes directly; each one checks the certificate's role -
// or its scope (cli_scope.go) - itself.

// Certificate lifetimes: a working day from a sign-in, an hour from an
// API token (a CI job).
const (
	cliSessionTTL = 12 * time.Hour
	cliTokenTTL   = time.Hour
)

func (a *app) registerCLIRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/cli/certificate", a.gate(anyone, anyone, a.handleCLICertificate))
	mux.HandleFunc("GET /api/cli/inventory", a.gate(anyone, anyone, a.handleCLIInventory))
}

type cliCertificate struct {
	// CertificatePEM is the certificate, followed by the fleet's issuing
	// CA.
	CertificatePEM string    `json:"certificate_pem"`
	ExpiresAt      time.Time `json:"expires_at"`
	User           string    `json:"user"`
	// Role is what it carries: the node role, or what its scope gives.
	Role string `json:"role"`
}

// handleCLICertificate signs the request's CSR (its key janusctl's own)
// for the account, with what the request may do (cliIdentityFor): an
// hour from an API token, twelve from a session.
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
	id, err := a.cliIdentityFor(p)
	if err != nil {
		writeError(w, http.StatusForbidden, p.User+" "+err.Error())
		return
	}
	chain, notAfter, err := a.fleet.IssueUser(csr.PublicKey, p.User, id.Role, id.Scope, ttl)
	if errors.Is(err, fleet.ErrState) {
		writeError(w, http.StatusConflict, "set up the fleet first: janusctl's certificates are the fleet's")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Printf("issued a janusctl certificate to %s (%s, %s) until %s", p.User, id.Summary, p.via(), notAfter.Format(time.RFC3339))
	writeJSON(w, http.StatusOK, cliCertificate{CertificatePEM: string(chain), ExpiresAt: notAfter, User: p.User, Role: id.Summary})
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
func (a *app) handleCLIInventory(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, struct {
		Nodes []cliNode `json:"nodes"`
	}{a.cliNodes(requestPrincipal(r))})
}

// cliNodes are the nodes p reaches.
func (a *app) cliNodes(p principal) []cliNode {
	out := []cliNode{}
	for _, n := range a.visibleNodes(p) {
		out = append(out, cliNode{ID: n.ID, Name: n.Name, Address: n.Addr(), CAPEM: string(n.CA()), Fleet: n.TrustsFleet()})
	}
	return out
}

// --- SSH keys: what janusctl signs in with ---

// servedFingerprint is the SHA-256 (hex) of the certificate the main port
// serves: what janusctl pins, and what an SSH sign-in's signature names -
// a signature made for another server is worth nothing here.
func (a *app) servedFingerprint() string {
	sum := sha256.Sum256(a.serverCert.Certificate[0])
	return hex.EncodeToString(sum[:])
}

func (a *app) registerSSHKeyRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/auth/ssh-keys", a.sessionGate(anyone, anyone, a.handleSSHKeyList))
	mux.HandleFunc("POST /api/auth/ssh-keys", a.sessionGate(anyone, anyone, a.handleSSHKeyAdd))
	mux.HandleFunc("DELETE /api/auth/ssh-keys/{fingerprint...}", a.sessionGate(anyone, anyone, a.handleSSHKeyRemove))
	mux.HandleFunc("POST /api/cli/challenge", a.handleCLIChallenge)
	mux.HandleFunc("POST /api/cli/ssh-login", a.handleCLISSHLogin)
}

func (a *app) handleSSHKeyList(w http.ResponseWriter, r *http.Request) {
	p, _ := principalOf(r)
	u, _ := a.auth.User(p.User)
	writeJSON(w, http.StatusOK, nonNil(u.SSHKeys))
}

// handleSSHKeyAdd gives the account a key - from a sign-in that gave a
// second factor: the key then signs janusctl in alone.
func (a *app) handleSSHKeyAdd(w http.ResponseWriter, r *http.Request) {
	p, _ := principalOf(r)
	if !p.MFA {
		writeError(w, http.StatusForbidden, "adding an SSH key needs a sign-in with a second factor - set one up (your account), then sign in again")
		return
	}
	var req struct {
		Name      string    `json:"name"`
		PublicKey string    `json:"public_key"`
		Role      auth.Role `json:"role"`
		// ExpiresInDays: 0, until removed.
		ExpiresInDays int `json:"expires_in_days"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.ExpiresInDays < 0 || req.ExpiresInDays > 3650 {
		writeError(w, http.StatusBadRequest, "expires_in_days: 0 (never) to 3650")
		return
	}
	k, err := a.auth.AddSSHKey(p.User, req.Name, req.PublicKey, req.Role, time.Duration(req.ExpiresInDays)*24*time.Hour)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, k)
}

func (a *app) handleSSHKeyRemove(w http.ResponseWriter, r *http.Request) {
	p, _ := principalOf(r)
	if err := a.auth.RemoveSSHKey(p.User, r.PathValue("fingerprint")); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// challenges are the SSH sign-ins under way: a random challenge each,
// good once, for two minutes.
type challenges struct {
	mu sync.Mutex
	m  map[string]challenge
}

type challenge struct {
	user, fingerprint string
	expires           time.Time
}

const (
	challengeLife = 2 * time.Minute
	maxChallenges = 10000
)

// SSHLoginNamespace is the SSHSIG namespace of janusctl's sign-ins.
const SSHLoginNamespace = "janus-login"

// sshLoginMessage is what janusctl signs: the challenge, for the server
// whose certificate it saw - a signature relayed to another server says
// so.
func sshLoginMessage(serverFingerprint, challenge string) []byte {
	return []byte("janus-login\n" + serverFingerprint + "\n" + challenge)
}

func (c *challenges) issue(user, fingerprint string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	id := base64.RawURLEncoding.EncodeToString(raw)
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]challenge{}
	}
	for k, v := range c.m {
		if now.After(v.expires) {
			delete(c.m, k)
		}
	}
	if len(c.m) >= maxChallenges {
		return "", errors.New("too many sign-ins under way - try again in a minute")
	}
	c.m[id] = challenge{user: user, fingerprint: fingerprint, expires: now.Add(challengeLife)}
	return id, nil
}

// take is the challenge id, used up, if it's live and for user's key.
func (c *challenges) take(id, user, fingerprint string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[id]
	delete(c.m, id)
	return ok && time.Now().Before(v.expires) && v.user == user && v.fingerprint == fingerprint
}

type sshLoginRequest struct {
	User        string `json:"user"`
	Fingerprint string `json:"fingerprint"`
	Challenge   string `json:"challenge"`
	// Signature is the SSHSIG (armored) of sshLoginMessage.
	Signature string `json:"signature"`
}

// handleCLIChallenge starts an SSH sign-in: a challenge for the key -
// whether or not the account has it, so this tells nothing.
func (a *app) handleCLIChallenge(w http.ResponseWriter, r *http.Request) {
	var req sshLoginRequest
	if !decodeBody(w, r, &req) {
		return
	}
	noteAudit(r, strings.TrimSpace(req.User), "ssh key "+req.Fingerprint)
	id, err := a.challenges.issue(strings.TrimSpace(req.User), req.Fingerprint)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"challenge": id, "server_fingerprint": a.servedFingerprint()})
}

// handleCLISSHLogin ends it: the challenge signed with the account's key
// gets a certificate of the fleet for that key itself - its role, twelve
// hours - and the nodes.
func (a *app) handleCLISSHLogin(w http.ResponseWriter, r *http.Request) {
	client := clientAddr(r)
	if ok, wait := a.loginLimiter.Allow(client); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
		http.Error(w, "too many failed attempts - try again later", http.StatusTooManyRequests)
		return
	}
	var req sshLoginRequest
	if !decodeBody(w, r, &req) {
		return
	}
	user := strings.TrimSpace(req.User)
	noteAudit(r, user, "ssh key "+req.Fingerprint)
	fail := func() {
		a.loginLimiter.Fail(client)
		writeError(w, http.StatusUnauthorized, "this key doesn't sign this account in (unknown, expired, or a signature for another server)")
	}
	if !a.challenges.take(req.Challenge, user, req.Fingerprint) {
		fail()
		return
	}
	pub, account, limit, err := a.auth.SSHKeyFor(user, req.Fingerprint)
	if err != nil {
		fail()
		return
	}
	if err := sshsig.Verify([]byte(req.Signature), pub, SSHLoginNamespace, sshLoginMessage(a.servedFingerprint(), req.Challenge)); err != nil {
		fail()
		return
	}
	a.loginLimiter.Succeed(client)
	a.auth.SSHKeyUsed(user, req.Fingerprint)
	cpk, ok := pub.(ssh.CryptoPublicKey)
	if !ok || a.fleet == nil {
		writeError(w, http.StatusConflict, "set up the fleet first: janusctl's certificates are the fleet's")
		return
	}
	p := userPrincipal(account, limit)
	id, err := a.cliIdentityFor(p)
	if err != nil {
		writeError(w, http.StatusForbidden, user+" "+err.Error())
		return
	}
	chain, notAfter, err := a.fleet.IssueUser(cpk.CryptoPublicKey(), user, id.Role, id.Scope, cliSessionTTL)
	if errors.Is(err, fleet.ErrState) {
		writeError(w, http.StatusConflict, "set up the fleet first: janusctl's certificates are the fleet's")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Printf("issued a janusctl certificate to %s (%s, ssh key %s) until %s", user, id.Summary, req.Fingerprint, notAfter.Format(time.RFC3339))
	writeJSON(w, http.StatusOK, struct {
		cliCertificate
		Nodes []cliNode `json:"nodes"`
	}{cliCertificate{CertificatePEM: string(chain), ExpiresAt: notAfter, User: user, Role: id.Summary}, a.cliNodes(p)})
}
