package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Trusted browsers: a sign-in that gave its second factor can ask for
// its browser to be trusted - the browser then gets a secret (a cookie)
// that stands for the second factor at its next sign-ins, until the
// policy's TrustBrowserHours run out. The password is still asked. The
// account keeps the SHA-256 of each secret only; a password change, a
// reset, a disablement (the account's Epoch moving), a second-factor
// reset or the last factor's removal forgets them all, and the account
// can forget any of them itself.

// TrustedBrowser is one browser an account trusts for its second factor.
type TrustedBrowser struct {
	ID string `json:"id"`
	// Hash is the SHA-256 of the browser's secret, hex.
	Hash string `json:"hash"`
	// Label names it for its owner ("Firefox on Linux"), Client is the
	// address it was trusted from.
	Label      string     `json:"label"`
	Client     string     `json:"client"`
	Epoch      int        `json:"epoch"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// DefaultTrustBrowserHours is how long a browser is trusted when the
// policy doesn't say: as long as a session lasts by default - signing
// out and in again, or an idle session ending, within a working day
// doesn't ask for the second factor again.
const DefaultTrustBrowserHours = 12

// maxTrustedBrowsers bounds an account's list: the oldest is forgotten.
const maxTrustedBrowsers = 20

// TrustHours is the policy's TrustBrowserHours, its default spelled out;
// 0 = browsers are never trusted.
func (p Settings) TrustHours() int {
	if p.TrustBrowserHours == nil {
		return DefaultTrustBrowserHours
	}
	return *p.TrustBrowserHours
}

// ErrTrustOff refuses trusting a browser when the policy says never.
var ErrTrustOff = errors.New("this Controller doesn't trust browsers: the second factor is asked at every sign-in")

// trustLiveLocked reports whether t still stands for u's second factor -
// unexpired, from u's current epoch, and within the current policy (a
// shorter one applies to the browsers already trusted). Called with
// s.mu held.
func (s *Store) trustLiveLocked(u *User, t TrustedBrowser, now time.Time) bool {
	hours := s.settings.TrustHours()
	return hours > 0 && t.Epoch == u.Epoch && now.Before(t.ExpiresAt) && now.Before(t.CreatedAt.Add(time.Duration(hours)*time.Hour))
}

// pruneTrustLocked drops u's browsers that no longer stand for anything,
// and the oldest beyond maxTrustedBrowsers. Called with s.mu held.
func (s *Store) pruneTrustLocked(u *User, now time.Time) {
	live := u.MFA.Trusted[:0:0]
	for _, t := range u.MFA.Trusted {
		if s.trustLiveLocked(u, t, now) {
			live = append(live, t)
		}
	}
	slices.SortFunc(live, func(a, b TrustedBrowser) int { return a.CreatedAt.Compare(b.CreatedAt) })
	if len(live) > maxTrustedBrowsers {
		live = live[len(live)-maxTrustedBrowsers:]
	}
	u.MFA.Trusted = live
}

// TrustBrowser trusts the browser of token's session - one whose
// second factor was given in this very sign-in -, returning the secret
// it presents next time (cookie value) and when it stops working.
func (s *Store) TrustBrowser(token, label, client string) (string, time.Time, error) {
	id := make([]byte, 8)
	secret := make([]byte, 32)
	if _, err := rand.Read(id); err != nil {
		return "", time.Time{}, err
	}
	if _, err := rand.Read(secret); err != nil {
		return "", time.Time{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fresh()
	ss, u, err := s.sessionLocked(token)
	if err != nil {
		return "", time.Time{}, err
	}
	if !u.MFA.Enabled() || !ss.MFA || ss.Trusted {
		return "", time.Time{}, errors.New("only a sign-in that just gave its second factor can trust its browser")
	}
	hours := s.settings.TrustHours()
	if hours == 0 {
		return "", time.Time{}, ErrTrustOff
	}
	now := s.now().UTC()
	t := TrustedBrowser{
		ID:        hex.EncodeToString(id),
		Label:     label,
		Client:    client,
		Epoch:     u.Epoch,
		CreatedAt: now,
		ExpiresAt: now.Add(time.Duration(hours) * time.Hour),
	}
	value := t.ID + "_" + base64.RawURLEncoding.EncodeToString(secret)
	t.Hash = hashTrustSecret(value)
	old := u.MFA.Trusted
	u.MFA.Trusted = append(slices.Clone(old), t)
	s.pruneTrustLocked(u, now)
	if err := s.save(); err != nil {
		u.MFA.Trusted = old
		return "", time.Time{}, err
	}
	return value, t.ExpiresAt, nil
}

func hashTrustSecret(value string) string {
	h := sha256.Sum256([]byte(value))
	return hex.EncodeToString(h[:])
}

// TrustedSignIn reports whether value (the browser's cookie) stands for
// user's second factor: then the sign-in needs none. The browser's last
// use is noted.
func (s *Store) TrustedSignIn(user, value string) bool {
	id, _, ok := strings.Cut(value, "_")
	if !ok || id == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fresh()
	u, ok := s.users[user]
	if !ok || u.Disabled || !u.MFA.Enabled() {
		return false
	}
	now := s.now().UTC()
	h := hashTrustSecret(value)
	for i, t := range u.MFA.Trusted {
		if t.ID != id || subtle.ConstantTimeCompare([]byte(t.Hash), []byte(h)) != 1 {
			continue
		}
		if !s.trustLiveLocked(u, t, now) {
			return false
		}
		u.MFA.Trusted[i].LastUsedAt = &now
		_ = s.save() // informational: never fails a sign-in
		return true
	}
	return false
}

// TrustedBrowsers are the browsers user trusts now, oldest first.
func (s *Store) TrustedBrowsers(user string) ([]TrustedBrowser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fresh()
	u, ok := s.users[user]
	if !ok {
		return nil, ErrNoUser
	}
	now := s.now()
	out := []TrustedBrowser{}
	for _, t := range u.MFA.Trusted {
		if s.trustLiveLocked(u, t, now) {
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b TrustedBrowser) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out, nil
}

// ForgetBrowser stops trusting user's browser id - every one when id is
// empty. Forgetting one that isn't there is no error: it's forgotten.
func (s *Store) ForgetBrowser(user, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fresh()
	u, ok := s.users[user]
	if !ok {
		return ErrNoUser
	}
	old := u.MFA.Trusted
	keep := []TrustedBrowser{}
	for _, t := range old {
		if id != "" && t.ID != id {
			keep = append(keep, t)
		}
	}
	if len(keep) == len(old) {
		return nil
	}
	u.MFA.Trusted = keep
	if len(keep) == 0 {
		u.MFA.Trusted = nil
	}
	if err := s.save(); err != nil {
		u.MFA.Trusted = old
		return fmt.Errorf("forget the browser: %w", err)
	}
	return nil
}
