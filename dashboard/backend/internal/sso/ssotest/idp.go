// Package ssotest is a fake OpenID Connect provider for tests: discovery,
// keys, an authorization endpoint that signs whoever the test names in,
// a token endpoint that checks the client, the redirect URI and PKCE,
// user info and an end-session endpoint - with knobs to make it answer
// the ways real providers (Keycloak, a Synology's SSO Server) do, or
// shouldn't.
package ssotest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// Knobs change how the provider answers.
type Knobs struct {
	// DiscoveryIssuer is the discovery document's issuer instead of the
	// provider's URL; TokenIssuer the ID tokens' iss.
	DiscoveryIssuer string
	TokenIssuer     string
	// NoNonce leaves the nonce out of ID tokens.
	NoNonce bool
	// ExtraAudience adds an audience to ID tokens; Azp sets azp.
	ExtraAudience string
	Azp           string
	// ExpiresIn is ID tokens' lifetime (5 min when zero; negative:
	// expired already); IssuedAt their iat (now when zero).
	ExpiresIn time.Duration
	IssuedAt  time.Time
	// SignHS256 signs ID tokens with the client secret (HMAC).
	SignHS256 bool
	// AuthMethods are token_endpoint_auth_methods_supported - what the
	// token endpoint accepts too (both basic and post by default).
	AuthMethods []string
	// NoS256 lists no PKCE method; NoEndSession no end_session_endpoint.
	NoS256       bool
	NoEndSession bool
	// UserinfoOnly are claims only user info gives, not the ID token;
	// UserinfoSubject its sub instead of the user's.
	UserinfoOnly    []string
	UserinfoSubject string
	// ISSParam adds the iss parameter (RFC 9207) to the authorization
	// response: "right" or "wrong".
	ISSParam string
	// BigDiscovery pads the discovery document past a megabyte;
	// RedirectDiscovery answers it with a redirect.
	BigDiscovery      bool
	RedirectDiscovery bool
	// ScopesSupported are listed as scopes_supported.
	ScopesSupported []string
	// AtHash: "right" or "wrong" adds the at_hash claim.
	AtHash string
}

type grant struct {
	user, clientID, redirectURI, nonce, challenge, method string
	used                                                  bool
}

// IdP is the fake provider.
type IdP struct {
	// URL is its issuer; CAPEM the certificate its TLS is served with (a
	// CA of its own).
	URL          string
	CAPEM        string
	ClientID     string
	ClientSecret string

	srv *httptest.Server
	key *rsa.PrivateKey

	mu     sync.Mutex
	knobs  Knobs
	users  map[string]map[string]any
	codes  map[string]*grant
	access map[string]string
	// authUsed are the client authentications the token endpoint saw;
	// logouts the end-session requests' queries.
	authUsed []string
	logouts  []url.Values
}

// New starts a provider for t, with the client "janus" and its secret.
func New(t testing.TB) *IdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	cert, caPEM := serverCert(t)
	p := &IdP{CAPEM: caPEM, ClientID: "janus", ClientSecret: "janus-client-secret-0123456789abcdef", key: key,
		users: map[string]map[string]any{}, codes: map[string]*grant{}, access: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", p.discovery)
	mux.HandleFunc("GET /jwks", p.jwks)
	mux.HandleFunc("GET /authorize", p.authorize)
	mux.HandleFunc("POST /token", p.token)
	mux.HandleFunc("GET /userinfo", p.userinfo)
	mux.HandleFunc("GET /logout", p.logout)
	p.srv = httptest.NewUnstartedServer(mux)
	p.srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	p.srv.StartTLS()
	t.Cleanup(p.srv.Close)
	p.URL = p.srv.URL
	return p
}

// serverCert is a self-signed CA certificate for 127.0.0.1 and localhost.
func serverCert(t testing.TB) (tls.Certificate, string) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "ssotest provider"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k}, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// Set changes the knobs.
func (p *IdP) Set(f func(*Knobs)) {
	p.mu.Lock()
	f(&p.knobs)
	p.mu.Unlock()
}

// AddUser makes user sign in with claims (sub among them).
func (p *IdP) AddUser(user string, claims map[string]any) {
	p.mu.Lock()
	p.users[user] = claims
	p.mu.Unlock()
}

// AuthUsed are the client authentications the token endpoint saw:
// "client_secret_basic", "client_secret_post" or "none".
func (p *IdP) AuthUsed() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.authUsed)
}

// Logouts are the end-session requests' queries.
func (p *IdP) Logouts() []url.Values {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.logouts)
}

// Client is an HTTP client trusting the provider, following no
// redirect.
func (p *IdP) Client() *http.Client {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM([]byte(p.CAPEM))
	return &http.Client{
		Transport:     &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// SignIn is user signing in at authURL (what the Controller sent the
// browser to): the URL the provider sends the browser back to, with the
// code and the state - or the error, when deny.
func (p *IdP) SignIn(t testing.TB, authURL, user string, deny bool) *url.URL {
	t.Helper()
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("test_user", user)
	if deny {
		q.Set("test_deny", "1")
	}
	u.RawQuery = q.Encode()
	resp, err := p.Client().Get(u.String())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize: %s", resp.Status)
	}
	back, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	return back
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (p *IdP) discovery(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	k := p.knobs
	p.mu.Unlock()
	if k.RedirectDiscovery {
		http.Redirect(w, r, p.URL+"/elsewhere", http.StatusFound)
		return
	}
	doc := map[string]any{
		"issuer":                                or(k.DiscoveryIssuer, p.URL),
		"authorization_endpoint":                p.URL + "/authorize",
		"token_endpoint":                        p.URL + "/token",
		"userinfo_endpoint":                     p.URL + "/userinfo",
		"jwks_uri":                              p.URL + "/jwks",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"token_endpoint_auth_methods_supported": p.methods(k),
	}
	if !k.NoS256 {
		doc["code_challenge_methods_supported"] = []string{"S256"}
	}
	if !k.NoEndSession {
		doc["end_session_endpoint"] = p.URL + "/logout"
	}
	if k.ScopesSupported != nil {
		doc["scopes_supported"] = k.ScopesSupported
	}
	if k.BigDiscovery {
		doc["padding"] = strings.Repeat("x", 2<<20)
	}
	writeJSON(w, doc)
}

func (p *IdP) methods(k Knobs) []string {
	if k.AuthMethods != nil {
		return k.AuthMethods
	}
	return []string{"client_secret_basic", "client_secret_post"}
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (p *IdP) jwks(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &p.key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
}

func (p *IdP) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	redirect, err := url.Parse(q.Get("redirect_uri"))
	if err != nil || q.Get("client_id") != p.ClientID || q.Get("response_type") != "code" {
		http.Error(w, "bad authorization request", http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	k := p.knobs
	_, known := p.users[q.Get("test_user")]
	p.mu.Unlock()
	back := url.Values{"state": {q.Get("state")}}
	switch k.ISSParam {
	case "right":
		back.Set("iss", p.URL)
	case "wrong":
		back.Set("iss", "https://elsewhere.example")
	}
	if q.Get("test_deny") == "1" || !known {
		back.Set("error", "access_denied")
		back.Set("error_description", "the user said no")
	} else {
		code := base64.RawURLEncoding.EncodeToString(random(24))
		p.mu.Lock()
		p.codes[code] = &grant{user: q.Get("test_user"), clientID: q.Get("client_id"), redirectURI: q.Get("redirect_uri"), nonce: q.Get("nonce"),
			challenge: q.Get("code_challenge"), method: q.Get("code_challenge_method")}
		p.mu.Unlock()
		back.Set("code", code)
	}
	redirect.RawQuery = back.Encode()
	http.Redirect(w, r, redirect.String(), http.StatusFound)
}

func random(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func tokenError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}

func (p *IdP) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		tokenError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	k := p.knobs
	// The client's authentication: Basic (form-encoded, RFC 6749
	// 2.3.1), the form, or none - each only when listed.
	method, id, secret := "none", r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	if u, pw, ok := r.BasicAuth(); ok {
		id, _ = url.QueryUnescape(u)
		secret, _ = url.QueryUnescape(pw)
		method = "client_secret_basic"
	} else if secret != "" {
		method = "client_secret_post"
	}
	p.authUsed = append(p.authUsed, method)
	if id != p.ClientID || (method != "none" && (secret != p.ClientSecret || !slices.Contains(p.methods(k), method))) {
		tokenError(w, http.StatusUnauthorized, "invalid_client")
		return
	}
	g, ok := p.codes[r.PostForm.Get("code")]
	if !ok || g.used || g.clientID != id || g.redirectURI != r.PostForm.Get("redirect_uri") || r.PostForm.Get("grant_type") != "authorization_code" {
		tokenError(w, http.StatusBadRequest, "invalid_grant")
		return
	}
	g.used = true
	if g.challenge != "" {
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if g.method != "S256" || base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
			tokenError(w, http.StatusBadRequest, "invalid_grant")
			return
		}
	} else if method == "none" {
		tokenError(w, http.StatusBadRequest, "invalid_request") // a public client needs PKCE
		return
	}
	access := base64.RawURLEncoding.EncodeToString(random(24))
	p.access[access] = g.user
	idToken, err := p.idToken(k, g, access)
	if err != nil {
		tokenError(w, http.StatusInternalServerError, "server_error")
		return
	}
	writeJSON(w, map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": 300, "id_token": idToken})
}

func (p *IdP) idToken(k Knobs, g *grant, access string) (string, error) {
	now := time.Now()
	iat := now
	if !k.IssuedAt.IsZero() {
		iat = k.IssuedAt
	}
	life := 5 * time.Minute
	if k.ExpiresIn != 0 {
		life = k.ExpiresIn
	}
	claims := map[string]any{}
	for c, v := range p.users[g.user] {
		if !slices.Contains(k.UserinfoOnly, c) {
			claims[c] = v
		}
	}
	claims["iss"] = or(k.TokenIssuer, p.URL)
	claims["aud"] = p.ClientID
	if k.ExtraAudience != "" {
		claims["aud"] = []string{p.ClientID, k.ExtraAudience}
	}
	if k.Azp != "" {
		claims["azp"] = k.Azp
	}
	claims["iat"] = iat.Unix()
	claims["exp"] = now.Add(life).Unix()
	if !k.NoNonce && g.nonce != "" {
		claims["nonce"] = g.nonce
	}
	switch k.AtHash {
	case "right":
		sum := sha256.Sum256([]byte(access))
		claims["at_hash"] = base64.RawURLEncoding.EncodeToString(sum[:16])
	case "wrong":
		claims["at_hash"] = base64.RawURLEncoding.EncodeToString(random(16))
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	key := jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: p.key, KeyID: "k1"}}
	if k.SignHS256 {
		key = jose.SigningKey{Algorithm: jose.HS256, Key: []byte(p.ClientSecret)}
	}
	signer, err := jose.NewSigner(key, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		return "", err
	}
	jws, err := signer.Sign(payload)
	if err != nil {
		return "", err
	}
	return jws.CompactSerialize()
}

func (p *IdP) userinfo(w http.ResponseWriter, r *http.Request) {
	bearer, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	p.mu.Lock()
	defer p.mu.Unlock()
	user, ok := p.access[bearer]
	if !ok {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}
	claims := map[string]any{}
	for c, v := range p.users[user] {
		claims[c] = v
	}
	if p.knobs.UserinfoSubject != "" {
		claims["sub"] = p.knobs.UserinfoSubject
	}
	writeJSON(w, claims)
}

func (p *IdP) logout(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.logouts = append(p.logouts, r.URL.Query())
	p.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}
