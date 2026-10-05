package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/swenske/Janus/dashboard/backend/internal/auth"
	"github.com/swenske/Janus/dashboard/backend/internal/machines"
	"github.com/swenske/Janus/dashboard/backend/internal/nodeproxy"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
	"github.com/swenske/Janus/internal/pki"
)

// TestNodePerms: an account's role over everything, its grants on the
// nodes their labels pick, a token's cap, selector and domains on top.
func TestNodePerms(t *testing.T) {
	web := map[string]string{"team": "web", "env": "prod"}
	db := map[string]string{"team": "db"}
	grants := []auth.Grant{{Role: auth.Operator, Selector: map[string]string{"team": "web"}, Domains: []string{"haproxy", "machines"}}}
	wes := principal{User: "wes", Grants: grants}
	if pm := wes.nodePerms(web); len(pm) != 1 || pm[0].role != auth.Operator || !slices.Equal(pm[0].domains, []string{"haproxy", "machines"}) {
		t.Errorf("wes on web: %+v", pm)
	}
	if pm := wes.nodePerms(db); pm != nil {
		t.Errorf("wes on db: %+v", pm)
	}
	reader := principal{User: "rita", Role: auth.Reader, Base: auth.Reader, Grants: grants}
	if pm := reader.nodePerms(web); len(pm) != 2 || pm[0].role != auth.Operator || pm[1].role != auth.Reader || pm[1].domains != nil {
		t.Errorf("a reader with a grant on web: %+v", pm)
	}
	// A token: capped at reader, only haproxy.
	tok := principal{User: "wes", Grants: grants, Cap: auth.Reader, Scope: auth.TokenScope{Domains: []string{"haproxy"}}}
	if pm := tok.nodePerms(web); len(pm) != 1 || pm[0].role != auth.Reader || !slices.Equal(pm[0].domains, []string{"haproxy"}) {
		t.Errorf("a token: %+v", pm)
	}
	// A token whose selector the node doesn't have reaches nothing there.
	admin := principal{User: "root", Base: auth.Admin, Scope: auth.TokenScope{Selector: map[string]string{"team": "db"}}}
	if admin.nodePerms(web) != nil || len(admin.nodePerms(db)) != 1 {
		t.Error("a token's selector")
	}
	// Machines alone: the node is only observed.
	m := principal{User: "m", Grants: []auth.Grant{{Role: auth.Operator, Domains: []string{"machines"}}}}
	u := m.nodeUser(&store.Node{Labels: web})
	if len(u.Perms) != 1 || !slices.Equal(u.Perms[0].Domains, []string{"observe"}) {
		t.Errorf("machines alone: %+v", u.Perms)
	}
}

// TestScopes: an account without a role over everything, with a grant
// on team=web over haproxy and machines - what it sees, opens and does;
// a scoped token of its; labels changed by an admin.
func TestScopes(t *testing.T) {
	a := newAuthApp(t)
	root := a.login(t, "root", "root-password")
	var made struct {
		Password string `json:"password"`
	}
	code, body := a.req(t, "POST", "/api/users", root, map[string]any{"name": "wes", "role": "none",
		"grants": []map[string]any{{"role": "operator", "selector": map[string]string{"team": "web"}, "domains": []string{"haproxy", "machines"}}}})
	if code != http.StatusCreated {
		t.Fatalf("wes: %d %s", code, body)
	}
	_ = json.Unmarshal([]byte(body), &made)
	if err := a.auth.ChangePassword("wes", made.Password, "wes-password"); err != nil {
		t.Fatal(err)
	}
	if code, _ := a.req(t, "POST", "/api/users", root, map[string]any{"name": "bad", "grants": []map[string]any{{"role": "operator", "domains": []string{"root"}}}}); code != http.StatusBadRequest {
		t.Errorf("an unknown domain: %d", code)
	}
	wes := a.login(t, "wes", "wes-password")

	nodeCA, _ := pki.NewCA("node")
	add := func(name string, l map[string]string) *store.Node {
		n := &store.Node{Name: name, Address: "127.0.0.1:1", CACertPEM: nodeCA.CertPEM, Fleet: true, Labels: l}
		if err := a.store.Add(n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	web, db := add("web-1", map[string]string{"team": "web"}), add("db-1", map[string]string{"team": "db"})
	// The Controller's fleet certificate (any: the nodes here aren't
	// reached).
	ca, _ := pki.NewCA("fleet")
	certPEM, keyPEM, _ := ca.Issue(pki.IssueOptions{CommonName: "janus-controller", Roles: []string{pki.RoleController}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	fleetCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	prev := nodeproxy.FleetIdentity
	nodeproxy.FleetIdentity = func() (*tls.Certificate, error) { return &fleetCert, nil }
	t.Cleanup(func() { nodeproxy.FleetIdentity = prev })

	names := func(cred string) []string {
		code, body := a.req(t, "GET", "/api/nodes", cred, nil)
		if code != http.StatusOK {
			t.Fatalf("nodes: %d %s", code, body)
		}
		var ns []nodeView
		_ = json.Unmarshal([]byte(body), &ns)
		var out []string
		for _, n := range ns {
			out = append(out, n.Name)
		}
		slices.Sort(out)
		return out
	}
	if got := names(wes); !slices.Equal(got, []string{"web-1"}) {
		t.Errorf("wes sees %v", got)
	}
	if got := names(root); len(got) != 2 {
		t.Errorf("root sees %v", got)
	}
	for _, path := range []string{"/api/hypervisors", "/api/pending"} {
		if code, body := a.req(t, "GET", path, wes, nil); code != http.StatusOK || body != "[]\n" {
			t.Errorf("wes %s: %d %q", path, code, body)
		}
	}
	for _, path := range []string{"/api/users", "/api/fleet", "/api/backups"} {
		if code, _ := a.req(t, "GET", path, wes, nil); code != http.StatusForbidden {
			t.Errorf("wes %s: %d", path, code)
		}
	}

	// Node pages: web-1's, with what haproxy and observing allow; not db-1's.
	if code, _ := a.req(t, "GET", "/nodes/"+db.ID+"/api/me", wes, nil); code != http.StatusForbidden {
		t.Errorf("wes on db-1's page: %d", code)
	}
	code, body = a.req(t, "GET", "/nodes/"+web.ID+"/api/me", wes, nil)
	if code != http.StatusOK || !strings.Contains(body, `"HAProxyService/ApplyConfig"`) || !strings.Contains(body, `"SystemService/Stats"`) ||
		strings.Contains(body, `"SystemService/Reboot"`) || strings.Contains(body, `"NetworkService/NetworkConfigApply"`) {
		t.Errorf("wes on web-1's page: %d %s", code, body)
	}
	// A reboot is refused by the Controller before it leaves (the node
	// here can't be reached at all).
	if code, body := a.req(t, "POST", "/nodes/"+web.ID+"/api/system/power", wes, map[string]string{"action": "reboot"}); code != http.StatusForbidden || !strings.Contains(body, "may not call SystemService/Reboot") {
		t.Errorf("wes rebooting web-1: %d %s", code, body)
	}

	// Machines: web-1's - its power reaches the check (no VM here: 409);
	// db-1's isn't seen.
	mach := func(n *store.Node) string {
		m := &machines.Machine{Spec: machines.Spec{Name: n.Name}, NodeID: n.ID, Phase: machines.PhaseReady}
		if err := a.machines.Add(m); err != nil {
			t.Fatal(err)
		}
		return m.ID
	}
	mWeb, mDB := mach(web), mach(db)
	power := func(cred, id string) int {
		code, _ := a.req(t, "POST", "/api/machines/"+id+"/power", cred, map[string]string{"action": "reset"})
		return code
	}
	if c := power(wes, mWeb); c != http.StatusConflict {
		t.Errorf("wes powering web-1's machine: %d", c)
	}
	if c := power(wes, mDB); c != http.StatusNotFound {
		t.Errorf("wes powering db-1's machine: %d", c)
	}
	rita := a.login(t, "rita", "rita-password")
	if c := power(rita, mWeb); c != http.StatusForbidden {
		t.Errorf("a reader powering a machine: %d", c)
	}
	olga := a.login(t, "olga", "olga-password")
	if c := power(olga, mDB); c != http.StatusConflict {
		t.Errorf("an operator over everything powering a machine: %d", c)
	}

	// wes's token: haproxy only - no machines; never more than operator.
	if code, _ := a.req(t, "POST", "/api/tokens", wes, map[string]any{"name": "x", "role": "admin"}); code != http.StatusForbidden {
		t.Errorf("a token above the account: %d", code)
	}
	code, body = a.req(t, "POST", "/api/tokens", wes, map[string]any{"name": "terraform", "scope": map[string]any{"domains": []string{"haproxy"}}})
	if code != http.StatusCreated {
		t.Fatalf("wes's token: %d %s", code, body)
	}
	var tok struct {
		Token string `json:"token"`
		Role  string `json:"role"`
	}
	_ = json.Unmarshal([]byte(body), &tok)
	bearer := "Bearer " + tok.Token
	if tok.Role != "operator" {
		t.Errorf("the token's role: %q", tok.Role)
	}
	if got := names(bearer); !slices.Equal(got, []string{"web-1"}) {
		t.Errorf("the token sees %v", got)
	}
	if c := power(bearer, mWeb); c != http.StatusForbidden {
		t.Errorf("the haproxy-only token powering a machine: %d", c)
	}
	// root's token scoped to team=db: no Controller route, db-1 only.
	code, body = a.req(t, "POST", "/api/tokens", root, map[string]any{"name": "db", "scope": map[string]any{"selector": map[string]string{"team": "db"}}})
	if code != http.StatusCreated {
		t.Fatalf("root's token: %d %s", code, body)
	}
	_ = json.Unmarshal([]byte(body), &tok)
	if got := names("Bearer " + tok.Token); !slices.Equal(got, []string{"db-1"}) {
		t.Errorf("root's db token sees %v", got)
	}
	if code, _ := a.req(t, "GET", "/api/users", "Bearer "+tok.Token, nil); code != http.StatusForbidden {
		t.Errorf("a scoped admin token on the accounts: %d", code)
	}

	// Labels: an admin's; wes then reaches db-1 too.
	if code, _ := a.req(t, "PATCH", "/api/nodes/"+db.ID, wes, map[string]any{"labels": map[string]string{"team": "web"}}); code != http.StatusForbidden {
		t.Errorf("wes labelling: %d", code)
	}
	if code, _ := a.req(t, "PATCH", "/api/nodes/"+db.ID, root, map[string]any{"labels": map[string]string{"Team": "web"}}); code != http.StatusBadRequest {
		t.Errorf("a bad label: %d", code)
	}
	if code, body := a.req(t, "PATCH", "/api/nodes/"+db.ID, root, map[string]any{"labels": map[string]string{"team": "web"}}); code != http.StatusOK {
		t.Fatalf("labelling: %d %s", code, body)
	}
	if got := names(wes); len(got) != 2 {
		t.Errorf("wes after the label: %v", got)
	}
}
