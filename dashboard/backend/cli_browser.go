package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/auth"
	"github.com/swenske/Janus/dashboard/backend/internal/fleet"
)

// janusctl signed in through the browser: the account signs in on the
// Controller's page - its second factor and all - and approves a
// certificate for a key janusctl made, named by its fingerprint.
//
// -browser: the page answers janusctl's loopback listener with a code
// (POST /api/cli/grant), which janusctl exchanges with its CSR - the
// code is good once, for two minutes, and only for the key approved.
//
// -device (no browser where janusctl runs): janusctl posts its CSR and
// shows a short code; the account enters it on the page and approves;
// janusctl polls until it gets the certificate.

const (
	grantLife  = 2 * time.Minute
	deviceLife = 10 * time.Minute
	// devicePoll is how often janusctl asks.
	devicePoll    = 5 * time.Second
	maxCLIPending = 10000
)

// keyFingerprint is the SHA-256 of a public key's SubjectPublicKeyInfo,
// hex - what the page shows and the account approves.
func keyFingerprint(pub any) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:]), nil
}

// grant is a page's approval of janusctl's key: for what the account
// may do, limited to the role asked.
type grant struct {
	p           principal
	fingerprint string
	expires     time.Time
}

type device struct {
	userCode    string
	fingerprint string
	csr         *x509.CertificateRequest
	client      string
	created     time.Time
	// state: "pending", "approved", "denied".
	state string
	// p is the approving account, limited to the role asked.
	p principal
}

type cliLogins struct {
	mu      sync.Mutex
	grants  map[string]grant
	devices map[string]*device // by device code
}

func (c *cliLogins) sweepLocked(now time.Time) {
	for k, g := range c.grants {
		if now.After(g.expires) {
			delete(c.grants, k)
		}
	}
	for k, d := range c.devices {
		if now.After(d.created.Add(deviceLife)) {
			delete(c.devices, k)
		}
	}
}

func randomCode(n int) (string, error) {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// newUserCode is a short code to type: eight Crockford base32 characters
// (40 bits), XXXX-XXXX.
func newUserCode() (string, error) {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	var b strings.Builder
	for i, v := range raw {
		if i == 4 {
			b.WriteByte('-')
		}
		b.WriteByte("0123456789ABCDEFGHJKMNPQRSTVWXYZ"[v&31])
	}
	return b.String(), nil
}

func normalizeUserCode(s string) string {
	s = strings.NewReplacer("-", "", " ", "", "O", "0", "I", "1", "L", "1").Replace(strings.ToUpper(strings.TrimSpace(s)))
	if len(s) == 8 {
		return s[:4] + "-" + s[4:]
	}
	return s
}

func (a *app) registerCLIBrowserRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/cli/grant", a.sessionGate(anyone, anyone, a.handleCLIGrant))
	mux.HandleFunc("POST /api/cli/exchange", a.handleCLIExchange)
	mux.HandleFunc("POST /api/cli/device", a.handleCLIDevice)
	mux.HandleFunc("GET /api/cli/device/{code}", a.sessionGate(anyone, anyone, a.handleCLIDeviceShow))
	mux.HandleFunc("POST /api/cli/device/{code}/approve", a.sessionGate(anyone, anyone, a.handleCLIDeviceDecide))
	mux.HandleFunc("POST /api/cli/device/{code}/deny", a.sessionGate(anyone, anyone, a.handleCLIDeviceDecide))
	mux.HandleFunc("POST /api/cli/device/token", a.handleCLIDeviceToken)
}

// approved is what a page approves: what the account may do, or less -
// limited to the role asked.
func approved(p principal, asked auth.Role) (principal, error) {
	if asked == "" {
		return p, nil
	}
	if _, err := auth.ParseRole(string(asked)); err != nil {
		return principal{}, err
	}
	if !p.Max.AtLeast(asked) {
		return principal{}, errors.New("a certificate can't do more than its account: " + p.User + " is " + string(p.Max) + " at most")
	}
	return p.capped(asked), nil
}

// handleCLIGrant is the page approving janusctl's key: a code for
// janusctl's loopback listener.
func (a *app) handleCLIGrant(w http.ResponseWriter, r *http.Request) {
	p, _ := principalOf(r)
	var req struct {
		KeyFingerprint string    `json:"key_fingerprint"`
		Role           auth.Role `json:"role"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	fp := strings.ToLower(strings.TrimSpace(req.KeyFingerprint))
	if len(fp) != 64 {
		writeError(w, http.StatusBadRequest, "key_fingerprint: the SHA-256 of janusctl's key, hex")
		return
	}
	ap, err := approved(p, req.Role)
	if err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	code, err := randomCode(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	now := time.Now()
	a.cliLogins.mu.Lock()
	defer a.cliLogins.mu.Unlock()
	if a.cliLogins.grants == nil {
		a.cliLogins.grants = map[string]grant{}
	}
	a.cliLogins.sweepLocked(now)
	if len(a.cliLogins.grants) >= maxCLIPending {
		writeError(w, http.StatusServiceUnavailable, "too many sign-ins under way - try again in a minute")
		return
	}
	a.cliLogins.grants[code] = grant{p: ap, fingerprint: fp, expires: now.Add(grantLife)}
	writeJSON(w, http.StatusOK, map[string]string{"code": code})
}

// issueForCSR answers janusctl a certificate for csr's key - for what p
// may do, twelve hours - and the nodes.
func (a *app) issueForCSR(w http.ResponseWriter, csr *x509.CertificateRequest, p principal, how string) {
	if a.fleet == nil {
		writeError(w, http.StatusConflict, "set up the fleet first: janusctl's certificates are the fleet's")
		return
	}
	id, err := a.cliIdentityFor(p)
	if err != nil {
		writeError(w, http.StatusForbidden, p.User+" "+err.Error())
		return
	}
	user := p.User
	chain, notAfter, err := a.fleet.IssueUser(csr.PublicKey, user, id.Role, id.Scope, cliSessionTTL)
	if errors.Is(err, fleet.ErrState) {
		writeError(w, http.StatusConflict, "set up the fleet first: janusctl's certificates are the fleet's")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Printf("issued a janusctl certificate to %s (%s, %s) until %s", user, id.Summary, how, notAfter.Format(time.RFC3339))
	writeJSON(w, http.StatusOK, struct {
		cliCertificate
		Nodes []cliNode `json:"nodes"`
	}{cliCertificate{CertificatePEM: string(chain), ExpiresAt: notAfter, User: user, Role: id.Summary}, a.cliNodes(p)})
}

func (a *app) throttledCLI(w http.ResponseWriter, r *http.Request) bool {
	if ok, wait := a.loginLimiter.Allow(clientAddr(r)); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
		http.Error(w, "too many failed attempts - try again later", http.StatusTooManyRequests)
		return true
	}
	return false
}

// handleCLIExchange is janusctl's listener's code exchanged with its CSR:
// the certificate, if the CSR is for the key the page approved.
func (a *app) handleCLIExchange(w http.ResponseWriter, r *http.Request) {
	if a.throttledCLI(w, r) {
		return
	}
	var req struct {
		Code   string `json:"code"`
		CSRPEM string `json:"csr_pem"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	a.cliLogins.mu.Lock()
	g, ok := a.cliLogins.grants[req.Code]
	delete(a.cliLogins.grants, req.Code)
	a.cliLogins.mu.Unlock()
	fail := func(msg string) {
		a.loginLimiter.Fail(clientAddr(r))
		writeError(w, http.StatusUnauthorized, msg)
	}
	if !ok || time.Now().After(g.expires) {
		fail("this code isn't good (any more): janusctl login again")
		return
	}
	noteAudit(r, g.p.User, "browser")
	csr, err := parseCSR([]byte(req.CSRPEM))
	if err != nil {
		fail(err.Error())
		return
	}
	if fp, err := keyFingerprint(csr.PublicKey); err != nil || fp != g.fingerprint {
		fail("the page approved another key")
		return
	}
	a.loginLimiter.Succeed(clientAddr(r))
	a.issueForCSR(w, csr, g.p, "browser")
}

// handleCLIDevice starts a sign-in from a machine without a browser: a
// code to enter on the page, and one to poll with.
func (a *app) handleCLIDevice(w http.ResponseWriter, r *http.Request) {
	if a.throttledCLI(w, r) {
		return
	}
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
	fp, err := keyFingerprint(csr.PublicKey)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	deviceCode, err := randomCode(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	userCode, err := newUserCode()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	now := time.Now()
	a.cliLogins.mu.Lock()
	defer a.cliLogins.mu.Unlock()
	if a.cliLogins.devices == nil {
		a.cliLogins.devices = map[string]*device{}
	}
	a.cliLogins.sweepLocked(now)
	if len(a.cliLogins.devices) >= maxCLIPending {
		writeError(w, http.StatusServiceUnavailable, "too many sign-ins under way - try again in a minute")
		return
	}
	a.cliLogins.devices[deviceCode] = &device{userCode: userCode, fingerprint: fp, csr: csr, client: clientAddr(r), created: now, state: "pending"}
	writeJSON(w, http.StatusOK, map[string]any{
		"device_code":      deviceCode,
		"user_code":        userCode,
		"verification_uri": "/#/cli-device?code=" + userCode,
		"expires_in":       int(deviceLife.Seconds()),
		"interval":         int(devicePoll.Seconds()),
	})
}

// deviceByUserCode is the pending sign-in a code names. Called with the
// lock held.
func (a *app) deviceByUserCodeLocked(code string) *device {
	code = normalizeUserCode(code)
	now := time.Now()
	for _, d := range a.cliLogins.devices {
		if d.userCode == code && d.state == "pending" && now.Before(d.created.Add(deviceLife)) {
			return d
		}
	}
	return nil
}

// handleCLIDeviceShow is what the page shows before approving: the key,
// where the request came from, when.
func (a *app) handleCLIDeviceShow(w http.ResponseWriter, r *http.Request) {
	a.cliLogins.mu.Lock()
	d := a.deviceByUserCodeLocked(r.PathValue("code"))
	var out map[string]any
	if d != nil {
		out = map[string]any{"user_code": d.userCode, "key_fingerprint": d.fingerprint, "client": d.client, "created_at": d.created}
	}
	a.cliLogins.mu.Unlock()
	if out == nil {
		writeError(w, http.StatusNotFound, "no sign-in waits with this code - it's wrong, used, or ended (10 minutes)")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleCLIDeviceDecide approves (for the account, its role or a lower
// one) or denies a sign-in.
func (a *app) handleCLIDeviceDecide(w http.ResponseWriter, r *http.Request) {
	p, _ := principalOf(r)
	var req struct {
		Role auth.Role `json:"role"`
	}
	if r.ContentLength != 0 && !decodeBody(w, r, &req) {
		return
	}
	approve := strings.HasSuffix(r.URL.Path, "/approve")
	ap, err := approved(p, req.Role)
	if approve && err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	a.cliLogins.mu.Lock()
	defer a.cliLogins.mu.Unlock()
	d := a.deviceByUserCodeLocked(r.PathValue("code"))
	if d == nil {
		writeError(w, http.StatusNotFound, "no sign-in waits with this code - it's wrong, used, or ended (10 minutes)")
		return
	}
	if approve {
		d.state, d.p = "approved", ap
	} else {
		d.state = "denied"
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleCLIDeviceToken is janusctl's poll: still pending, denied, ended,
// or the certificate - once.
func (a *app) handleCLIDeviceToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceCode string `json:"device_code"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	a.cliLogins.mu.Lock()
	d, ok := a.cliLogins.devices[req.DeviceCode]
	if ok && (d.state != "pending" || time.Now().After(d.created.Add(deviceLife))) {
		delete(a.cliLogins.devices, req.DeviceCode)
	}
	a.cliLogins.mu.Unlock()
	switch {
	case !ok || time.Now().After(d.created.Add(deviceLife)):
		writeError(w, http.StatusGone, "the sign-in ended: janusctl login -device again")
	case d.state == "pending":
		writeError(w, http.StatusPreconditionRequired, "authorization_pending")
	case d.state == "denied":
		writeError(w, http.StatusForbidden, "the sign-in was denied on the Controller's page")
	default:
		noteAudit(r, d.p.User, "device")
		a.issueForCSR(w, d.csr, d.p, "device")
	}
}
