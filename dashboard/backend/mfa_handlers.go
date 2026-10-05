package main

import (
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"rsc.io/qr"

	"github.com/swenske/Janus/dashboard/backend/internal/auth"
)

// --- second factors: /api/auth/mfa/* ---
//
// Two kinds of calls, both from the account's own session: those that
// finish a sign-in waiting for its second factor (a code, a recovery
// code, a passkey), and those that set the account's factors up - from a
// session that gave its second factor, or whose account has none yet.

type passkeyView struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	RPID       string     `json:"rp_id"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	// Here: made for the name the page is opened by - usable here.
	Here bool `json:"here"`
}

type mfaView struct {
	TOTP          bool          `json:"totp"`
	Passkeys      []passkeyView `json:"passkeys"`
	RecoveryCodes int           `json:"recovery_codes"`
	// Required: the account's role must have a second factor.
	Required bool `json:"required"`
	// RPID is the name passkeys are made and used for here - empty when
	// the page is opened by an IP address, where browsers refuse them.
	RPID string `json:"rp_id"`
}

func (a *app) viewMFA(r *http.Request, u auth.User) mfaView {
	_, rp, _ := relyingParty(r)
	v := mfaView{TOTP: u.MFA.TOTPAddedAt != nil, Passkeys: []passkeyView{}, RecoveryCodes: len(u.MFA.RecoveryCodes), Required: a.auth.Settings().Requires(u.MaxRole()), RPID: rp}
	for _, p := range u.MFA.Passkeys {
		v.Passkeys = append(v.Passkeys, passkeyView{ID: p.ID(), Name: p.Name, RPID: p.RPID, CreatedAt: p.CreatedAt, LastUsedAt: p.LastUsedAt, Here: p.RPID == rp && rp != ""})
	}
	return v
}

// relyingParty is the page's origin host (with its port) and the name
// passkeys are for there - none for an IP address.
func relyingParty(r *http.Request) (host, rpID string, ok bool) {
	host = strings.ToLower(r.Host)
	name := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		name = h
	}
	name = strings.TrimSuffix(name, ".")
	if name == "" || net.ParseIP(strings.Trim(name, "[]")) != nil {
		return host, "", false
	}
	return host, name, true
}

func ceremony(r *http.Request) (auth.Ceremony, error) {
	host, rp, ok := relyingParty(r)
	if !ok {
		return auth.Ceremony{}, errors.New("passkeys need the Controller opened by its name, not an IP address")
	}
	return auth.NewCeremony(host, rp)
}

func (a *app) registerMFARoutes(mux *http.ServeMux) {
	// Finishing a sign-in.
	mux.HandleFunc("POST /api/auth/mfa/totp", a.handleMFACode)
	mux.HandleFunc("POST /api/auth/mfa/recovery", a.handleMFACode)
	mux.HandleFunc("POST /api/auth/mfa/passkey/begin", a.handleMFAPasskeyBegin)
	mux.HandleFunc("POST /api/auth/mfa/passkey/finish", a.handleMFAPasskeyFinish)
	// Setting the account's factors up.
	mux.HandleFunc("POST /api/auth/mfa/totp/setup", a.handleTOTPSetup)
	mux.HandleFunc("POST /api/auth/mfa/totp/enable", a.handleTOTPEnable)
	mux.HandleFunc("DELETE /api/auth/mfa/totp", a.handleTOTPRemove)
	mux.HandleFunc("POST /api/auth/mfa/passkeys/begin", a.handlePasskeyBegin)
	mux.HandleFunc("POST /api/auth/mfa/passkeys/finish", a.handlePasskeyFinish)
	mux.HandleFunc("DELETE /api/auth/mfa/passkeys/{id}", a.handlePasskeyRemove)
	mux.HandleFunc("POST /api/auth/mfa/recovery-codes", a.handleRecoveryCodes)
}

// mfaSession is the request's session for a second-factor call: signIn,
// one finishing a sign-in that waits for its second factor; else one
// setting factors up, refused to a sign-in still waiting.
func (a *app) mfaSession(w http.ResponseWriter, r *http.Request, signIn bool) (string, auth.User, bool) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return "", auth.User{}, false
	}
	u, ss, ok := a.auth.Session(c.Value, true)
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return "", auth.User{}, false
	}
	noteAudit(r, u.Name, "session")
	waiting := slices.Contains(a.auth.Needs(u, ss), "mfa")
	switch {
	case signIn && !waiting:
		writeError(w, http.StatusConflict, "this sign-in needs no second factor now")
		return "", auth.User{}, false
	case !signIn && waiting:
		writeError(w, http.StatusForbidden, needsMessage["mfa"])
		return "", auth.User{}, false
	}
	return c.Value, u, true
}

// throttled answers 429 when the client's address is locked out - wrong
// second factors count like wrong passwords.
func (a *app) throttled(w http.ResponseWriter, r *http.Request) bool {
	if ok, wait := a.loginLimiter.Allow(clientAddr(r)); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
		http.Error(w, "too many failed attempts - try again later", http.StatusTooManyRequests)
		return true
	}
	return false
}

// factorFailed answers a wrong second factor: 400 to try again, 401 once
// the sign-in is over.
func (a *app) factorFailed(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, auth.ErrSecondFactor) || errors.Is(err, auth.ErrTooManyTries) {
		a.loginLimiter.Fail(clientAddr(r))
	}
	if errors.Is(err, auth.ErrTooManyTries) {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	writeError(w, http.StatusBadRequest, err.Error())
}

// handleMFACode finishes a sign-in with an authenticator app's code
// (/totp) or a recovery code (/recovery).
func (a *app) handleMFACode(w http.ResponseWriter, r *http.Request) {
	token, _, ok := a.mfaSession(w, r, true)
	if !ok || a.throttled(w, r) {
		return
	}
	var req struct {
		Code string `json:"code"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	check := a.auth.VerifyTOTP
	if strings.HasSuffix(r.URL.Path, "/recovery") {
		check = a.auth.UseRecoveryCode
	}
	if err := check(token, req.Code); err != nil {
		a.factorFailed(w, r, err)
		return
	}
	a.loginLimiter.Succeed(clientAddr(r))
	w.WriteHeader(http.StatusNoContent)
}

func (a *app) handleMFAPasskeyBegin(w http.ResponseWriter, r *http.Request) {
	token, _, ok := a.mfaSession(w, r, true)
	if !ok {
		return
	}
	c, err := ceremony(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	opts, err := a.auth.BeginPasskeyLogin(token, c)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, opts)
}

func (a *app) handleMFAPasskeyFinish(w http.ResponseWriter, r *http.Request) {
	token, _, ok := a.mfaSession(w, r, true)
	if !ok || a.throttled(w, r) {
		return
	}
	c, err := ceremony(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	answer, err := protocol.ParseCredentialRequestResponseBody(http.MaxBytesReader(w, r.Body, 1<<16))
	if err != nil {
		writeError(w, http.StatusBadRequest, "the browser's answer: "+protocolError(err))
		return
	}
	if err := a.auth.FinishPasskeyLogin(token, c, answer); err != nil {
		a.factorFailed(w, r, err)
		return
	}
	a.loginLimiter.Succeed(clientAddr(r))
	w.WriteHeader(http.StatusNoContent)
}

// protocolError is go-webauthn's error with its details.
func protocolError(err error) string {
	var pe *protocol.Error
	if errors.As(err, &pe) && pe.DevInfo != "" {
		return pe.Details + ": " + pe.DevInfo
	}
	return err.Error()
}

// handleTOTPSetup starts setting an authenticator app up: the secret, as
// text and as the QR code the app reads.
func (a *app) handleTOTPSetup(w http.ResponseWriter, r *http.Request) {
	token, u, ok := a.mfaSession(w, r, false)
	if !ok {
		return
	}
	if u.MFA.TOTPAddedAt != nil {
		writeError(w, http.StatusConflict, "this account has an authenticator app: remove it first to set up another")
		return
	}
	secret, err := a.auth.StartTOTP(token)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	host, _, _ := relyingParty(r)
	account := u.Name
	if h, _, err := net.SplitHostPort(host); err == nil {
		account += "@" + h
	} else if host != "" {
		account += "@" + host
	}
	uri := auth.TOTPURI("Janus Controller", account, secret)
	svg, err := qrSVG(uri)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"secret": secret, "uri": uri, "qr_svg": svg})
}

type recoveryCodesView struct {
	// RecoveryCodes are shown this once: an account's first factor comes
	// with them, and a new set replaces the old one.
	RecoveryCodes []string `json:"recovery_codes"`
}

func (a *app) handleTOTPEnable(w http.ResponseWriter, r *http.Request) {
	token, _, ok := a.mfaSession(w, r, false)
	if !ok {
		return
	}
	var req struct {
		Code string `json:"code"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	codes, err := a.auth.EnableTOTP(token, req.Code)
	if err != nil {
		a.factorFailed(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, recoveryCodesView{nonNil(codes)})
}

type withPassword struct {
	Password string `json:"password"`
}

func (a *app) handleTOTPRemove(w http.ResponseWriter, r *http.Request) {
	_, u, ok := a.mfaSession(w, r, false)
	if !ok || a.throttled(w, r) {
		return
	}
	var req withPassword
	if !decodeBody(w, r, &req) {
		return
	}
	a.answerRemoval(w, r, a.auth.RemoveTOTP(u.Name, req.Password))
}

func (a *app) answerRemoval(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, auth.ErrFactorRequired):
		writeError(w, http.StatusConflict, err.Error())
	default:
		if strings.Contains(err.Error(), "password") {
			a.loginLimiter.Fail(clientAddr(r))
		}
		writeError(w, http.StatusBadRequest, err.Error())
	}
}

func (a *app) handlePasskeyBegin(w http.ResponseWriter, r *http.Request) {
	token, _, ok := a.mfaSession(w, r, false)
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	c, err := ceremony(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	opts, err := a.auth.BeginPasskey(token, c, req.Name)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, opts)
}

func (a *app) handlePasskeyFinish(w http.ResponseWriter, r *http.Request) {
	token, _, ok := a.mfaSession(w, r, false)
	if !ok {
		return
	}
	c, err := ceremony(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	answer, err := protocol.ParseCredentialCreationResponseBody(http.MaxBytesReader(w, r.Body, 1<<16))
	if err != nil {
		writeError(w, http.StatusBadRequest, "the browser's answer: "+protocolError(err))
		return
	}
	codes, err := a.auth.FinishPasskey(token, c, answer)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, recoveryCodesView{nonNil(codes)})
}

func (a *app) handlePasskeyRemove(w http.ResponseWriter, r *http.Request) {
	_, u, ok := a.mfaSession(w, r, false)
	if !ok || a.throttled(w, r) {
		return
	}
	var req withPassword
	if !decodeBody(w, r, &req) {
		return
	}
	a.answerRemoval(w, r, a.auth.RemovePasskey(u.Name, req.Password, r.PathValue("id")))
}

func (a *app) handleRecoveryCodes(w http.ResponseWriter, r *http.Request) {
	_, u, ok := a.mfaSession(w, r, false)
	if !ok || a.throttled(w, r) {
		return
	}
	var req withPassword
	if !decodeBody(w, r, &req) {
		return
	}
	codes, err := a.auth.NewRecoveryCodes(u.Name, req.Password)
	if err != nil {
		if strings.Contains(err.Error(), "password") {
			a.loginLimiter.Fail(clientAddr(r))
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, recoveryCodesView{codes})
}

// qrSVG draws text's QR code as an SVG - the Controller's page shows it
// to an authenticator app's camera; nothing leaves the Controller.
func qrSVG(text string) (string, error) {
	c, err := qr.Encode(text, qr.M)
	if err != nil {
		return "", err
	}
	const quiet = 4
	n := c.Size + 2*quiet
	var path strings.Builder
	for y := 0; y < c.Size; y++ {
		for x := 0; x < c.Size; x++ {
			if !c.Black(x, y) {
				continue
			}
			run := 1
			for x+run < c.Size && c.Black(x+run, y) {
				run++
			}
			fmt.Fprintf(&path, "M%d %dh%dv1h-%dz", x+quiet, y+quiet, run, run)
			x += run
		}
	}
	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges"><rect width="%d" height="%d" fill="#fff"/><path d="%s" fill="#000"/></svg>`, n, n, n, n, path.String()), nil
}
