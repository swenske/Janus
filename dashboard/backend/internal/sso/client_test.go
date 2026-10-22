package sso

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/swenske/Janus/dashboard/backend/internal/auth"
	"github.com/swenske/Janus/dashboard/backend/internal/sso/ssotest"
)

const redirectURI = "https://controller.test/api/auth/sso/callback"

// provider is idp's settings, preset kind, changed by change.
func provider(t *testing.T, idp *ssotest.IdP, kind string, change func(*Provider)) Provider {
	t.Helper()
	p, err := Preset(kind)
	if err != nil {
		t.Fatal(err)
	}
	p.ID, p.Name, p.Issuer, p.ClientID, p.Enabled, p.Version = "idp", "Test", idp.URL, idp.ClientID, true, 1
	p.TLS = Trust{CAPEM: idp.CAPEM}
	p.Rules = []Rule{{Group: "/janus-admins", Role: auth.Admin}}
	if change != nil {
		change(&p)
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	return p
}

func client(t *testing.T, idp *ssotest.IdP, p Provider, secret string) *Client {
	t.Helper()
	c, err := NewClients().Get(context.Background(), p, secret)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// signIn is user signing in through c: the Controller's side of a whole
// sign-in, the provider's answer exchanged.
func signIn(t *testing.T, idp *ssotest.IdP, c *Client, user string) (*Result, error) {
	t.Helper()
	state, nonce, verifier := Random(32), Random(32), oauth2.GenerateVerifier()
	back := idp.SignIn(t, c.AuthURL(redirectURI, state, nonce, verifier, ""), user, false)
	if got := back.Query().Get("state"); got != state {
		t.Fatalf("state %q came back as %q", state, got)
	}
	return c.Exchange(context.Background(), redirectURI, back.Query().Get("code"), verifier, nonce)
}

func keycloakUser(idp *ssotest.IdP) {
	idp.AddUser("alice", map[string]any{
		"sub": "5f0c1a7e-4b7d-4a39-9d3e-0c1d2e3f4a5b", "preferred_username": "Alice", "email": "alice@example.com",
		"groups": []string{"/janus-admins", "/staff"},
	})
}

func TestExchangeKeycloak(t *testing.T) {
	idp := ssotest.New(t)
	keycloakUser(idp)
	c := client(t, idp, provider(t, idp, PresetKeycloak, nil), idp.ClientSecret)
	res, err := signIn(t, idp, c, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if res.Issuer != idp.URL || res.Subject != "5f0c1a7e-4b7d-4a39-9d3e-0c1d2e3f4a5b" || res.Username != "alice" {
		t.Fatalf("result: %+v", res)
	}
	if !slices.Equal(res.Groups, []string{"/janus-admins", "/staff"}) || res.IDToken == "" || !slices.Contains(res.Claims, "nonce") {
		t.Fatalf("result: %+v", res)
	}
	if got := idp.AuthUsed(); !slices.Equal(got, []string{"client_secret_basic"}) {
		t.Fatalf("client authentication: %v", got)
	}
}

func TestExchangeSynology(t *testing.T) {
	idp := ssotest.New(t)
	// A Synology's SSO Server: sub is the DSM user name, "username" too,
	// no profile scope, no end_session_endpoint - here its groups come
	// from user info only.
	idp.AddUser("alice", map[string]any{"sub": "alice", "username": "alice", "email": "alice@nas.example", "groups": []string{"administrators", "users"}})
	idp.Set(func(k *ssotest.Knobs) {
		k.NoEndSession, k.UserinfoOnly, k.AuthMethods = true, []string{"groups"}, []string{"client_secret_post"}
	})
	p := provider(t, idp, PresetSynology, func(p *Provider) { p.Rules = []Rule{{Group: "administrators", Role: auth.Admin}} })
	c := client(t, idp, p, idp.ClientSecret)
	res, err := signIn(t, idp, c, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if res.Subject != "alice" || res.Username != "alice" || !slices.Equal(res.Groups, []string{"administrators", "users"}) || !slices.Equal(res.UserinfoClaims, []string{"groups"}) {
		t.Fatalf("result: %+v", res)
	}
	if got := idp.AuthUsed(); !slices.Equal(got, []string{"client_secret_post"}) {
		t.Fatalf("client authentication: %v", got)
	}
	if u := c.EndSessionURL(res.IDToken, "https://controller.test/"); u != "" {
		t.Fatalf("no end_session_endpoint, yet %q", u)
	}
}

func TestExchangePublicClient(t *testing.T) {
	idp := ssotest.New(t)
	keycloakUser(idp)
	c := client(t, idp, provider(t, idp, PresetKeycloak, nil), "")
	if _, err := signIn(t, idp, c, "alice"); err != nil {
		t.Fatal(err)
	}
	if got := idp.AuthUsed(); !slices.Equal(got, []string{"none"}) {
		t.Fatalf("client authentication: %v", got)
	}
}

func code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func TestExchangeRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		knobs  func(*ssotest.Knobs)
		change func(*Provider)
		want   string // in the error; "" = accepted
	}{
		"no nonce":                    {func(k *ssotest.Knobs) { k.NoNonce = true }, nil, "no nonce"},
		"no nonce, allowed":           {func(k *ssotest.Knobs) { k.NoNonce = true }, func(p *Provider) { p.AllowMissingNonce = true }, ""},
		"another issuer's token":      {func(k *ssotest.Knobs) { k.TokenIssuer = "https://elsewhere.example" }, nil, "different provider"},
		"another audience, no azp":    {func(k *ssotest.Knobs) { k.ExtraAudience = "other" }, nil, "azp"},
		"another audience, azp ours":  {func(k *ssotest.Knobs) { k.ExtraAudience, k.Azp = "other", "janus" }, nil, ""},
		"issued to another client":    {func(k *ssotest.Knobs) { k.Azp = "other" }, nil, "issued to"},
		"expired":                     {func(k *ssotest.Knobs) { k.ExpiresIn = -2 * time.Minute }, nil, "expired"},
		"expired within the skew":     {func(k *ssotest.Knobs) { k.ExpiresIn = -30 * time.Second }, nil, ""},
		"issued in the future":        {func(k *ssotest.Knobs) { k.IssuedAt = time.Now().Add(10 * time.Minute) }, nil, "in the future"},
		"signed with the secret":      {func(k *ssotest.Knobs) { k.SignHS256 = true }, nil, "ID token"},
		"at_hash right":               {func(k *ssotest.Knobs) { k.AtHash = "right" }, nil, ""},
		"at_hash wrong":               {func(k *ssotest.Knobs) { k.AtHash = "wrong" }, nil, "at_hash"},
		"user info about another":     {func(k *ssotest.Knobs) { k.UserinfoOnly, k.UserinfoSubject = []string{"groups"}, "mallory" }, nil, "another subject"},
		"no username":                 {nil, func(p *Provider) { p.UsernameClaim = "nickname" }, "no username"},
		"secret in the form only":     {func(k *ssotest.Knobs) { k.AuthMethods = []string{"client_secret_post"} }, nil, ""},
		"no client authentication ok": {func(k *ssotest.Knobs) { k.AuthMethods = []string{"private_key_jwt"} }, nil, "neither client_secret_basic"},
	} {
		t.Run(name, func(t *testing.T) {
			idp := ssotest.New(t)
			keycloakUser(idp)
			if tc.knobs != nil {
				idp.Set(tc.knobs)
			}
			p := provider(t, idp, PresetKeycloak, tc.change)
			c, err := NewClients().Get(context.Background(), p, idp.ClientSecret)
			if err == nil {
				_, err = signIn(t, idp, c, "alice")
			}
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestExchangeOnce(t *testing.T) {
	idp := ssotest.New(t)
	keycloakUser(idp)
	c := client(t, idp, provider(t, idp, PresetKeycloak, nil), idp.ClientSecret)
	state, nonce, verifier := Random(32), Random(32), oauth2.GenerateVerifier()
	back := idp.SignIn(t, c.AuthURL(redirectURI, state, nonce, verifier, ""), "alice", false)
	ctx := context.Background()
	if _, err := c.Exchange(ctx, redirectURI, back.Query().Get("code"), oauth2.GenerateVerifier(), nonce); code(err) != CodeProvider {
		t.Fatalf("another verifier: %v", err)
	}
	back = idp.SignIn(t, c.AuthURL(redirectURI, state, nonce, verifier, ""), "alice", false)
	if _, err := c.Exchange(ctx, redirectURI, back.Query().Get("code"), verifier, Random(32)); code(err) != CodeToken {
		t.Fatalf("another nonce: %v", err)
	}
	// The code was used by that exchange: a second one fails.
	if _, err := c.Exchange(ctx, redirectURI, back.Query().Get("code"), verifier, nonce); code(err) != CodeProvider {
		t.Fatalf("a code used twice: %v", err)
	}
	back = idp.SignIn(t, c.AuthURL(redirectURI, state, nonce, verifier, ""), "alice", false)
	if _, err := c.Exchange(ctx, "https://evil.example/cb", back.Query().Get("code"), verifier, nonce); code(err) != CodeProvider {
		t.Fatalf("another redirect URI: %v", err)
	}
}

func TestDiscoveryRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		knobs  func(*ssotest.Knobs)
		change func(*Provider, *ssotest.IdP)
		want   string
	}{
		"issuer mismatch": {func(k *ssotest.Knobs) { k.DiscoveryIssuer = "https://127.0.0.1/other" }, nil, `issuer is "https://127.0.0.1/other"`},
		"trailing slash":  {nil, func(p *Provider, idp *ssotest.IdP) { p.Issuer = idp.URL + "/" }, "set the issuer exactly"},
		"too big":         {func(k *ssotest.Knobs) { k.BigDiscovery = true }, nil, "over 1048576 bytes"},
		"redirected":      {func(k *ssotest.Knobs) { k.RedirectDiscovery = true }, nil, "302"},
		"another CA": {nil, func(p *Provider, _ *ssotest.IdP) {
			p.TLS = Trust{CAPEM: ssotest.New(t).CAPEM}
		}, "certificate"},
		"pinned elsewhere": {nil, func(p *Provider, _ *ssotest.IdP) {
			p.TLS = Trust{Fingerprint: strings.Repeat("ab", 32)}
		}, "SHA-256"},
	} {
		t.Run(name, func(t *testing.T) {
			idp := ssotest.New(t)
			if tc.knobs != nil {
				idp.Set(tc.knobs)
			}
			p := provider(t, idp, PresetKeycloak, nil)
			if tc.change != nil {
				tc.change(&p, idp)
			}
			_, err := NewClients().Get(context.Background(), p, idp.ClientSecret)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestAuthURL(t *testing.T) {
	idp := ssotest.New(t)
	p := provider(t, idp, PresetKeycloak, nil)
	q := func(p Provider, prompt string) url.Values {
		u, err := url.Parse(client(t, idp, p, idp.ClientSecret).AuthURL(redirectURI, "st", "no", "ve", prompt))
		if err != nil {
			t.Fatal(err)
		}
		return u.Query()
	}
	got := q(p, "")
	if got.Get("state") != "st" || got.Get("nonce") != "no" || got.Get("code_challenge_method") != "S256" || got.Get("code_challenge") == "" ||
		got.Get("redirect_uri") != redirectURI || got.Get("scope") != "openid profile email" || got.Has("prompt") || got.Has("acr_values") {
		t.Fatalf("query: %v", got)
	}
	p.DisablePKCE, p.MFA, p.ACR, p.Version = true, MFAProvider, []string{"gold", "silver"}, 2
	got = q(p, "login")
	if got.Has("code_challenge") || got.Get("acr_values") != "gold silver" || got.Get("prompt") != "login" {
		t.Fatalf("query: %v", got)
	}
}

func TestEndSessionURL(t *testing.T) {
	idp := ssotest.New(t)
	p := provider(t, idp, PresetKeycloak, nil)
	u, err := url.Parse(client(t, idp, p, idp.ClientSecret).EndSessionURL("tok", "https://controller.test/"))
	if err != nil {
		t.Fatal(err)
	}
	if q := u.Query(); u.Path != "/logout" || q.Get("id_token_hint") != "tok" || q.Get("client_id") != "janus" || q.Get("post_logout_redirect_uri") != "https://controller.test/" {
		t.Fatalf("end session: %s", u)
	}
	p.LocalLogout, p.Version = true, 2
	if got := client(t, idp, p, idp.ClientSecret).EndSessionURL("tok", "https://controller.test/"); got != "" {
		t.Fatalf("a local sign-out: %q", got)
	}
}

func TestClientsCache(t *testing.T) {
	idp := ssotest.New(t)
	p := provider(t, idp, PresetKeycloak, nil)
	cs := NewClients()
	now := time.Now()
	cs.now = func() time.Time { return now }
	a, err := cs.Get(context.Background(), p, "")
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := cs.Get(context.Background(), p, ""); b != a {
		t.Fatal("fetched again within the hour")
	}
	p.Version++
	b, err := cs.Get(context.Background(), p, "")
	if err != nil || b == a {
		t.Fatalf("new settings, same client: %v", err)
	}
	// The provider stops answering: the last discovery serves a day.
	idp.Set(func(k *ssotest.Knobs) { k.RedirectDiscovery = true })
	now = now.Add(2 * time.Hour)
	if c, err := cs.Get(context.Background(), p, ""); err != nil || c != b {
		t.Fatalf("stale: %v", err)
	}
	now = now.Add(23 * time.Hour)
	if _, err := cs.Get(context.Background(), p, ""); err == nil {
		t.Fatal("a discovery over a day old still served")
	}
}

func TestHTTPClientHTTPSOnly(t *testing.T) {
	_, err := HTTPClient(Trust{}).Get("http://127.0.0.1:1/")
	if err == nil || !strings.Contains(err.Error(), "https only") {
		t.Fatalf("plain http: %v", err)
	}
	req, _ := http.NewRequest(http.MethodGet, "https://127.0.0.1:1/", nil)
	if _, err := HTTPClient(Trust{}).Do(req); err == nil {
		t.Fatal("nothing listens there")
	}
}

func TestCheck(t *testing.T) {
	idp := ssotest.New(t)
	p := provider(t, idp, PresetKeycloak, nil)
	r := Check(context.Background(), p, true)
	if r.Error != "" || !r.IssuerMatches || r.Keys != 1 || r.TokenAuth != "client_secret_basic" || len(r.Warnings) != 0 {
		t.Fatalf("report: %+v", r)
	}
	idp.Set(func(k *ssotest.Knobs) {
		k.DiscoveryIssuer, k.NoEndSession, k.NoS256, k.ScopesSupported = idp.URL+"/", true, true, []string{"openid", "email", "groups"}
	})
	r = Check(context.Background(), p, false)
	want := []string{`issuer is "` + idp.URL + `/"`, `scope "profile"`, "PKCE", "end_session_endpoint", "public client"}
	if r.Error != "" || r.IssuerMatches || len(r.Warnings) != len(want) {
		t.Fatalf("report: %+v", r)
	}
	for _, w := range want {
		if !slices.ContainsFunc(r.Warnings, func(s string) bool { return strings.Contains(s, w) }) {
			t.Errorf("no warning about %q: %v", w, r.Warnings)
		}
	}
	p.Issuer = "https://127.0.0.1:1"
	if r := Check(context.Background(), p, true); r.Error == "" {
		t.Fatal("nothing there, no error")
	}
}
