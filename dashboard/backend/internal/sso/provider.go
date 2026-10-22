// Package sso is the Controller's single sign-on: OpenID Connect
// providers - a Keycloak realm, a Synology's SSO Server, any other - that
// accounts sign in through instead of a password.
//
// A provider's rules turn the groups its ID token names into a role (and
// grants) on the Controller: the highest role a rule gives wins, grants
// add up, and nobody a rule doesn't match gets in (unless the provider
// has a default role). An identity is bound to one account by its issuer
// and subject - never by a name or an e-mail address someone could
// change at the provider.
//
// The settings are <data-dir>/sso.json (0600), the client secrets sealed
// with the master key. The sign-in itself is the authorization code flow
// with PKCE (S256), a state and a nonce; the ID token is checked by
// go-oidc (issuer, audience, expiry, signature by a key of the
// provider's set, asymmetric algorithms only) and here (nonce, azp, iat,
// at_hash).
package sso

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/swenske/Janus/dashboard/backend/internal/auth"
	"github.com/swenske/Janus/dashboard/backend/internal/labels"
	"github.com/swenske/Janus/internal/tlstrust"
)

// MFA modes: who asks for the second factor of a sign-in through a
// provider.
const (
	// MFAJanus: the Controller, as for a password - the provider only
	// stands for the password.
	MFAJanus = "janus"
	// MFAProvider: the provider did - trusted as is, or when its ID
	// token carries one of ACR or AMR; otherwise the Controller asks.
	MFAProvider = "provider"
)

// Presets fill a new provider's claims and scopes for a kind of server.
const (
	PresetKeycloak = "keycloak"
	PresetSynology = "synology"
	PresetGeneric  = "generic"
)

// Limits on a provider's settings.
const (
	MaxRules       = 64
	MaxScopes      = 16
	MaxMFAValues   = 16
	maxGroupLen    = 256
	maxClaimLen    = 128
	maxNameLen     = 40
	maxIssuerLen   = 512
	maxClientIDLen = 256
	// DefaultSignInEveryDays: an account the provider made lapses a week
	// after its last sign-in through it.
	DefaultSignInEveryDays = 7
	maxSignInEveryDays     = 365
)

// IDPattern is what a provider's ID may be: it's in URLs and in the
// accounts bound to it, so it never changes.
var IDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

var scopePattern = regexp.MustCompile(`^[\x21\x23-\x5B\x5D-\x7E]{1,64}$`)

// Rule gives the accounts whose groups include Group a role: over
// everything when it has neither Selector nor Domains, else a grant
// (auth.Grant) on the nodes Selector matches, narrowed to Domains.
type Rule struct {
	Group    string            `json:"group"`
	Role     auth.Role         `json:"role"`
	Selector map[string]string `json:"selector,omitempty"`
	Domains  []string          `json:"domains,omitempty"`
}

// grant reports whether r is a grant rather than a role over everything.
func (r Rule) grant() bool { return len(r.Selector) > 0 || len(r.Domains) > 0 }

// Provider is one OpenID Connect provider's settings. Its client secret
// is kept apart, sealed (Store).
type Provider struct {
	// ID names it in URLs and in the accounts it signs in: set once.
	ID string `json:"id"`
	// Name is the sign-in button's: "Sign in with <Name>".
	Name   string `json:"name"`
	Preset string `json:"preset"`
	// Enabled: offered on the sign-in page. A provider starts disabled.
	Enabled bool `json:"enabled"`
	// Issuer is the provider's issuer URL exactly as its discovery
	// document says it (https://<host>/realms/<realm>, https://<nas>/webman/sso).
	Issuer   string   `json:"issuer"`
	ClientID string   `json:"client_id"`
	Scopes   []string `json:"scopes"`
	// UsernameClaim names the account the provider makes; GroupsClaim
	// lists the groups the rules match. A dotted name reaches into an
	// object (realm_access.roles) unless a claim has that name itself.
	UsernameClaim string `json:"username_claim"`
	GroupsClaim   string `json:"groups_claim"`
	// TLS is how the Controller trusts the provider's certificate when it
	// fetches its keys and exchanges codes: the system's store, a CA, or
	// a fingerprint.
	TLS Trust `json:"tls,omitzero"`
	// MFA is MFAJanus (empty) or MFAProvider; ACR and AMR are what an ID
	// token must carry for the provider's second factor to count (any
	// one of them; none: trusted as is).
	MFA string   `json:"mfa,omitempty"`
	ACR []string `json:"acr,omitempty"`
	AMR []string `json:"amr,omitempty"`
	// DefaultRole is the least anyone the provider signs in gets: none
	// (empty) refuses whoever no rule matches.
	DefaultRole auth.Role `json:"default_role,omitempty"`
	Rules       []Rule    `json:"rules"`
	// SignInEveryDays: an account this provider made stops working - its
	// sessions, API tokens and SSH keys - that long after its last
	// sign-in through it, until the next one (0: never).
	SignInEveryDays int `json:"sign_in_every_days"`
	// DisablePKCE leaves PKCE out, for a server that fails with it;
	// AllowMissingNonce accepts an ID token without the nonce, for one
	// that doesn't send it back. Never both.
	DisablePKCE       bool `json:"disable_pkce,omitempty"`
	AllowMissingNonce bool `json:"allow_missing_nonce,omitempty"`
	// LocalLogout signs out of the Controller only, never of the
	// provider (which ends its own session when it publishes how).
	LocalLogout bool `json:"local_logout,omitempty"`
	// Version moves with every change: what a sign-in under way and the
	// cached discovery belong to.
	Version int `json:"version"`
}

// Trust is tlstrust.Trust without a client certificate: a provider's
// settings hold no private key.
type Trust struct {
	CAPEM       string `json:"ca_pem,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	ServerName  string `json:"server_name,omitempty"`
}

func (t Trust) tls() tlstrust.Trust {
	return tlstrust.Trust{CAPEM: t.CAPEM, Fingerprint: t.Fingerprint, ServerName: t.ServerName}
}

// Preset is a new provider of that kind, before its issuer, client and
// rules.
func Preset(kind string) (Provider, error) {
	p := Provider{Preset: kind, Rules: []Rule{}, SignInEveryDays: DefaultSignInEveryDays, MFA: MFAJanus}
	switch kind {
	case PresetKeycloak:
		// Keycloak's groups come from a "Group Membership" mapper named
		// groups - full paths by default: /janus-admins.
		p.Scopes = []string{"openid", "profile", "email"}
		p.UsernameClaim, p.GroupsClaim = "preferred_username", "groups"
	case PresetSynology:
		// A Synology's SSO Server knows no "profile" scope; its names
		// are in "username" (and "sub"), its groups need their scope.
		p.Scopes = []string{"openid", "email", "groups"}
		p.UsernameClaim, p.GroupsClaim = "username", "groups"
	case PresetGeneric:
		p.Scopes = []string{"openid", "profile", "email"}
		p.UsernameClaim, p.GroupsClaim = "preferred_username", "groups"
	default:
		return Provider{}, fmt.Errorf("unknown preset %q (keycloak, synology or generic)", kind)
	}
	return p, nil
}

// Presets are every preset, for the page.
func Presets() map[string]Provider {
	out := map[string]Provider{}
	for _, k := range []string{PresetKeycloak, PresetSynology, PresetGeneric} {
		out[k], _ = Preset(k)
	}
	return out
}

// Validate checks p without connecting anywhere.
func (p Provider) Validate() error {
	if !IDPattern.MatchString(p.ID) {
		return errors.New("id: 1 to 32 lowercase letters, digits and -, starting with a letter or a digit")
	}
	if n := strings.TrimSpace(p.Name); n == "" || utf8.RuneCountInString(n) > maxNameLen || n != p.Name {
		return fmt.Errorf("name: 1 to %d characters, no leading or trailing space", maxNameLen)
	}
	switch p.Preset {
	case PresetKeycloak, PresetSynology, PresetGeneric:
	default:
		return fmt.Errorf("preset: keycloak, synology or generic, not %q", p.Preset)
	}
	if err := checkIssuer(p.Issuer); err != nil {
		return err
	}
	if p.ClientID == "" || len(p.ClientID) > maxClientIDLen || strings.TrimSpace(p.ClientID) != p.ClientID {
		return fmt.Errorf("client_id: 1 to %d characters, no leading or trailing space", maxClientIDLen)
	}
	if err := checkScopes(p.Scopes); err != nil {
		return err
	}
	for field, c := range map[string]string{"username_claim": p.UsernameClaim, "groups_claim": p.GroupsClaim} {
		if c == "" || len(c) > maxClaimLen || strings.ContainsAny(c, " \t\r\n") {
			return fmt.Errorf("%s: 1 to %d characters, no space", field, maxClaimLen)
		}
	}
	if err := p.TLS.tls().Validate(); err != nil {
		return fmt.Errorf("tls: %w", err)
	}
	switch p.MFA {
	case "", MFAJanus:
		if len(p.ACR) > 0 || len(p.AMR) > 0 {
			return errors.New("acr and amr are what the provider's second factor is checked by: only with mfa provider")
		}
	case MFAProvider:
	default:
		return fmt.Errorf("mfa: janus or provider, not %q", p.MFA)
	}
	for field, vs := range map[string][]string{"acr": p.ACR, "amr": p.AMR} {
		if len(vs) > MaxMFAValues {
			return fmt.Errorf("%s: %d values at most", field, MaxMFAValues)
		}
		for _, v := range vs {
			if v == "" || len(v) > maxClaimLen || strings.ContainsAny(v, " \t\r\n") {
				return fmt.Errorf("%s: each value 1 to %d characters, no space", field, maxClaimLen)
			}
		}
	}
	if p.DefaultRole != auth.None {
		if _, err := auth.ParseRole(string(p.DefaultRole)); err != nil {
			return fmt.Errorf("default_role: %w", err)
		}
	}
	if err := checkRules(p.Rules); err != nil {
		return err
	}
	if p.SignInEveryDays < 0 || p.SignInEveryDays > maxSignInEveryDays {
		return fmt.Errorf("sign_in_every_days: 0 (never) to %d", maxSignInEveryDays)
	}
	if p.DisablePKCE && p.AllowMissingNonce {
		return errors.New("without PKCE, the nonce is what binds the ID token to this sign-in: disable_pkce and allow_missing_nonce can't both be on")
	}
	return nil
}

// checkIssuer wants an https URL without a query or a fragment - the
// discovery document is under it, and an ID token's iss must equal it.
func checkIssuer(issuer string) error {
	if issuer == "" || len(issuer) > maxIssuerLen {
		return errors.New("issuer: the provider's issuer URL")
	}
	u, err := url.Parse(issuer)
	if err != nil {
		return fmt.Errorf("issuer: %w", err)
	}
	if u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.HasSuffix(issuer, "?") || strings.HasSuffix(issuer, "#") {
		return errors.New("issuer: an https URL with no query, fragment or credentials (https://keycloak.example.com/realms/janus)")
	}
	return nil
}

func checkScopes(scopes []string) error {
	if len(scopes) > MaxScopes {
		return fmt.Errorf("scopes: %d at most", MaxScopes)
	}
	if !slices.Contains(scopes, "openid") {
		return errors.New("scopes: openid is one of them")
	}
	for i, s := range scopes {
		if !scopePattern.MatchString(s) {
			return fmt.Errorf("scopes: %q isn't a scope", s)
		}
		if slices.Contains(scopes[:i], s) {
			return fmt.Errorf("scopes: %q twice", s)
		}
	}
	return nil
}

func checkRules(rules []Rule) error {
	if len(rules) > MaxRules {
		return fmt.Errorf("rules: %d at most", MaxRules)
	}
	grants := 0
	for i, r := range rules {
		if r.Group == "" || len(r.Group) > maxGroupLen {
			return fmt.Errorf("rule %d: a group of 1 to %d bytes", i+1, maxGroupLen)
		}
		if _, err := auth.ParseRole(string(r.Role)); err != nil {
			return fmt.Errorf("rule %d: %w", i+1, err)
		}
		if err := labels.Check(r.Selector); err != nil {
			return fmt.Errorf("rule %d: %w", i+1, err)
		}
		if err := auth.CheckDomains(r.Domains); err != nil {
			return fmt.Errorf("rule %d: %w", i+1, err)
		}
		if r.grant() {
			grants++
		}
	}
	// Every grant rule matching at once must still fit an account.
	if grants > auth.MaxGrants {
		return fmt.Errorf("rules: %d giving grants, %d at most", grants, auth.MaxGrants)
	}
	return nil
}

// Evaluate is what p's rules give someone in groups: the highest role a
// rule gives - the default role at least -, every grant a rule gives,
// once each.
func (p Provider) Evaluate(groups []string) auth.SSOAccess {
	var a auth.SSOAccess
	for _, r := range p.Rules {
		if !slices.Contains(groups, r.Group) {
			continue
		}
		a.Matched = true
		if !r.grant() {
			if !a.Role.AtLeast(r.Role) {
				a.Role = r.Role
			}
			continue
		}
		g := auth.Grant{Role: r.Role, Selector: r.Selector, Domains: r.Domains}
		if !slices.ContainsFunc(a.Grants, func(h auth.Grant) bool { return auth.SameGrant(g, h) }) {
			a.Grants = append(a.Grants, g)
		}
	}
	if p.DefaultRole != auth.None {
		a.Matched = true
		if !a.Role.AtLeast(p.DefaultRole) {
			a.Role = p.DefaultRole
		}
	}
	return a
}

// MFAMet reports whether a sign-in's ID token stands for its second
// factor: p in MFAProvider mode, trusting the provider as is (no ACR nor
// AMR asked) or the token carrying one of them.
func (p Provider) MFAMet(acr string, amr []string) bool {
	if p.MFA != MFAProvider {
		return false
	}
	if len(p.ACR) == 0 && len(p.AMR) == 0 {
		return true
	}
	if acr != "" && slices.Contains(p.ACR, acr) {
		return true
	}
	for _, m := range amr {
		if slices.Contains(p.AMR, m) {
			return true
		}
	}
	return false
}
