package sso

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/swenske/Janus/dashboard/backend/internal/httpsclient"
)

const (
	// maxResponse bounds every answer a provider gives - discovery, keys,
	// tokens, user info: go-oidc reads them whole.
	maxResponse = 1 << 20
	// requestTimeout bounds each request to a provider.
	requestTimeout = 15 * time.Second
	// clockSkew is how far behind the Controller's clock may be from the
	// provider's for an ID token's expiry; maxFuture how far ahead its
	// "issued at" may be.
	clockSkew = time.Minute
	maxFuture = 5 * time.Minute
	// discoveryTTL is how long a provider's discovery is used before it's
	// fetched again; staleFor how long a stale one still serves while
	// the provider doesn't answer.
	discoveryTTL = time.Hour
	staleFor     = 24 * time.Hour
	// MaxGroups is the most groups of an ID token the rules look at;
	// longer group names are left out.
	MaxGroups = 256
	// maxIDToken is the longest ID token a session keeps, as the hint of
	// a sign-out at the provider.
	maxIDToken = 8 << 10
)

// Error codes: what the browser is told went wrong - the details go to
// the log.
const (
	// CodeConfig: the provider is unknown, disabled, or not set up.
	CodeConfig = "config"
	// CodeProvider: the provider didn't answer as it should (discovery,
	// keys, the code exchange).
	CodeProvider = "provider"
	// CodeDenied: the provider refused, or the person cancelled.
	CodeDenied = "denied"
	// CodeToken: the ID token didn't check.
	CodeToken = "token"
	// CodeExpired: no sign-in under way in this browser (too old, already
	// used, or another one started since).
	CodeExpired = "expired"
)

// Error is a failed sign-in: Code for the browser, Err for the log.
type Error struct {
	Code string
	Err  error
}

func (e *Error) Error() string { return e.Code + ": " + e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

func fail(code string, format string, args ...any) error {
	return &Error{Code: code, Err: fmt.Errorf(format, args...)}
}

// Metadata is what the Controller reads of a provider's discovery
// document.
type Metadata struct {
	Issuer                   string   `json:"issuer"`
	AuthorizationEndpoint    string   `json:"authorization_endpoint"`
	TokenEndpoint            string   `json:"token_endpoint"`
	UserinfoEndpoint         string   `json:"userinfo_endpoint"`
	EndSessionEndpoint       string   `json:"end_session_endpoint"`
	JWKSURI                  string   `json:"jwks_uri"`
	ScopesSupported          []string `json:"scopes_supported"`
	ClaimsSupported          []string `json:"claims_supported"`
	TokenEndpointAuthMethods []string `json:"token_endpoint_auth_methods_supported"`
	CodeChallengeMethods     []string `json:"code_challenge_methods_supported"`
	SigningAlgs              []string `json:"id_token_signing_alg_values_supported"`
}

// endpoints are the URLs the Controller or the browser goes to: https
// only.
func (m Metadata) checkEndpoints() error {
	for name, u := range map[string]string{
		"authorization_endpoint": m.AuthorizationEndpoint, "token_endpoint": m.TokenEndpoint, "jwks_uri": m.JWKSURI,
		"userinfo_endpoint": m.UserinfoEndpoint, "end_session_endpoint": m.EndSessionEndpoint,
	} {
		if u == "" {
			if name == "authorization_endpoint" || name == "token_endpoint" || name == "jwks_uri" {
				return fmt.Errorf("the discovery document has no %s", name)
			}
			continue
		}
		p, err := url.Parse(u)
		if err != nil || p.Scheme != "https" || p.Host == "" {
			return fmt.Errorf("the discovery document's %s isn't an https URL: %q", name, u)
		}
	}
	return nil
}

// authStyle is how the client authenticates at the token endpoint: HTTP
// Basic unless the provider only takes the secret in the form (never
// both in turn - a code is good once). A public client sends its ID
// alone.
func authStyle(methods []string, hasSecret bool) (oauth2.AuthStyle, string, error) {
	switch {
	case !hasSecret:
		return oauth2.AuthStyleInParams, "none", nil
	case len(methods) == 0 || slices.Contains(methods, "client_secret_basic"):
		return oauth2.AuthStyleInHeader, "client_secret_basic", nil
	case slices.Contains(methods, "client_secret_post"):
		return oauth2.AuthStyleInParams, "client_secret_post", nil
	}
	return 0, "", fmt.Errorf("the provider takes neither client_secret_basic nor client_secret_post (%s)", strings.Join(methods, ", "))
}

// HTTPClient is how the Controller reaches a provider: trusting its
// certificate as t says, no proxy, no redirect followed, https only,
// answers capped (internal/httpsclient).
func HTTPClient(t Trust) *http.Client {
	return httpsclient.New(t.tls(), httpsclient.Options{MaxBytes: maxResponse, Timeout: requestTimeout, What: "provider"})
}

// Client signs in through one version of a provider's settings.
type Client struct {
	p        Provider
	secret   string
	http     *http.Client
	oidc     *oidc.Provider
	verifier *oidc.IDTokenVerifier
	meta     Metadata
	style    oauth2.AuthStyle
	now      func() time.Time
	fetched  time.Time
}

// newClient fetches p's discovery document.
func newClient(ctx context.Context, p Provider, secret string, now func() time.Time) (*Client, error) {
	hc := HTTPClient(p.TLS)
	ctx, cancel := context.WithTimeout(oidc.ClientContext(ctx, hc), requestTimeout)
	defer cancel()
	op, err := oidc.NewProvider(ctx, p.Issuer)
	if err != nil {
		var mismatch *oidc.IssuerMismatchError
		if errors.As(err, &mismatch) {
			return nil, fail(CodeConfig, "the discovery document's issuer is %q, not %q: set the issuer exactly as it says", mismatch.Discovered, mismatch.Provided)
		}
		return nil, fail(CodeProvider, "discovery: %w", err)
	}
	var meta Metadata
	if err := op.Claims(&meta); err != nil {
		return nil, fail(CodeProvider, "discovery: %w", err)
	}
	if err := meta.checkEndpoints(); err != nil {
		return nil, fail(CodeProvider, "discovery: %w", err)
	}
	style, _, err := authStyle(meta.TokenEndpointAuthMethods, secret != "")
	if err != nil {
		return nil, fail(CodeConfig, "%w", err)
	}
	c := &Client{p: p, secret: secret, http: hc, oidc: op, meta: meta, style: style, now: now, fetched: now()}
	// go-oidc keeps the asymmetric algorithms the provider lists (RS256
	// when it lists none): "none" and HS256 never verify.
	c.verifier = op.Verifier(&oidc.Config{ClientID: p.ClientID, Now: func() time.Time { return c.now().Add(-clockSkew) }})
	return c, nil
}

// Provider is the settings c signs in with.
func (c *Client) Provider() Provider { return c.p }

// Metadata is what the provider's discovery says.
func (c *Client) Metadata() Metadata { return c.meta }

func (c *Client) config(redirectURI string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     c.p.ClientID,
		ClientSecret: c.secret,
		Endpoint:     oauth2.Endpoint{AuthURL: c.meta.AuthorizationEndpoint, TokenURL: c.meta.TokenEndpoint, AuthStyle: c.style},
		RedirectURL:  redirectURI,
		Scopes:       c.p.Scopes,
	}
}

// AuthURL is where the browser goes to sign in: state, nonce, the PKCE
// challenge of verifier, the ACR values a provider asked for its second
// factor should run, and prompt when not empty ("login": sign in again,
// whatever session the provider has).
func (c *Client) AuthURL(redirectURI, state, nonce, verifier, prompt string) string {
	opts := []oauth2.AuthCodeOption{oidc.Nonce(nonce)}
	if !c.p.DisablePKCE {
		opts = append(opts, oauth2.S256ChallengeOption(verifier))
	}
	if c.p.MFA == MFAProvider && len(c.p.ACR) > 0 {
		opts = append(opts, oauth2.SetAuthURLParam("acr_values", strings.Join(c.p.ACR, " ")))
	}
	if prompt != "" {
		opts = append(opts, oauth2.SetAuthURLParam("prompt", prompt))
	}
	return c.config(redirectURI).AuthCodeURL(state, opts...)
}

// Result is who a sign-in through a provider is.
type Result struct {
	Issuer, Subject string
	// Username is the username claim, lowercased.
	Username string
	Groups   []string
	ACR      string
	AMR      []string
	// IDToken is the ID token as received - the hint of a sign-out at the
	// provider -, empty when too long to keep.
	IDToken string
	// Claims are the names of the claims the ID token carried: logged,
	// never their values.
	Claims []string
	// UserinfoClaims are those user info added.
	UserinfoClaims []string
}

// Exchange turns the code the provider sent back into who signed in:
// the ID token checked, its nonce equal to nonce, the claims the
// settings name read from it (or from user info, for those it lacks).
func (c *Client) Exchange(ctx context.Context, redirectURI, code, verifier, nonce string) (*Result, error) {
	ctx, cancel := context.WithTimeout(oidc.ClientContext(ctx, c.http), 2*requestTimeout)
	defer cancel()
	var opts []oauth2.AuthCodeOption
	if !c.p.DisablePKCE {
		opts = append(opts, oauth2.VerifierOption(verifier))
	}
	tok, err := c.config(redirectURI).Exchange(ctx, code, opts...)
	if err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) && re.ErrorCode != "" {
			return nil, fail(CodeProvider, "the code exchange: the provider answered %q", re.ErrorCode)
		}
		return nil, fail(CodeProvider, "the code exchange: %w", err)
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return nil, fail(CodeToken, "the provider's answer has no ID token (is openid among the scopes?)")
	}
	idt, err := c.verifier.Verify(ctx, raw)
	if err != nil {
		return nil, fail(CodeToken, "the ID token: %w", err)
	}
	now := c.now()
	if idt.IssuedAt.After(now.Add(maxFuture)) {
		return nil, fail(CodeToken, "the ID token was issued at %s, in the future: is the Controller's clock right?", idt.IssuedAt.UTC().Format(time.RFC3339))
	}
	switch {
	case idt.Nonce == "" && !c.p.AllowMissingNonce:
		return nil, fail(CodeToken, "the ID token has no nonce (allow_missing_nonce, with PKCE on, accepts that)")
	case idt.Nonce != "" && subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(nonce)) != 1:
		return nil, fail(CodeToken, "the ID token's nonce isn't this sign-in's")
	}
	claims := map[string]any{}
	if err := idt.Claims(&claims); err != nil {
		return nil, fail(CodeToken, "the ID token's claims: %w", err)
	}
	azp, _ := claims["azp"].(string)
	if len(idt.Audience) > 1 && azp == "" {
		return nil, fail(CodeToken, "the ID token is for several audiences and names no authorized party (azp)")
	}
	if azp != "" && azp != c.p.ClientID {
		return nil, fail(CodeToken, "the ID token was issued to %q, not this client", azp)
	}
	if idt.AccessTokenHash != "" && tok.AccessToken != "" {
		if err := idt.VerifyAccessToken(tok.AccessToken); err != nil {
			return nil, fail(CodeToken, "the ID token's at_hash: %w", err)
		}
	}
	res := &Result{Issuer: idt.Issuer, Subject: idt.Subject, Claims: claimNames(claims)}
	if idt.Subject == "" {
		return nil, fail(CodeToken, "the ID token has no subject (sub)")
	}
	_, hasName := lookup(claims, c.p.UsernameClaim)
	_, hasGroups := lookup(claims, c.p.GroupsClaim)
	if (!hasName || !hasGroups) && c.meta.UserinfoEndpoint != "" && tok.AccessToken != "" {
		extra, err := c.userInfo(ctx, tok, idt.Subject)
		if err != nil {
			return nil, err
		}
		for k, v := range extra {
			if _, ok := claims[k]; !ok {
				claims[k] = v
				res.UserinfoClaims = append(res.UserinfoClaims, k)
			}
		}
		slices.Sort(res.UserinfoClaims)
	}
	v, _ := lookup(claims, c.p.UsernameClaim)
	name, _ := v.(string)
	if res.Username = strings.ToLower(strings.TrimSpace(name)); res.Username == "" {
		return nil, fail(CodeToken, "no username in the claim %q", c.p.UsernameClaim)
	}
	v, _ = lookup(claims, c.p.GroupsClaim)
	res.Groups = groups(v)
	res.ACR, _ = claims["acr"].(string)
	res.AMR = stringList(claims["amr"])
	if len(raw) <= maxIDToken {
		res.IDToken = raw
	}
	return res, nil
}

// userInfo is the provider's user info - only for the same subject.
func (c *Client) userInfo(ctx context.Context, tok *oauth2.Token, subject string) (map[string]any, error) {
	ui, err := c.oidc.UserInfo(ctx, oauth2.StaticTokenSource(tok))
	if err != nil {
		return nil, fail(CodeProvider, "user info: %w", err)
	}
	if ui.Subject != subject {
		return nil, fail(CodeToken, "user info is about another subject than the ID token")
	}
	out := map[string]any{}
	if err := ui.Claims(&out); err != nil {
		return nil, fail(CodeProvider, "user info: %w", err)
	}
	return out, nil
}

// EndSessionURL is where the browser signs out of the provider too -
// empty when the provider publishes no way, or the settings keep the
// sign-out local.
func (c *Client) EndSessionURL(idToken, postLogout string) string {
	if c.p.LocalLogout || c.meta.EndSessionEndpoint == "" {
		return ""
	}
	u, err := url.Parse(c.meta.EndSessionEndpoint)
	if err != nil {
		return ""
	}
	q := u.Query()
	if idToken != "" {
		q.Set("id_token_hint", idToken)
	}
	q.Set("client_id", c.p.ClientID)
	q.Set("post_logout_redirect_uri", postLogout)
	u.RawQuery = q.Encode()
	return u.String()
}

// lookup is a claim by its name - or, a dotted name no claim has, by its
// path into objects (realm_access.roles).
func lookup(claims map[string]any, name string) (any, bool) {
	if v, ok := claims[name]; ok {
		return v, true
	}
	if !strings.Contains(name, ".") {
		return nil, false
	}
	var cur any = claims
	for part := range strings.SplitSeq(name, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[part]; !ok {
			return nil, false
		}
	}
	return cur, true
}

// groups reads a groups claim - a list of strings, or one string -,
// MaxGroups at most, each up to maxGroupLen bytes.
func groups(v any) []string {
	var out []string
	for _, g := range stringList(v) {
		if g == "" || len(g) > maxGroupLen || slices.Contains(out, g) {
			continue
		}
		if out = append(out, g); len(out) == MaxGroups {
			break
		}
	}
	return out
}

// stringList reads a claim that's a string or a list of them.
func stringList(v any) []string {
	switch v := v.(type) {
	case string:
		return []string{v}
	case []any:
		var out []string
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func claimNames(claims map[string]any) []string {
	out := make([]string, 0, len(claims))
	for k := range claims {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// Clients caches each provider's discovery: an hour, then fetched again
// - and while the provider doesn't answer, the last one still serves a
// day long. A change of the settings (a new Version) fetches it again.
type Clients struct {
	now func() time.Time

	mu sync.Mutex
	by map[string]*Client
}

// NewClients is an empty cache.
func NewClients() *Clients { return &Clients{now: time.Now, by: map[string]*Client{}} }

// Get is a client for p's settings, with its client secret.
func (cs *Clients) Get(ctx context.Context, p Provider, secret string) (*Client, error) {
	cs.mu.Lock()
	old := cs.by[p.ID]
	cs.mu.Unlock()
	now := cs.now()
	same := old != nil && old.p.Version == p.Version
	if same && now.Sub(old.fetched) < discoveryTTL {
		return old, nil
	}
	c, err := newClient(ctx, p, secret, cs.now)
	if err != nil {
		if same && now.Sub(old.fetched) < staleFor {
			return old, nil
		}
		return nil, err
	}
	cs.mu.Lock()
	cs.by[p.ID] = c
	cs.mu.Unlock()
	return c, nil
}

// Forget drops a provider's client (deleted).
func (cs *Clients) Forget(id string) {
	cs.mu.Lock()
	delete(cs.by, id)
	cs.mu.Unlock()
}
