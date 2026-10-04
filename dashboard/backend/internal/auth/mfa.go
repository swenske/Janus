package auth

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

// A second factor: an authenticator app's code (TOTP), a passkey
// (WebAuthn), and recovery codes for when neither is at hand. An account
// with a factor finishes every sign-in with one; the policy says which
// roles must have one (an admin's, by default) - such an account without
// any sets one up before anything else.

// MFA is an account's second factors.
type MFA struct {
	// TOTP is the authenticator app's secret, sealed with the master key
	// (TOTPPurpose) - users.json alone doesn't give the codes.
	TOTP        []byte     `json:"totp,omitempty"`
	TOTPAddedAt *time.Time `json:"totp_added_at,omitempty"`
	// TOTPLastStep is the time step of the last code accepted: a code is
	// good once.
	TOTPLastStep uint64 `json:"totp_last_step,omitempty"`

	// WebAuthnID is the account's random user handle for its passkeys.
	WebAuthnID []byte    `json:"webauthn_id,omitempty"`
	Passkeys   []Passkey `json:"passkeys,omitempty"`

	// RecoveryCodes are the SHA-256 of the codes left, each good once.
	RecoveryCodes []string `json:"recovery_codes,omitempty"`
}

// Passkey is one WebAuthn credential.
type Passkey struct {
	Name string `json:"name"`
	// RPID is the Controller's name it was made for: a passkey works
	// where the Controller is opened by that name only.
	RPID       string              `json:"rp_id"`
	CreatedAt  time.Time           `json:"created_at"`
	LastUsedAt *time.Time          `json:"last_used_at,omitempty"`
	Credential webauthn.Credential `json:"credential"`
}

// ID is the passkey's credential ID, hex.
func (p Passkey) ID() string { return hex.EncodeToString(p.Credential.ID) }

// Enabled reports whether the account has a second factor.
func (m MFA) Enabled() bool { return m.TOTPAddedAt != nil || len(m.Passkeys) > 0 }

// PasskeysFor are the passkeys made for rpID.
func (m MFA) PasskeysFor(rpID string) []Passkey {
	var out []Passkey
	for _, p := range m.Passkeys {
		if p.RPID == rpID {
			out = append(out, p)
		}
	}
	return out
}

// MFA policies: which accounts must have a second factor.
const (
	MFAAdmins   = "admins"
	MFAEveryone = "everyone"
	MFANobody   = "nobody"
)

// MFAPolicy is the policy, an admin's by default.
func (p Settings) MFAPolicy() string {
	if p.MFARequired == "" {
		return MFAAdmins
	}
	return p.MFARequired
}

// Requires reports whether the policy makes an account of role have a
// second factor.
func (p Settings) Requires(r Role) bool {
	switch p.MFAPolicy() {
	case MFAEveryone:
		return true
	case MFAAdmins:
		return r == Admin
	}
	return false
}

// Sealer seals and opens the TOTP secrets: the Controller's master key.
type Sealer interface {
	Seal(plaintext []byte, purpose string) ([]byte, error)
	Open(sealed []byte, purpose string) ([]byte, error)
}

// SetSealer gives the store the key it seals TOTP secrets with.
func (s *Store) SetSealer(k Sealer) {
	s.mu.Lock()
	s.sealer = k
	s.mu.Unlock()
}

func totpPurpose(user string) string { return "totp:" + user }

// mfaPendingFor is how long a sign-in has to give its second factor, and
// mfaTries how many wrong ones it gets.
const (
	mfaPendingFor = 5 * time.Minute
	mfaTries      = 5
)

var (
	// ErrSecondFactor is a wrong code or passkey.
	ErrSecondFactor = errors.New("this doesn't match: try again")
	// ErrTooManyTries ends the sign-in.
	ErrTooManyTries = errors.New("too many wrong codes: sign in again")
	// ErrFactorRequired refuses removing the last factor of an account
	// the policy makes have one.
	ErrFactorRequired = errors.New("your role needs a second factor: add another one before removing this one")
	errNoSession      = errors.New("no such session")
)

// Needs is what a session must do before anything else, in order:
// "mfa" (give its second factor), "password" (change the one someone
// else set), "mfa_enroll" (set up a second factor its role needs).
func (s *Store) Needs(u User, ss Session) []string {
	if u.MFA.Enabled() && !ss.MFA {
		return []string{"mfa"}
	}
	var out []string
	if u.MustChangePassword {
		out = append(out, "password")
	}
	if !u.MFA.Enabled() && s.Settings().Requires(u.Role) {
		out = append(out, "mfa_enroll")
	}
	return out
}

// session returns token's live session and account. Called with s.mu
// held.
func (s *Store) sessionLocked(token string) (*Session, *User, error) {
	ss, ok := s.sessions[token]
	if !ok || !s.liveLocked(ss, s.now()) {
		return nil, nil, errNoSession
	}
	return ss, s.users[ss.User], nil
}

// failLocked counts a wrong second factor against the session - ending it
// at the last try.
func (s *Store) failLocked(token string, ss *Session) error {
	ss.Failures++
	if ss.Failures >= mfaTries {
		delete(s.sessions, token)
		return ErrTooManyTries
	}
	return ErrSecondFactor
}

// passLocked marks the session's second factor given.
func passLocked(ss *Session) {
	ss.MFA, ss.Failures = true, 0
}

// VerifyTOTP finishes token's sign-in with an authenticator app's code.
func (s *Store) VerifyTOTP(token, code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fresh()
	ss, u, err := s.sessionLocked(token)
	if err != nil {
		return err
	}
	if u.MFA.TOTPAddedAt == nil {
		return errors.New("this account has no authenticator app")
	}
	if s.sealer == nil {
		return errors.New("no master key to open the secret with")
	}
	secret, err := s.sealer.Open(u.MFA.TOTP, totpPurpose(u.Name))
	if err != nil {
		return fmt.Errorf("open the account's secret: %w", err)
	}
	step, ok := checkTOTP(string(secret), code, s.now(), u.MFA.TOTPLastStep)
	if !ok {
		return s.failLocked(token, ss)
	}
	u.MFA.TOTPLastStep = step
	if err := s.save(); err != nil {
		return err
	}
	passLocked(ss)
	return nil
}

// UseRecoveryCode finishes token's sign-in with a recovery code, which is
// then used up.
func (s *Store) UseRecoveryCode(token, code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fresh()
	ss, u, err := s.sessionLocked(token)
	if err != nil {
		return err
	}
	h := hashRecoveryCode(code)
	for i, c := range u.MFA.RecoveryCodes {
		if subtle.ConstantTimeCompare([]byte(c), []byte(h)) == 1 {
			left := append(append([]string{}, u.MFA.RecoveryCodes[:i]...), u.MFA.RecoveryCodes[i+1:]...)
			old := u.MFA.RecoveryCodes
			u.MFA.RecoveryCodes = left
			if err := s.save(); err != nil {
				u.MFA.RecoveryCodes = old
				return err
			}
			passLocked(ss)
			return nil
		}
	}
	return s.failLocked(token, ss)
}

// recoveryCodes is how many an account gets: ten codes of 12 Crockford
// base32 characters (60 bits) each.
const recoveryCodes = 10

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

func newRecoveryCodes() (codes, hashes []string, err error) {
	for range recoveryCodes {
		raw := make([]byte, 12)
		if _, err := rand.Read(raw); err != nil {
			return nil, nil, err
		}
		var b strings.Builder
		for i, v := range raw {
			if i > 0 && i%4 == 0 {
				b.WriteByte('-')
			}
			b.WriteByte(crockford[v&31])
		}
		codes = append(codes, b.String())
		hashes = append(hashes, hashRecoveryCode(b.String()))
	}
	return codes, hashes, nil
}

// hashRecoveryCode reads a code as typed: any case, with or without
// dashes and spaces, O for 0 and I or L for 1.
func hashRecoveryCode(code string) string {
	code = strings.NewReplacer("-", "", " ", "", "O", "0", "I", "1", "L", "1").Replace(strings.ToUpper(code))
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

// StartTOTP gives token's session a new secret to set up - kept with the
// session until EnableTOTP, never written down before.
func (s *Store) StartTOTP(token string) (string, error) {
	secret, err := NewTOTPSecret()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ss, _, err := s.sessionLocked(token)
	if err != nil {
		return "", err
	}
	ss.PendingTOTP = secret
	return secret, nil
}

// EnableTOTP keeps the secret StartTOTP gave once a code of it checks -
// answering recovery codes, once, when it's the account's first factor.
func (s *Store) EnableTOTP(token, code string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fresh()
	ss, u, err := s.sessionLocked(token)
	if err != nil {
		return nil, err
	}
	if ss.PendingTOTP == "" {
		return nil, errors.New("start setting up the authenticator app first")
	}
	if s.sealer == nil {
		return nil, errors.New("no master key to seal the secret with")
	}
	step, ok := checkTOTP(ss.PendingTOTP, code, s.now(), 0)
	if !ok {
		return nil, s.failLocked(token, ss)
	}
	sealed, err := s.sealer.Seal([]byte(ss.PendingTOTP), totpPurpose(u.Name))
	if err != nil {
		return nil, err
	}
	return s.addFactorLocked(ss, u, func(m *MFA) {
		now := s.now().UTC()
		m.TOTP, m.TOTPAddedAt, m.TOTPLastStep = sealed, &now, step
	})
}

// addFactorLocked adds a factor with add, the session counting as having
// given it - with recovery codes for a first factor, answered once.
func (s *Store) addFactorLocked(ss *Session, u *User, add func(*MFA)) ([]string, error) {
	old := u.MFA
	old.Passkeys = append([]Passkey{}, u.MFA.Passkeys...)
	first := !u.MFA.Enabled()
	add(&u.MFA)
	var codes []string
	if first {
		var hashes []string
		var err error
		if codes, hashes, err = newRecoveryCodes(); err != nil {
			u.MFA = old
			return nil, err
		}
		u.MFA.RecoveryCodes = hashes
	}
	if err := s.save(); err != nil {
		u.MFA = old
		return nil, err
	}
	ss.PendingTOTP = ""
	passLocked(ss)
	return codes, nil
}

// webAuthnUser is an account as go-webauthn sees it: its passkeys for
// one name of the Controller.
type webAuthnUser struct {
	u    *User
	rpID string
}

func (w webAuthnUser) WebAuthnID() []byte          { return w.u.MFA.WebAuthnID }
func (w webAuthnUser) WebAuthnName() string        { return w.u.Name }
func (w webAuthnUser) WebAuthnDisplayName() string { return w.u.Name }
func (w webAuthnUser) WebAuthnCredentials() []webauthn.Credential {
	var out []webauthn.Credential
	for _, p := range w.u.MFA.PasskeysFor(w.rpID) {
		out = append(out, p.Credential)
	}
	return out
}

// Ceremony is the Controller as a WebAuthn relying party, as opened by
// one name (rpID): passkeys are made and used for that name only.
type Ceremony struct {
	wa   *webauthn.WebAuthn
	rpID string
}

// NewCeremony is the relying party for the Controller opened as
// https://host (host with its port, if any; rpID: without).
func NewCeremony(host, rpID string) (Ceremony, error) {
	wa, err := webauthn.New(&webauthn.Config{RPID: rpID, RPDisplayName: "Janus Controller", RPOrigins: []string{"https://" + host}})
	if err != nil {
		return Ceremony{}, err
	}
	return Ceremony{wa, rpID}, nil
}

// RPID is the name the passkeys are for.
func (c Ceremony) RPID() string { return c.rpID }

// BeginPasskey starts registering a passkey named name: the options for
// navigator.credentials.create.
func (s *Store) BeginPasskey(token string, c Ceremony, name string) (any, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 60 {
		return nil, errors.New("name the passkey (1 to 60 characters): what holds it")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fresh()
	ss, u, err := s.sessionLocked(token)
	if err != nil {
		return nil, err
	}
	if u.MFA.WebAuthnID == nil {
		id := make([]byte, 32)
		if _, err := rand.Read(id); err != nil {
			return nil, err
		}
		u.MFA.WebAuthnID = id
		if err := s.save(); err != nil {
			u.MFA.WebAuthnID = nil
			return nil, err
		}
	}
	wu := webAuthnUser{u, c.rpID}
	var exclude []webauthn.Credential
	for _, p := range u.MFA.Passkeys {
		exclude = append(exclude, p.Credential)
	}
	opts, data, err := c.wa.BeginRegistration(wu, webauthn.WithExclusions(webauthn.Credentials(exclude).CredentialDescriptors()))
	if err != nil {
		return nil, err
	}
	ss.WebAuthn, ss.PasskeyName = data, name
	return opts, nil
}

// FinishPasskey ends registering a passkey with the browser's answer -
// answering recovery codes when it's the account's first factor.
func (s *Store) FinishPasskey(token string, c Ceremony, answer *protocol.ParsedCredentialCreationData) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fresh()
	ss, u, err := s.sessionLocked(token)
	if err != nil {
		return nil, err
	}
	if ss.WebAuthn == nil || ss.PasskeyName == "" {
		return nil, errors.New("start registering the passkey first")
	}
	data, name := *ss.WebAuthn, ss.PasskeyName
	ss.WebAuthn, ss.PasskeyName = nil, ""
	cred, err := c.wa.CreateCredential(webAuthnUser{u, c.rpID}, data, answer)
	if err != nil {
		return nil, fmt.Errorf("the passkey wasn't made: %w", err)
	}
	for _, p := range u.MFA.Passkeys {
		if bytes.Equal(p.Credential.ID, cred.ID) {
			return nil, errors.New("this passkey is already the account's")
		}
	}
	return s.addFactorLocked(ss, u, func(m *MFA) {
		m.Passkeys = append(m.Passkeys, Passkey{Name: name, RPID: c.rpID, CreatedAt: s.now().UTC(), Credential: *cred})
	})
}

// BeginPasskeyLogin starts finishing token's sign-in with a passkey: the
// options for navigator.credentials.get.
func (s *Store) BeginPasskeyLogin(token string, c Ceremony) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fresh()
	ss, u, err := s.sessionLocked(token)
	if err != nil {
		return nil, err
	}
	if len(u.MFA.PasskeysFor(c.rpID)) == 0 {
		return nil, fmt.Errorf("no passkey of this account works for %s", c.rpID)
	}
	opts, data, err := c.wa.BeginLogin(webAuthnUser{u, c.rpID})
	if err != nil {
		return nil, err
	}
	ss.WebAuthn = data
	return opts, nil
}

// FinishPasskeyLogin ends it with the browser's answer.
func (s *Store) FinishPasskeyLogin(token string, c Ceremony, answer *protocol.ParsedCredentialAssertionData) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fresh()
	ss, u, err := s.sessionLocked(token)
	if err != nil {
		return err
	}
	if ss.WebAuthn == nil {
		return errors.New("start with the passkey first")
	}
	data := *ss.WebAuthn
	ss.WebAuthn = nil
	cred, err := c.wa.ValidateLogin(webAuthnUser{u, c.rpID}, data, answer)
	if err != nil {
		if e := s.failLocked(token, ss); errors.Is(e, ErrTooManyTries) {
			return e
		}
		return fmt.Errorf("the passkey didn't check: %w", err)
	}
	for i, p := range u.MFA.Passkeys {
		if bytes.Equal(p.Credential.ID, cred.ID) {
			now := s.now().UTC()
			u.MFA.Passkeys[i].Credential.Authenticator = cred.Authenticator
			u.MFA.Passkeys[i].Credential.Flags = cred.Flags
			u.MFA.Passkeys[i].LastUsedAt = &now
		}
	}
	if err := s.save(); err != nil {
		return err
	}
	passLocked(ss)
	return nil
}

// RemoveTOTP and RemovePasskey take a factor away, with the account's
// password - never the last one the policy makes the account have.
func (s *Store) RemoveTOTP(name, password string) error {
	return s.removeFactor(name, password, func(m *MFA) bool {
		if m.TOTPAddedAt == nil {
			return false
		}
		m.TOTP, m.TOTPAddedAt, m.TOTPLastStep = nil, nil, 0
		return true
	})
}

func (s *Store) RemovePasskey(name, password, id string) error {
	return s.removeFactor(name, password, func(m *MFA) bool {
		for i, p := range m.Passkeys {
			if p.ID() == id {
				m.Passkeys = append(m.Passkeys[:i:i], m.Passkeys[i+1:]...)
				return true
			}
		}
		return false
	})
}

func (s *Store) removeFactor(name, password string, remove func(*MFA) bool) error {
	if _, err := s.Authenticate(name, password); err != nil {
		return errors.New("the password is wrong")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[name]
	if !ok {
		return ErrNoUser
	}
	old := u.MFA
	old.Passkeys = append([]Passkey{}, u.MFA.Passkeys...)
	if !remove(&u.MFA) {
		return errors.New("no such factor")
	}
	if !u.MFA.Enabled() {
		if s.settings.Requires(u.Role) {
			u.MFA = old
			return ErrFactorRequired
		}
		u.MFA.RecoveryCodes = nil
	}
	if err := s.save(); err != nil {
		u.MFA = old
		return err
	}
	return nil
}

// NewRecoveryCodes replaces the account's recovery codes, with its
// password: the new ones are answered once.
func (s *Store) NewRecoveryCodes(name, password string) ([]string, error) {
	if _, err := s.Authenticate(name, password); err != nil {
		return nil, errors.New("the password is wrong")
	}
	codes, hashes, err := newRecoveryCodes()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[name]
	if !ok {
		return nil, ErrNoUser
	}
	if !u.MFA.Enabled() {
		return nil, errors.New("set up a second factor first")
	}
	old := u.MFA.RecoveryCodes
	u.MFA.RecoveryCodes = hashes
	if err := s.save(); err != nil {
		u.MFA.RecoveryCodes = old
		return nil, err
	}
	return codes, nil
}

// ResetMFA takes every second factor away from an account - an admin's
// help for a lost phone or key; its sessions end.
func (s *Store) ResetMFA(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fresh()
	u, ok := s.users[name]
	if !ok {
		return ErrNoUser
	}
	old := *u
	u.MFA = MFA{}
	u.Epoch++
	if err := s.save(); err != nil {
		*u = old
		return err
	}
	return nil
}
