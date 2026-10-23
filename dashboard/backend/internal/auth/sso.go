package auth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"
)

// Single sign-on (dashboard/backend/internal/sso): an account signs in
// through an OpenID Connect provider by an identity bound to it - the
// provider's issuer and the subject it gives the person, never a name or
// an e-mail address. Two kinds of accounts have one:
//
//   - an account the provider made at its first sign-in (SSO set): no
//     password; its role and grants are what the provider's rules give
//     its groups, set again at every sign-in and whenever the rules
//     change; it lapses - sessions, API tokens, SSH keys - some days
//     after its last sign-in through the provider, until the next;
//   - an account with a password whose owner linked an identity to it:
//     its role stays its own, the provider is one more way in.

// MaxIdentities is the most identities an account has.
const MaxIdentities = 8

// Identity is who a single sign-on provider says an account is.
type Identity struct {
	ID string `json:"id"`
	// Provider is the provider's ID in the Controller's settings; Issuer
	// and Subject are what bind the identity.
	Provider string `json:"provider"`
	Issuer   string `json:"issuer"`
	Subject  string `json:"subject"`
	// Username is the provider's name for it, at its last sign-in.
	Username   string     `json:"username"`
	LinkedAt   time.Time  `json:"linked_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// SSOAccount marks an account a provider made.
type SSOAccount struct {
	Provider     string    `json:"provider"`
	LastSignInAt time.Time `json:"last_sign_in_at"`
	// ValidUntil is when the account lapses unless it signs in through
	// the provider again (nil: never).
	ValidUntil *time.Time `json:"valid_until,omitempty"`
	// Groups are the provider's at the last sign-in: what the rules are
	// applied to again when they change.
	Groups []string `json:"groups,omitempty"`
}

// SSOAccess is what a provider's rules give an identity.
type SSOAccess struct {
	// Role is over everything (none: only Grants).
	Role   Role
	Grants []Grant
	// Matched: a rule matched, or the provider has a default role -
	// otherwise the identity gets nothing.
	Matched bool
}

var (
	// ErrNoAccess: the provider's rules give the identity nothing.
	ErrNoAccess = errors.New("the provider's rules give you no access to this Controller")
	// ErrNameTaken: the provider's name for the identity is another
	// account's.
	ErrNameTaken = errors.New("an account of this name already exists: its owner signs in with its password, then links the identity to it")
	// ErrNameInvalid: the provider's name isn't a name an account may
	// have.
	ErrNameInvalid = errors.New("the provider's name for you isn't one an account can have (1 to 64 lowercase letters, digits and . _ @ -)")
	// ErrIdentityTaken: the identity is bound to an account already.
	ErrIdentityTaken = errors.New("this identity is linked to an account already")
	// ErrManaged refuses what a provider decides for an account it made.
	ErrManaged = errors.New("a single sign-on provider manages this account")
	// ErrDisabled: the account is disabled.
	ErrDisabled = errors.New("this account is disabled")
	errNotSetUp = errors.New("the Controller isn't set up yet")
)

// Lapsed reports whether u, an account a provider made, needs to sign
// in through it again before anything else.
func (u User) Lapsed(now time.Time) bool {
	return u.SSO != nil && u.SSO.ValidUntil != nil && !now.Before(*u.SSO.ValidUntil)
}

// SameGrant reports whether a and b give the same thing.
func SameGrant(a, b Grant) bool {
	if a.Role != b.Role || len(a.Selector) != len(b.Selector) || !slices.Equal(a.Domains, b.Domains) {
		return false
	}
	for k, v := range a.Selector {
		if w, ok := b.Selector[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// demoted reports whether going from role and grants to next's takes
// anything away.
func demoted(role Role, grants []Grant, next SSOAccess) bool {
	if roleRank[next.Role] < roleRank[role] {
		return true
	}
	for _, g := range grants {
		if !slices.ContainsFunc(next.Grants, func(h Grant) bool { return SameGrant(g, h) }) {
			return true
		}
	}
	return false
}

// boundLocked is the account id (issuer, subject) is bound to, and the
// identity's index. Called with s.mu held.
func (s *Store) boundLocked(issuer, subject string) (*User, int) {
	for _, u := range s.users {
		for i, id := range u.Identities {
			if id.Issuer == issuer && id.Subject == subject {
				return u, i
			}
		}
	}
	return nil, -1
}

func validUntil(at time.Time, validFor time.Duration) *time.Time {
	if validFor <= 0 {
		return nil
	}
	t := at.Add(validFor)
	return &t
}

// applyLocked gives an account a provider made what access says - none
// when nothing matched -, a step down ending its sessions. Called with
// s.mu held.
func applyLocked(u *User, groups []string, access SSOAccess) {
	if !access.Matched {
		access = SSOAccess{}
	}
	if demoted(u.Role, u.Grants, access) {
		u.Epoch++
	}
	u.Role, u.Grants = access.Role, slices.Clone(access.Grants)
	u.SSO.Groups = slices.Clone(groups)
}

// SSOSignIn is id signing in through its provider, whose rules give its
// groups access: the account it's bound to - an account the provider
// made getting that access, ValidFor more of life (0: unlimited) -, or a
// new one named after it. Refused when nothing matched (an account the
// provider made then loses its role and grants), for a disabled account,
// for a name another account has.
func (s *Store) SSOSignIn(id Identity, groups []string, access SSOAccess, validFor time.Duration) (User, error) {
	if access.Matched {
		if _, err := ParseAccountRole(string(access.Role)); err != nil {
			return User{}, err
		}
		if err := CheckGrants(access.Grants); err != nil {
			return User{}, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fresh()
	now := s.now().UTC()
	u, i := s.boundLocked(id.Issuer, id.Subject)
	if u == nil {
		return s.createSSOLocked(id, groups, access, validFor, now)
	}
	if u.Disabled {
		return User{}, ErrDisabled
	}
	old := cloneUser(u)
	if u.SSO != nil {
		u.SSO.Provider = id.Provider
		applyLocked(u, groups, access)
		if access.Matched {
			u.SSO.LastSignInAt, u.SSO.ValidUntil = now, validUntil(now, validFor)
		}
	} else if !access.Matched {
		return User{}, ErrNoAccess
	}
	if access.Matched {
		u.Identities[i].Username, u.Identities[i].LastUsedAt = id.Username, &now
		u.LastLoginAt = &now
	}
	if err := s.save(); err != nil {
		*u = old
		return User{}, err
	}
	if !access.Matched {
		return User{}, ErrNoAccess
	}
	return view(u), nil
}

func (s *Store) createSSOLocked(id Identity, groups []string, access SSOAccess, validFor time.Duration, now time.Time) (User, error) {
	if !access.Matched {
		return User{}, ErrNoAccess
	}
	if len(s.users) == 0 {
		return User{}, errNotSetUp
	}
	name := id.Username
	if checkName(name) != nil {
		return User{}, ErrNameInvalid
	}
	if _, ok := s.users[name]; ok {
		return User{}, ErrNameTaken
	}
	id.ID, id.LinkedAt, id.LastUsedAt = newIdentityID(), now, &now
	u := &User{
		Name: name, Role: access.Role, Grants: slices.Clone(access.Grants), CreatedAt: now, LastLoginAt: &now,
		Identities: []Identity{id},
		SSO:        &SSOAccount{Provider: id.Provider, LastSignInAt: now, ValidUntil: validUntil(now, validFor), Groups: slices.Clone(groups)},
	}
	s.users[name] = u
	if err := s.save(); err != nil {
		delete(s.users, name)
		return User{}, err
	}
	return view(u), nil
}

func newIdentityID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on Linux
	}
	return hex.EncodeToString(b)
}

// cloneUser is a copy of u sharing none of its slices: what a failed
// save puts back.
func cloneUser(u *User) User {
	c := *u
	c.Grants = slices.Clone(u.Grants)
	c.Identities = slices.Clone(u.Identities)
	if u.SSO != nil {
		sso := *u.SSO
		sso.Groups = slices.Clone(u.SSO.Groups)
		c.SSO = &sso
	}
	return c
}

// LinkIdentity binds id to user's account - one with a password, signing
// in by it: the provider becomes one more way in.
func (s *Store) LinkIdentity(user string, id Identity) (Identity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fresh()
	u, ok := s.users[user]
	switch {
	case !ok:
		return Identity{}, ErrNoUser
	case u.Disabled:
		return Identity{}, ErrDisabled
	case u.SSO != nil || len(u.PasswordHash) == 0:
		return Identity{}, fmt.Errorf("%w: it signs in through it already", ErrManaged)
	case len(u.Identities) >= MaxIdentities:
		return Identity{}, fmt.Errorf("%d identities at most", MaxIdentities)
	}
	if other, _ := s.boundLocked(id.Issuer, id.Subject); other != nil {
		return Identity{}, ErrIdentityTaken
	}
	now := s.now().UTC()
	id.ID, id.LinkedAt, id.LastUsedAt = newIdentityID(), now, nil
	old := u.Identities
	u.Identities = append(slices.Clone(old), id)
	if err := s.save(); err != nil {
		u.Identities = old
		return Identity{}, err
	}
	return id, nil
}

// UnlinkIdentity forgets one of the identities linked to user's account
// - never the one of an account a provider made (disable it instead).
func (s *Store) UnlinkIdentity(user, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fresh()
	u, ok := s.users[user]
	if !ok {
		return ErrNoUser
	}
	if u.SSO != nil {
		return fmt.Errorf("%w: its identity is how it signs in - disable the account instead", ErrManaged)
	}
	i := slices.IndexFunc(u.Identities, func(x Identity) bool { return x.ID == id })
	if i < 0 {
		return errors.New("no such identity")
	}
	old := u.Identities
	u.Identities = slices.Delete(slices.Clone(old), i, i+1)
	if len(u.Identities) == 0 {
		u.Identities = nil
	}
	if err := s.save(); err != nil {
		u.Identities = old
		return err
	}
	return nil
}

// ResyncSSO applies provider's rules (access) again to the accounts it
// made, from the groups of their last sign-in, and their life (validFor)
// from that sign-in: a change of the rules takes effect at once, a step
// down ending the account's sessions. It answers how many changed.
func (s *Store) ResyncSSO(provider string, access func(groups []string) SSOAccess, validFor time.Duration) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fresh()
	var olds []User
	changed := 0
	for _, u := range s.users {
		if u.SSO == nil || u.SSO.Provider != provider {
			continue
		}
		old := cloneUser(u)
		a := access(u.SSO.Groups)
		if CheckGrants(a.Grants) != nil {
			a = SSOAccess{}
		}
		applyLocked(u, u.SSO.Groups, a)
		u.SSO.ValidUntil = validUntil(u.SSO.LastSignInAt, validFor)
		if u.Role != old.Role || u.Epoch != old.Epoch || !slices.EqualFunc(u.Grants, old.Grants, SameGrant) || !sameTime(u.SSO.ValidUntil, old.SSO.ValidUntil) {
			changed++
			olds = append(olds, old)
		}
	}
	if changed == 0 {
		return 0, nil
	}
	if err := s.save(); err != nil {
		for _, old := range olds {
			*s.users[old.Name] = old
		}
		return 0, err
	}
	return changed, nil
}

func sameTime(a, b *time.Time) bool {
	return (a == nil) == (b == nil) && (a == nil || a.Equal(*b))
}

// LapseProvider makes the accounts provider made lapse now: the provider
// is gone, they can't sign in through it again.
func (s *Store) LapseProvider(provider string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fresh()
	now := s.now().UTC()
	var olds []User
	for _, u := range s.users {
		if u.SSO == nil || u.SSO.Provider != provider || u.Lapsed(now) {
			continue
		}
		olds = append(olds, cloneUser(u))
		u.SSO.ValidUntil = &now
	}
	if len(olds) == 0 {
		return nil
	}
	if err := s.save(); err != nil {
		for _, old := range olds {
			*s.users[old.Name] = old
		}
		return err
	}
	return nil
}

// EndProviderSessions ends every session signed in through provider.
func (s *Store) EndProviderSessions(provider string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for t, ss := range s.sessions {
		if ss.Provider == provider {
			delete(s.sessions, t)
		}
	}
}

// SSOSession is how a sign-in through a provider starts a session.
type SSOSession struct {
	Provider string
	// IDToken is the provider's, kept for signing out of it too.
	IDToken string
	// ProviderMFA: the provider's second factor counts.
	ProviderMFA bool
	// Trusted: the browser is trusted for the Controller's second factor.
	Trusted bool
}

// NewSSOSession signs user in through a provider.
func (s *Store) NewSSOSession(user string, o SSOSession) (string, error) {
	return s.newSession(Session{
		User: user, Provider: o.Provider, IDToken: o.IDToken, ProviderMFA: o.ProviderMFA,
		MFA: o.ProviderMFA || o.Trusted, Trusted: o.Trusted && !o.ProviderMFA,
	})
}

// RenewSession replaces the session token was - ended by a password
// change - with a new one of the same sign-in: its second factor, its
// provider.
func (s *Store) RenewSession(token string) (string, error) {
	s.mu.Lock()
	ss, ok := s.sessions[token]
	var next Session
	if ok {
		next = Session{User: ss.User, MFA: ss.MFA, Trusted: ss.Trusted, FactorAt: ss.FactorAt, Provider: ss.Provider, IDToken: ss.IDToken, ProviderMFA: ss.ProviderMFA}
		delete(s.sessions, token)
	}
	s.mu.Unlock()
	if !ok {
		return "", errNoSession
	}
	return s.newSession(next)
}
