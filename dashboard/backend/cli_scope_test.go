package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/swenske/Janus/dashboard/backend/internal/auth"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
	"github.com/swenske/Janus/internal/pki"
)

// TestCLIScopedCertificate: janusctl's certificate is plain - a role
// over every node - when the account's permissions are the same on all
// of them, scoped otherwise: what it may do on each node it reaches, by
// the node CA's key; none when it reaches none.
func TestCLIScopedCertificate(t *testing.T) {
	a := newAuthApp(t)
	root := a.login(t, "root", "root-password")
	withReadyFleet(t, a.app)
	webCA, _ := pki.NewCA("web-1")
	dbCA, _ := pki.NewCA("db-1")
	for _, n := range []*store.Node{
		{Name: "web-1", Address: "192.0.2.1:9505", CACertPEM: webCA.CertPEM, Fleet: true, Labels: map[string]string{"team": "web"}},
		{Name: "db-1", Address: "192.0.2.2:9505", CACertPEM: dbCA.CertPEM, Fleet: true, Labels: map[string]string{"team": "db"}},
	} {
		if err := a.store.Add(n); err != nil {
			t.Fatal(err)
		}
	}
	account := func(name, role string, grants ...map[string]any) string {
		t.Helper()
		var made struct {
			Password string `json:"password"`
		}
		code, body := a.req(t, "POST", "/api/users", root, map[string]any{"name": name, "role": role, "grants": grants})
		if code != http.StatusCreated {
			t.Fatalf("%s: %d %s", name, code, body)
		}
		_ = json.Unmarshal([]byte(body), &made)
		if err := a.auth.ChangePassword(name, made.Password, name+"-password"); err != nil {
			t.Fatal(err)
		}
		return a.login(t, name, name+"-password")
	}
	webOps := map[string]any{"role": "operator", "selector": map[string]string{"team": "web"}, "domains": []string{"haproxy", "machines"}}
	wes := account("wes", "none", webOps)
	rita := account("rita2", "reader", webOps)
	nobody := account("nobody", "none")

	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	csr := csrFor(t, key)
	issue := func(cred string) (cliCertificate, *x509.Certificate, *pki.Scope) {
		t.Helper()
		code, out := a.req(t, "POST", "/api/cli/certificate", cred, map[string]string{"csr_pem": csr})
		if code != http.StatusOK {
			t.Fatalf("certificate: %d %s", code, out)
		}
		var c cliCertificate
		_ = json.Unmarshal([]byte(out), &c)
		block, _ := pem.Decode([]byte(c.CertificatePEM))
		leaf, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		s, _, err := pki.ParseScope(leaf)
		if err != nil {
			t.Fatal(err)
		}
		return c, leaf, s
	}
	web, db := pki.CAKey(webCA.Cert), pki.CAKey(dbCA.Cert)
	haproxyOp := []pki.ScopePerm{{Role: pki.RoleOperator, Domains: []string{"haproxy"}}}

	// No role over everything: web-1 only, HAProxy only (machines isn't a
	// node's).
	c, leaf, s := issue(wes)
	if !slices.Equal(leaf.Subject.Organization, []string{pki.RoleScoped}) || s == nil || len(s.Any) != 0 ||
		!slices.EqualFunc(s.For(web), haproxyOp, scopePermEqual) || len(s.For(db)) != 0 {
		t.Errorf("wes: %v %+v", leaf.Subject.Organization, s)
	}
	if c.Role != "scoped: os:operator (haproxy) on 1 node" {
		t.Errorf("wes's summary %q", c.Role)
	}
	code, inv := a.req(t, "GET", "/api/cli/inventory", wes, nil)
	if code != http.StatusOK || !strings.Contains(inv, `"name":"web-1"`) || strings.Contains(inv, `"name":"db-1"`) {
		t.Errorf("wes's inventory: %d %s", code, inv)
	}
	// A reader with that grant: reader everywhere, operator on web-1.
	c, _, s = issue(rita)
	if s == nil || !slices.EqualFunc(s.Any, []pki.ScopePerm{{Role: pki.RoleReader}}, scopePermEqual) || len(s.For(web)) != 2 || len(s.For(db)) != 1 {
		t.Errorf("rita: %+v", s)
	}
	if c.Role != "scoped: os:reader everywhere, os:operator (haproxy) on 1 node" {
		t.Errorf("rita's summary %q", c.Role)
	}
	// An admin: a plain certificate, as before.
	c, leaf, s = issue(root)
	if s != nil || !slices.Equal(leaf.Subject.Organization, []string{pki.RoleAdmin}) || c.Role != "os:admin" {
		t.Errorf("root: %v %+v %q", leaf.Subject.Organization, s, c.Role)
	}
	// An admin's token narrowed to team=db over the system: db-1 only.
	code, out := a.req(t, "POST", "/api/tokens", root, map[string]any{"name": "db-system", "scope": map[string]any{"selector": map[string]string{"team": "db"}, "domains": []string{"system"}}})
	var tok struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(out), &tok); err != nil || code != http.StatusCreated {
		t.Fatalf("token: %d %s", code, out)
	}
	_, _, s = issue("Bearer " + tok.Token)
	if s == nil || len(s.Any) != 0 || len(s.For(web)) != 0 || !slices.EqualFunc(s.For(db), []pki.ScopePerm{{Role: pki.RoleAdmin, Domains: []string{"system"}}}, scopePermEqual) {
		t.Errorf("the scoped token: %+v", s)
	}
	// Reaching no node: no certificate.
	if code, out := a.req(t, "POST", "/api/cli/certificate", nobody, map[string]string{"csr_pem": csr}); code != http.StatusForbidden || !strings.Contains(out, "reaches no node") {
		t.Errorf("nobody: %d %s", code, out)
	}
	if code, inv := a.req(t, "GET", "/api/cli/inventory", nobody, nil); code != http.StatusOK || strings.Contains(inv, "web-1") {
		t.Errorf("nobody's inventory: %d %s", code, inv)
	}

	// An SSH key limited to reader: wes reads web-1, HAProxy only.
	u, _ := a.auth.User("wes")
	id, err := a.cliIdentityFor(userPrincipal(u, auth.Reader))
	if err != nil || id.Scope == nil || !slices.EqualFunc(id.Scope.For(web), []pki.ScopePerm{{Role: pki.RoleReader, Domains: []string{"haproxy"}}}, scopePermEqual) {
		t.Errorf("a reader key of wes: %+v %v", id, err)
	}
	// The page approving janusctl for a lower role: what the account may
	// do, capped; more than its most refused.
	p := userPrincipal(u, auth.None)
	if _, err := approved(p, auth.Admin); err == nil {
		t.Error("wes approved as admin")
	}
	if ap, err := approved(p, auth.Reader); err != nil || ap.Cap != auth.Reader {
		t.Errorf("wes approved as reader: %+v %v", ap, err)
	}
}

func scopePermEqual(a, b pki.ScopePerm) bool {
	return a.Role == b.Role && slices.Equal(a.Domains, b.Domains)
}
