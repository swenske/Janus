package sso

import (
	"reflect"
	"strings"
	"testing"

	"github.com/swenske/Janus/dashboard/backend/internal/auth"
)

func keycloak(t *testing.T) Provider {
	t.Helper()
	p, err := Preset(PresetKeycloak)
	if err != nil {
		t.Fatal(err)
	}
	p.ID, p.Name, p.Issuer, p.ClientID = "kc", "Keycloak", "https://kc.example.com/realms/janus", "janus"
	return p
}

func TestValidate(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*Provider)
		want   string
	}{
		"keycloak preset":    {func(*Provider) {}, ""},
		"bad id":             {func(p *Provider) { p.ID = "Kc" }, "id:"},
		"empty name":         {func(p *Provider) { p.Name = "" }, "name:"},
		"padded name":        {func(p *Provider) { p.Name = " Keycloak" }, "name:"},
		"plain http":         {func(p *Provider) { p.Issuer = "http://kc.example.com/realms/janus" }, "issuer:"},
		"issuer with query":  {func(p *Provider) { p.Issuer = "https://kc.example.com/?x=1" }, "issuer:"},
		"issuer with user":   {func(p *Provider) { p.Issuer = "https://a:b@kc.example.com/" }, "issuer:"},
		"no client":          {func(p *Provider) { p.ClientID = "" }, "client_id:"},
		"no openid":          {func(p *Provider) { p.Scopes = []string{"profile"} }, "openid"},
		"scope twice":        {func(p *Provider) { p.Scopes = []string{"openid", "openid"} }, "twice"},
		"scope with space":   {func(p *Provider) { p.Scopes = []string{"openid", "a b"} }, "isn't a scope"},
		"claim with space":   {func(p *Provider) { p.GroupsClaim = "my groups" }, "groups_claim"},
		"unknown preset":     {func(p *Provider) { p.Preset = "okta" }, "preset:"},
		"acr without mode":   {func(p *Provider) { p.ACR = []string{"gold"} }, "only with mfa provider"},
		"acr with mode":      {func(p *Provider) { p.MFA, p.ACR = MFAProvider, []string{"gold"} }, ""},
		"unknown mfa":        {func(p *Provider) { p.MFA = "both" }, "mfa:"},
		"bad default role":   {func(p *Provider) { p.DefaultRole = "root" }, "default_role"},
		"rule without group": {func(p *Provider) { p.Rules = []Rule{{Role: auth.Admin}} }, "rule 1"},
		"rule role none":     {func(p *Provider) { p.Rules = []Rule{{Group: "g", Role: auth.None}} }, "rule 1"},
		"rule bad domain":    {func(p *Provider) { p.Rules = []Rule{{Group: "g", Role: auth.Reader, Domains: []string{"x"}}} }, "rule 1"},
		"days too long":      {func(p *Provider) { p.SignInEveryDays = 400 }, "sign_in_every_days"},
		"pkce and nonce off": {func(p *Provider) { p.DisablePKCE, p.AllowMissingNonce = true, true }, "can't both"},
		"tls both":           {func(p *Provider) { p.TLS = Trust{CAPEM: "x", Fingerprint: "y"} }, "tls:"},
	} {
		t.Run(name, func(t *testing.T) {
			p := keycloak(t)
			tc.change(&p)
			err := p.Validate()
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestEvaluate(t *testing.T) {
	p := keycloak(t)
	p.Rules = []Rule{
		{Group: "/janus-readers", Role: auth.Reader},
		{Group: "/janus-admins", Role: auth.Admin},
		{Group: "/janus-ops", Role: auth.Operator},
		{Group: "/web", Role: auth.Operator, Selector: map[string]string{"site": "web"}},
		{Group: "/web-too", Role: auth.Operator, Selector: map[string]string{"site": "web"}},
		{Group: "/net", Role: auth.Admin, Domains: []string{"network"}},
	}
	for name, tc := range map[string]struct {
		groups []string
		want   auth.SSOAccess
	}{
		"none":            {nil, auth.SSOAccess{}},
		"unknown group":   {[]string{"/other"}, auth.SSOAccess{}},
		"reader":          {[]string{"/janus-readers"}, auth.SSOAccess{Role: auth.Reader, Matched: true}},
		"highest wins":    {[]string{"/janus-ops", "/janus-readers", "/janus-admins"}, auth.SSOAccess{Role: auth.Admin, Matched: true}},
		"order unrelated": {[]string{"/janus-admins", "/janus-readers"}, auth.SSOAccess{Role: auth.Admin, Matched: true}},
		"grant only": {[]string{"/web"}, auth.SSOAccess{Matched: true, Grants: []auth.Grant{
			{Role: auth.Operator, Selector: map[string]string{"site": "web"}}}}},
		"same grant once": {[]string{"/web", "/web-too"}, auth.SSOAccess{Matched: true, Grants: []auth.Grant{
			{Role: auth.Operator, Selector: map[string]string{"site": "web"}}}}},
		"role and grants": {[]string{"/janus-readers", "/web", "/net"}, auth.SSOAccess{Role: auth.Reader, Matched: true, Grants: []auth.Grant{
			{Role: auth.Operator, Selector: map[string]string{"site": "web"}}, {Role: auth.Admin, Domains: []string{"network"}}}}},
		"case matters": {[]string{"/JANUS-ADMINS"}, auth.SSOAccess{}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := p.Evaluate(tc.groups); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Evaluate(%v) = %+v, want %+v", tc.groups, got, tc.want)
			}
		})
	}
	p.DefaultRole = auth.Operator
	if got := p.Evaluate(nil); got.Role != auth.Operator || !got.Matched {
		t.Fatalf("default role: %+v", got)
	}
	if got := p.Evaluate([]string{"/janus-readers"}); got.Role != auth.Operator {
		t.Fatalf("the default role is the least: %+v", got)
	}
	if got := p.Evaluate([]string{"/janus-admins"}); got.Role != auth.Admin {
		t.Fatalf("a rule above the default: %+v", got)
	}
}

func TestMFAMet(t *testing.T) {
	p := keycloak(t)
	if p.MFAMet("1", []string{"otp"}) {
		t.Fatal("janus mode never trusts the provider")
	}
	p.MFA = MFAProvider
	if !p.MFAMet("", nil) {
		t.Fatal("provider mode without acr/amr trusts it as is")
	}
	p.ACR = []string{"gold"}
	p.AMR = []string{"otp", "hwk"}
	for _, tc := range []struct {
		acr  string
		amr  []string
		want bool
	}{
		{"", nil, false},
		{"1", []string{"pwd"}, false},
		{"gold", nil, true},
		{"", []string{"pwd", "hwk"}, true},
	} {
		if got := p.MFAMet(tc.acr, tc.amr); got != tc.want {
			t.Errorf("MFAMet(%q, %v) = %v", tc.acr, tc.amr, got)
		}
	}
}

func TestLookup(t *testing.T) {
	claims := map[string]any{
		"groups":                    []any{"/a", "/b", 3, "/a"},
		"realm_access":              map[string]any{"roles": []any{"admin"}},
		"https://example.com/roles": []any{"x"},
		"username":                  "Alice",
	}
	if v, _ := lookup(claims, "realm_access.roles"); !reflect.DeepEqual(groups(v), []string{"admin"}) {
		t.Errorf("dotted path: %v", v)
	}
	if v, _ := lookup(claims, "https://example.com/roles"); !reflect.DeepEqual(groups(v), []string{"x"}) {
		t.Errorf("a claim named with dots: %v", v)
	}
	if v, _ := lookup(claims, "groups"); !reflect.DeepEqual(groups(v), []string{"/a", "/b"}) {
		t.Errorf("groups: %v (strings only, once each)", groups(v))
	}
	if _, ok := lookup(claims, "realm_access.missing"); ok {
		t.Error("a missing path")
	}
	if !reflect.DeepEqual(groups("solo"), []string{"solo"}) {
		t.Error("a single string is a group")
	}
	many := make([]any, 300)
	for i := range many {
		many[i] = strings.Repeat("g", 1+i%3) + string(rune('a'+i%26)) + strings.Repeat("x", i/26)
	}
	many[0] = strings.Repeat("y", 300)
	if g := groups(many); len(g) != MaxGroups || g[0] == many[0] {
		t.Errorf("groups capped at %d, long names dropped: %d", MaxGroups, len(g))
	}
}

func TestValidReturn(t *testing.T) {
	for _, s := range []string{"#/", "#/nodes", "#/cli-login?port=4242&state=abc_DEF-1&key=0a1b", "#/cli-device?code=ABCD-EFGH", "#/nodes/n1%20x"} {
		if !ValidReturn(s) {
			t.Errorf("%q refused", s)
		}
	}
	for _, s := range []string{"", "/", "#", "#nodes", "https://evil.example/#/", "//evil.example", "#/\\evil", "#/<script>", "#/a b", "#/\"x", "#/" + strings.Repeat("a", 1001), "javascript:alert(1)", "#/\nx"} {
		if ValidReturn(s) {
			t.Errorf("%q accepted", s)
		}
	}
}
