package main

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/swenske/Janus/dashboard/backend/internal/acmeorder"
	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/acme"
	"github.com/swenske/Janus/internal/clusters"
)

// acmeCluster is a cluster of lb-1..3, each with the letsencrypt
// extension, lb-1 configured - through a proxy with a password, a DNS
// provider with its token - and ordering; made, not managing Let's
// Encrypt yet.
func acmeCluster(t *testing.T) (*app, *clusterFake, *clusters.Cluster) {
	t.Helper()
	a, f, nodes := clusterApp(t, "lb-1", "lb-2", "lb-3")
	running := janusv1alpha1.ModuleState_MODULE_STATE_RUNNING
	f.mu.Lock()
	for i, n := range []string{"lb-1", "lb-2", "lb-3"} {
		f.nodes[n].acme = &janusv1alpha1.ACMEStatusResponse{State: running, AccountThumbprint: "thumb-" + string(rune('1'+i))}
	}
	f.nodes["lb-1"].acme.Configured = true
	f.nodes["lb-1"].acmeCfg = acmeTestConfig("tok-1", "pw-1")
	f.mu.Unlock()
	c := newCluster(t, a, nodes, "lb-1", "lb-2", "lb-3")
	if err := a.clusters.Create(context.Background(), c.ID, "lb-1", "admin"); err != nil {
		t.Fatal(err)
	}
	c, _ = a.clusters.Store.Get(c.ID)
	return a, f, c
}

func acmeTestConfig(token, password string) *janusv1alpha1.ACMEConfig {
	return &janusv1alpha1.ACMEConfig{
		Account: &janusv1alpha1.ACMEAccount{Directory: "letsencrypt-staging", Email: "ops@example.com", AcceptTerms: true},
		Certificates: []*janusv1alpha1.ACMECertificate{
			{Name: "site", Domains: []string{"example.com"}},
			{Name: "wild", Domains: []string{"*.example.com"}, Challenge: "dns-01", DnsProvider: "gandi"},
		},
		DnsProviders: []*janusv1alpha1.ACMEDNSProvider{{Name: "gandi", Type: "gandiv5", Settings: map[string]string{"GANDIV5_PERSONAL_ACCESS_TOKEN": token}}},
		Path:         &janusv1alpha1.ACMEPath{Mode: janusv1alpha1.ACMEPathMode_ACME_PATH_MODE_PROXY, ProxyUrl: "http://proxy.example.net:3128", ProxyUsername: "acme", ProxyPassword: password},
	}
}

// The cluster's Let's Encrypt configuration (#569): given to the member
// that orders whole, its secrets with it; the cluster's next version
// carries it without them, every member takes it - the secrets pulled
// from the orderer -, locked; a change of orderer keeps the way the
// members reach the CA; drift is seen; secrets alone make no version.
func TestClusterLetsEncryptManaged(t *testing.T) {
	a, f, c := acmeCluster(t)
	ctx := context.Background()
	if c.Manages("acme") {
		t.Fatal("a new cluster manages Let's Encrypt")
	}
	mark := f.mark()
	n, err := a.clusters.ConfigureACME(ctx, c.ID, acmeTestConfig("tok-2", "pw-2"), clusters.ACMEOptions{}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	c, _ = a.clusters.Store.Get(c.ID)
	if n != 2 || c.Desired != 2 || !c.Manages("acme") {
		t.Fatalf("version %d, desired %d, managed %v", n, c.Desired, c.Managed)
	}
	v, _ := a.clusters.Store.Version(c.ID, 2)
	file := string(v.Content[clusters.ACMEFile])
	for _, secret := range []string{"tok-1", "tok-2", "pw-1", "pw-2"} {
		if strings.Contains(file, secret) {
			t.Errorf("the version holds secret %s:\n%s", secret, file)
		}
	}
	if !strings.Contains(file, "ACME_PATH_MODE_PROXY") || !strings.Contains(file, "GANDIV5_PERSONAL_ACCESS_TOKEN") {
		t.Errorf("the version's %s:\n%s", clusters.ACMEFile, file)
	}
	if string(v.Content["haproxy.cfg"]) != "global # lb-1\n" {
		t.Errorf("the rest of version 1 not carried: %v", v.Files)
	}
	got := f.callsSince(mark)
	// The orderer first: checked, then given it whole - before any member
	// takes the version.
	if !inOrder(got, "lb-1 ACMEApply proxy site wild check", "lb-1 ACMEApply proxy site wild", "lb-1 ClusterSet lb-1,lb-2,lb-3", "lb-2 ClusterSet lb-1,lb-2,lb-3") {
		t.Errorf("the orderer first: %v", got)
	}
	for _, name := range []string{"lb-2", "lb-3"} {
		calls := f.callsOf(name, mark)
		// It ran that configuration already, pulled at the cluster's
		// creation, its secrets aside: the apply leaves it - no reload -
		// and the new secrets are pulled after.
		if !inOrder(calls, "ClusterSet lb-1,lb-2,lb-3", "ClusterPull lb-1 acme_config") || slices.Contains(calls, "HAProxyApply") {
			t.Errorf("%s: the version, then the secrets pulled, no reload - got %v", name, calls)
		}
		fm := f.node(clusters.Member{Name: name})
		if p := fm.acmeCfg.GetPath(); p.GetMode() != janusv1alpha1.ACMEPathMode_ACME_PATH_MODE_PROXY || p.GetProxyPassword() != "pw-2" {
			t.Errorf("%s's path: %v", name, p)
		}
		if tok := fm.acmeCfg.GetDnsProviders()[0].GetSettings()["GANDIV5_PERSONAL_ACCESS_TOKEN"]; tok != "tok-2" {
			t.Errorf("%s's token: %q", name, tok)
		}
		if !slices.Contains(fm.set.GetManaged(), "acme") {
			t.Errorf("%s not locked: managed %v", name, fm.set.GetManaged())
		}
		if st := a.clusters.Store.State(c.ID).Members[name]; st.Applied != 2 || st.Hashes["acme"] == "" {
			t.Errorf("%s's state: %+v", name, st)
		}
	}
	if !journalSays(t, a, c.ID, "with its secrets (path.proxy_password, dns_providers[gandi].settings.GANDIV5_PERSONAL_ACCESS_TOKEN)") ||
		!journalSays(t, a, c.ID, "manages Let's Encrypt's configuration from version 2 on") {
		t.Errorf("the journal: %+v", a.clusters.Store.Entries(c.ID, 30))
	}
	// No drift: what each runs is the version's, whatever its secrets.
	for _, m := range c.Members {
		if d, err := a.clusters.Drift(ctx, c, m, a.clusters.Store.State(c.ID).Members[m.Name]); err != nil || len(d) > 0 {
			t.Errorf("%s: drift %v %v", m.Name, d, err)
		}
	}

	// Secrets alone: no version, the others pull them.
	mark = f.mark()
	if n, err := a.clusters.ConfigureACME(ctx, c.ID, acmeTestConfig("tok-3", ""), clusters.ACMEOptions{}, "admin"); err != nil || n != 0 {
		t.Fatalf("secrets alone: version %d, %v", n, err)
	}
	if c, _ = a.clusters.Store.Get(c.ID); c.Desired != 2 {
		t.Errorf("a version made for a secret: %d", c.Desired)
	}
	if got := f.callsSince(mark); !slices.Contains(got, "lb-2 ClusterPull lb-1 acme_config") || slices.ContainsFunc(got, func(s string) bool { return strings.Contains(s, "HAProxyApply") }) {
		t.Errorf("secrets alone: %v", got)
	}
	for _, name := range []string{"lb-1", "lb-3"} {
		fm := f.node(clusters.Member{Name: name})
		if tok := fm.acmeCfg.GetDnsProviders()[0].GetSettings()["GANDIV5_PERSONAL_ACCESS_TOKEN"]; tok != "tok-3" || fm.acmeCfg.GetPath().GetProxyPassword() != "pw-2" {
			t.Errorf("%s: token %q, password %q", name, tok, fm.acmeCfg.GetPath().GetProxyPassword())
		}
	}

	// A member's configuration changed behind the cluster's back: drift.
	f.mu.Lock()
	f.nodes["lb-3"].acmeCfg.Path = nil
	f.mu.Unlock()
	if d, _ := a.clusters.Drift(ctx, c, *c.Member("lb-3"), a.clusters.Store.State(c.ID).Members["lb-3"]); !slices.Equal(d, []string{"acme"}) {
		t.Errorf("lb-3's drift: %v", d)
	}
	f.mu.Lock()
	f.nodes["lb-3"].acmeCfg = proto.Clone(f.nodes["lb-2"].acmeCfg).(*janusv1alpha1.ACMEConfig)
	f.mu.Unlock()

	// lb-2 orders now: told so, it orders through the same proxy, with
	// the password it pulled - nothing armed by hand.
	if _, err := a.clusters.Store.Update(c.ID, func(c *clusters.Cluster) error { c.SecretsFrom = "lb-2"; return nil }); err != nil {
		t.Fatal(err)
	}
	c, _ = a.clusters.Store.Get(c.ID)
	if err := a.clusters.SyncACME(ctx, c, "cluster"); err != nil {
		t.Fatal(err)
	}
	if s := f.node(clusters.Member{Name: "lb-2"}).set; s.GetAcmeOrderer() != "lb-2" {
		t.Errorf("lb-2 not told it orders: %q", s.GetAcmeOrderer())
	}
	if p := f.node(clusters.Member{Name: "lb-2"}).acmeCfg.GetPath(); p.GetMode() != janusv1alpha1.ACMEPathMode_ACME_PATH_MODE_PROXY || p.GetProxyPassword() != "pw-2" {
		t.Errorf("the new orderer's path: %v", p)
	}
	// A renewal on it: the others pull the certificates - not the
	// configuration, the version's.
	f.mu.Lock()
	f.nodes["lb-2"].acme.Certificates = []*janusv1alpha1.ACMECertificateStatus{{Name: "site", State: "valid", Serial: "09"}}
	f.mu.Unlock()
	mark = f.mark()
	if err := a.clusters.SyncACME(ctx, c, "cluster"); err != nil {
		t.Fatal(err)
	}
	if got := f.callsSince(mark); !slices.Contains(got, "lb-1 ClusterPull lb-2 acme_certs") || slices.ContainsFunc(got, func(s string) bool { return strings.Contains(s, "acme_config") }) {
		t.Errorf("a renewal in a managed cluster: %v", got)
	}
}

// A version that drops a certificate haproxy.cfg used, from both: each
// member keeps it until its new haproxy.cfg is applied, then lets it go.
func TestClusterLetsEncryptCertificateGone(t *testing.T) {
	a, f, c := acmeCluster(t)
	ctx := context.Background()
	cfgWith := []byte("global\nfrontend fe\n    bind :443 ssl crt /etc/haproxy/acme/site.pem crt /etc/haproxy/acme/wild.pem\n")
	if _, err := a.clusters.Store.NewVersion(c.ID, "admin", "", map[string][]byte{"haproxy.cfg": cfgWith}); err != nil {
		t.Fatal(err)
	}
	if err := a.clusters.Apply(ctx, c.ID, 2, nil, "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.clusters.ConfigureACME(ctx, c.ID, acmeTestConfig("tok-1", ""), clusters.ACMEOptions{}, "admin"); err != nil {
		t.Fatal(err)
	}
	c, _ = a.clusters.Store.Get(c.ID)
	cfg := acmeTestConfig("", "")
	cfg.Certificates = cfg.Certificates[:1] // wild gone
	canonical, err := acme.Canonical(cfg)
	if err != nil {
		t.Fatal(err)
	}
	v, err := a.clusters.Store.NewVersion(c.ID, "admin", "", map[string][]byte{
		"haproxy.cfg":     []byte("global\nfrontend fe\n    bind :443 ssl crt /etc/haproxy/acme/site.pem\n"),
		clusters.ACMEFile: canonical,
	})
	if err != nil {
		t.Fatal(err)
	}
	mark := f.mark()
	if err := a.clusters.Apply(ctx, c.ID, v.Version, nil, "admin"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"lb-1", "lb-2", "lb-3"} {
		if calls := f.callsOf(name, mark); !inOrder(calls, "ACMEApply proxy site wild", "HAProxyApply", "ACMEApply proxy site") {
			t.Errorf("%s: kept, HAProxy, then gone - got %v", name, calls)
		}
		if certs := f.node(clusters.Member{Name: name}).acmeCfg.GetCertificates(); len(certs) != 1 || certs[0].GetName() != "site" {
			t.Errorf("%s's certificates: %v", name, certs)
		}
	}
	// A version without letsencrypt.json, the cluster managing it: refused.
	bare, _ := a.clusters.Store.NewVersion(c.ID, "admin", "", map[string][]byte{"haproxy.cfg": []byte("global\n")})
	if err := a.clusters.Apply(ctx, c.ID, bare.Version, nil, "admin"); err == nil || !strings.Contains(err.Error(), "has no letsencrypt.json") {
		t.Errorf("a version without Let's Encrypt applied: %v", err)
	}
	// One whose orderer lacks a provider's secret: refused before any
	// member is touched.
	cfg = acmeTestConfig("", "")
	cfg.DnsProviders = append(cfg.DnsProviders, &janusv1alpha1.ACMEDNSProvider{Name: "ovh", Type: "ovh", Settings: map[string]string{"OVH_APPLICATION_KEY": ""}})
	canonical, _ = acme.Canonical(cfg)
	bad, err := a.clusters.Store.NewVersion(c.ID, "admin", "", map[string][]byte{"haproxy.cfg": []byte("global\n"), clusters.ACMEFile: canonical})
	if err != nil {
		t.Fatal(err)
	}
	mark = f.mark()
	if err := a.clusters.Apply(ctx, c.ID, bad.Version, nil, "admin"); err == nil || !strings.Contains(err.Error(), "checked on lb-1") {
		t.Errorf("a provider without its secret: %v", err)
	}
	if slices.ContainsFunc(f.callsSince(mark), func(s string) bool { return strings.Contains(s, "ClusterSet") }) {
		t.Errorf("a member touched: %v", f.callsSince(mark))
	}
}

// The routes: the cluster's Let's Encrypt as the page reads it, a check
// on the orderer, a refused configuration answered at once, an accepted
// one configured in the background.
func TestClusterLetsEncryptRoutes(t *testing.T) {
	a, f, c := acmeCluster(t)
	path := "/api/clusters/" + c.ID + "/letsencrypt"
	rec := call(t, a.handleClusterACME, "GET", path, "GET /api/clusters/{id}/letsencrypt", nil)
	var view clusterACMEView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); rec.Code != 200 || err != nil {
		t.Fatalf("GET: %d %s", rec.Code, rec.Body)
	}
	if view.Managed || view.Orderer != "lb-1" || len(view.Members) != 3 || view.Members[0].PathMode != "proxy" || view.Members[0].State != "running" {
		t.Errorf("the view: %+v", view)
	}
	if strings.Contains(string(view.Config), "tok-1") || strings.Contains(string(view.Config), "pw-1") || !strings.Contains(string(view.Config), "GANDIV5_PERSONAL_ACCESS_TOKEN") {
		t.Errorf("the orderer's configuration as shown: %s", view.Config)
	}
	if !strings.Contains(string(view.Status), `"path_mode":"proxy"`) {
		t.Errorf("the orderer's status: %s", view.Status)
	}
	cfg, _ := acme.Marshal(acmeTestConfig("", ""))
	rec = call(t, a.handleClusterACMECheck, "POST", path+"/check", "POST /api/clusters/{id}/letsencrypt/check", map[string]any{"config": json.RawMessage(cfg)})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"accepted":true`) {
		t.Errorf("check: %d %s", rec.Code, rec.Body)
	}
	// A provider the orderer has no secret for: refused, nothing changed.
	bad := acmeTestConfig("", "")
	bad.DnsProviders = append(bad.DnsProviders, &janusv1alpha1.ACMEDNSProvider{Name: "ovh", Type: "ovh", Settings: map[string]string{"OVH_APPLICATION_KEY": ""}})
	badJSON, _ := acme.Marshal(bad)
	rec = call(t, a.handleClusterACMESet, "PUT", path, "PUT /api/clusters/{id}/letsencrypt", map[string]any{"config": json.RawMessage(badJSON)})
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "OVH_APPLICATION_KEY is empty") {
		t.Errorf("a refused configuration: %d %s", rec.Code, rec.Body)
	}
	if c, _ := a.clusters.Store.Get(c.ID); c.Desired != 1 || c.Manages("acme") {
		t.Errorf("changed by a refused configuration: desired %d, managed %v", c.Desired, c.Managed)
	}
	rec = call(t, a.handleClusterACMESet, "PUT", path, "PUT /api/clusters/{id}/letsencrypt", map[string]any{"config": json.RawMessage(cfg), "note": "LE through the proxy"})
	if rec.Code != 202 {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body)
	}
	a.clusters.wait()
	c, _ = a.clusters.Store.Get(c.ID)
	if c.Error != "" || c.Desired != 2 || !c.Manages("acme") {
		t.Fatalf("configured: desired %d, managed %v, error %q", c.Desired, c.Managed, c.Error)
	}
	if v, _ := a.clusters.Store.Version(c.ID, 2); v.Note != "LE through the proxy" {
		t.Errorf("the version's note: %q", v.Note)
	}
	if p := f.node(clusters.Member{Name: "lb-3"}).acmeCfg.GetPath(); p.GetProxyPassword() != "pw-1" {
		t.Errorf("lb-3's password: %q", p.GetProxyPassword())
	}
	rec = call(t, a.handleClusterACME, "GET", path, "GET /api/clusters/{id}/letsencrypt", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil || !view.Managed {
		t.Errorf("managed now: %s", rec.Body)
	}
}

// The cluster's configuration given again while a member drifted from
// it: no new version, the member takes the cluster's back.
func TestClusterLetsEncryptDriftedBack(t *testing.T) {
	a, f, c := acmeCluster(t)
	ctx := context.Background()
	if _, err := a.clusters.ConfigureACME(ctx, c.ID, acmeTestConfig("tok-1", ""), clusters.ACMEOptions{}, "admin"); err != nil {
		t.Fatal(err)
	}
	c, _ = a.clusters.Store.Get(c.ID)
	f.mu.Lock()
	f.nodes["lb-2"].acmeCfg.Path = nil
	f.mu.Unlock()
	a.clusters.Reconcile(ctx, c)
	if d := a.clusters.Store.State(c.ID).Members["lb-2"].Drift; !slices.Equal(d, []string{"acme"}) {
		t.Fatalf("lb-2's drift: %v", d)
	}
	mark := f.mark()
	if n, err := a.clusters.ConfigureACME(ctx, c.ID, acmeTestConfig("", ""), clusters.ACMEOptions{}, "admin"); err != nil || n != 0 {
		t.Fatalf("version %d, %v", n, err)
	}
	if c, _ := a.clusters.Store.Get(c.ID); c.Desired != 2 {
		t.Errorf("a version made for the same configuration: %d", c.Desired)
	}
	if !slices.Contains(f.callsOf("lb-2", mark), "ACMEApply proxy site wild") {
		t.Errorf("lb-2 didn't take it back: %v", f.callsOf("lb-2", mark))
	}
	if p := f.node(clusters.Member{Name: "lb-2"}).acmeCfg.GetPath(); p.GetMode() != janusv1alpha1.ACMEPathMode_ACME_PATH_MODE_PROXY || p.GetProxyPassword() != "pw-1" {
		t.Errorf("lb-2's path: %v", p)
	}
}

// A member whose release can't lock Let's Encrypt: refused before
// anything is touched, the cluster as it was.
func TestClusterLetsEncryptOldRelease(t *testing.T) {
	a, f, c := acmeCluster(t)
	f.mu.Lock()
	f.nodes["lb-3"].oldRelease = true
	f.mu.Unlock()
	mark := f.mark()
	_, err := a.clusters.ConfigureACME(context.Background(), c.ID, acmeTestConfig("tok-2", ""), clusters.ACMEOptions{}, "admin")
	if err == nil || !strings.Contains(err.Error(), "lb-3's release can't lock Let's Encrypt's configuration") {
		t.Fatalf("an old member: %v", err)
	}
	if c, _ := a.clusters.Store.Get(c.ID); c.Manages("acme") || c.Desired != 1 {
		t.Errorf("changed: managed %v, desired %d", c.Managed, c.Desired)
	}
	if got := f.callsSince(mark); slices.ContainsFunc(got, func(s string) bool { return strings.Contains(s, "ACMEApply") || strings.Contains(s, "ClusterSet") }) {
		t.Errorf("touched: %v", got)
	}
}

// An apply touches only what changed on a member: a Let's Encrypt-only
// version reloads no HAProxy and waits for no table; the same version
// again changes nothing; a file changed reloads HAProxy, which reads it
// as it starts.
func TestClusterApplyOnlyWhatChanged(t *testing.T) {
	a, f, c := acmeCluster(t)
	ctx := context.Background()
	mark := f.mark()
	if _, err := a.clusters.ConfigureACME(ctx, c.ID, acmeTestConfig("tok-1", ""), clusters.ACMEOptions{}, "admin"); err != nil {
		t.Fatal(err)
	}
	got := f.callsSince(mark)
	if slices.ContainsFunc(got, func(s string) bool { return strings.HasSuffix(s, " HAProxyApply") }) {
		t.Errorf("HAProxy applied for a Let's Encrypt change: %v", got)
	}
	// lb-1, the orderer, ran it already (given whole first): nothing; the
	// others took Let's Encrypt's - which may reload HAProxy - and waited.
	if calls := f.callsOf("lb-1", mark); len(slices.DeleteFunc(slices.Clone(calls), func(s string) bool { return s != "ACMEApply proxy site wild" })) != 1 {
		t.Errorf("lb-1 should be given it once, the apply leaving it alone: %v", calls)
	}
	c, _ = a.clusters.Store.Get(c.ID)
	mark = f.mark()
	if err := a.clusters.Apply(ctx, c.ID, c.Desired, nil, "admin"); err != nil {
		t.Fatal(err)
	}
	for _, s := range f.callsSince(mark) {
		if strings.HasSuffix(s, " check") {
			continue // the version checked on the orderer first
		}
		for _, w := range []string{"HAProxyApply", "ACMEApply", "ModuleApply", "FilePut", "ClusterStatus"} {
			if strings.HasSuffix(s, " "+w) || strings.Contains(s, " "+w+" ") {
				t.Errorf("the same version again: %s", s)
			}
		}
	}
	if st := a.clusters.Store.State(c.ID).Members["lb-2"]; st.Applied != c.Desired || st.Hashes["haproxy"] == "" || st.Hashes["acme"] == "" {
		t.Errorf("lb-2's state after an apply that changed nothing: %+v", st)
	}
	v, _ := a.clusters.Store.Version(c.ID, c.Desired)
	files := maps.Clone(v.Content)
	files["files/maps/hosts.map"] = []byte("a b\n")
	v3, err := a.clusters.Store.NewVersion(c.ID, "admin", "", files)
	if err != nil {
		t.Fatal(err)
	}
	mark = f.mark()
	if err := a.clusters.Apply(ctx, c.ID, v3.Version, nil, "admin"); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"lb-1", "lb-2", "lb-3"} {
		if calls := f.callsOf(n, mark); !inOrder(calls, "FilePut maps/hosts.map", "HAProxyApply", "ClusterStatus") || slices.Contains(calls, "ACMEApply proxy site wild") {
			t.Errorf("%s with a file changed: %v", n, calls)
		}
	}
}

// A newer version than the members' waits, not applied: the cluster's
// Let's Encrypt configuration says which it starts from - refused until
// told, then from the members' (the other stays aside) or from the newer
// (its change applied with it).
func TestClusterLetsEncryptUnappliedVersion(t *testing.T) {
	a, f, c := acmeCluster(t)
	ctx := context.Background()
	v2, err := a.clusters.Store.NewVersion(c.ID, "admin", "not yet", map[string][]byte{"haproxy.cfg": []byte("global # v2\n")})
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.clusters.ConfigureACME(ctx, c.ID, acmeTestConfig("", ""), clusters.ACMEOptions{}, "admin")
	var ue *clusters.UnappliedError
	if !errors.As(err, &ue) || ue.Newest != v2.Version || ue.Desired != 1 {
		t.Fatalf("a newer version waiting: %v", err)
	}
	rec := call(t, a.handleClusterACMESet, "PUT", "/api/clusters/"+c.ID+"/letsencrypt", "PUT /api/clusters/{id}/letsencrypt", map[string]any{"config": map[string]any{}})
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), `"unapplied":2`) || !strings.Contains(rec.Body.String(), `"desired":1`) {
		t.Errorf("the route: %d %s", rec.Code, rec.Body)
	}
	if c, _ := a.clusters.Store.Get(c.ID); c.Manages("acme") {
		t.Error("managed after a refusal")
	}
	// From the members' version: v2 stays aside.
	n, err := a.clusters.ConfigureACME(ctx, c.ID, acmeTestConfig("", ""), clusters.ACMEOptions{Base: 1}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	v, _ := a.clusters.Store.Version(c.ID, n)
	if string(v.Content["haproxy.cfg"]) != "global # lb-1\n" || v.Content[clusters.ACMEFile] == nil {
		t.Errorf("from version 1: %s", v.Content["haproxy.cfg"])
	}
	// From the newer one: its change with it.
	if _, err := a.clusters.Store.NewVersion(c.ID, "admin", "later", map[string][]byte{"haproxy.cfg": []byte("global # later\n"), clusters.ACMEFile: v.Content[clusters.ACMEFile]}); err != nil {
		t.Fatal(err)
	}
	vs, _ := a.clusters.Store.Versions(c.ID)
	cfg := acmeTestConfig("", "")
	cfg.Account.Email = "other@example.com"
	n, err = a.clusters.ConfigureACME(ctx, c.ID, cfg, clusters.ACMEOptions{Base: vs[0].Version}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	v, _ = a.clusters.Store.Version(c.ID, n)
	if string(v.Content["haproxy.cfg"]) != "global # later\n" || !strings.Contains(v.Note, "not applied before") {
		t.Errorf("from the newer version: %s, note %q", v.Content["haproxy.cfg"], v.Note)
	}
	if string(f.node(clusters.Member{Name: "lb-3"}).cfg) != "global # later\n" {
		t.Error("the newer version's change not applied with it")
	}
	if _, err := a.clusters.ConfigureACME(ctx, c.ID, cfg, clusters.ACMEOptions{Base: 1}, "admin"); err == nil || !strings.Contains(err.Error(), "older than the one the members run") {
		t.Errorf("a base older than the members': %v", err)
	}
}

// An account key goes to every member - whoever orders uses it, its
// thumbprint told at once -; never into a version or the journal.
func TestClusterLetsEncryptAccountKey(t *testing.T) {
	a, f, c := acmeCluster(t)
	ctx := context.Background()
	keyPEM, err := acme.NewAccountKey()
	if err != nil {
		t.Fatal(err)
	}
	key := string(keyPEM)
	thumb, _ := acme.AccountThumbprint(keyPEM)
	body := strings.Split(strings.TrimSpace(key), "\n")[1] // a line of the key's base64
	if _, err := a.clusters.ConfigureACME(ctx, c.ID, acmeTestConfig("", ""), clusters.ACMEOptions{AccountKey: "not a key"}, "admin"); err == nil || !strings.Contains(err.Error(), "the account key") {
		t.Errorf("a key that doesn't parse: %v", err)
	}
	n, err := a.clusters.ConfigureACME(ctx, c.ID, acmeTestConfig("", ""), clusters.ACMEOptions{AccountKey: key}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"lb-1", "lb-2", "lb-3"} {
		if fm := f.node(clusters.Member{Name: name}); fm.acmeKey != key {
			t.Errorf("%s's account key: %q", name, fm.acmeKey)
		}
	}
	if s := f.node(clusters.Member{Name: "lb-2"}).set; s.GetAcmeThumbprint() != thumb {
		t.Errorf("lb-2 told the thumbprint %q, the key's is %q", s.GetAcmeThumbprint(), thumb)
	}
	// The others answered with the key's account before lb-1 had it - it
	// ordered at once (#660's finding 3).
	if got := f.node(clusters.Member{Name: "lb-1"}).answeredWhenOrders; got["lb-2"] != thumb || got["lb-3"] != thumb {
		t.Errorf("when lb-1 was given the key, the others answered %v - not %s", got, thumb)
	}
	v, _ := a.clusters.Store.Version(c.ID, n)
	for p, b := range v.Content {
		if strings.Contains(string(b), body) {
			t.Errorf("the key in the version's %s", p)
		}
	}
	for _, e := range a.clusters.Store.Entries(c.ID, 100) {
		if strings.Contains(e.Message, body) {
			t.Errorf("the key in the journal: %s", e.Message)
		}
	}
	if !journalSays(t, a, c.ID, "the account key given to every member") {
		t.Errorf("the journal: %+v", a.clusters.Store.Entries(c.ID, 10))
	}
}

// The cluster's first Let's Encrypt configuration, its orderer configured
// for nothing before (v2026.11.17's lab, #660's finding 3): the orderer
// orders what's due as soon as it has the configuration, so the others
// answer HTTP-01 with its account first - a challenge reaching one of
// them is answered. The same with the Controller ordering: its account.
func TestClusterLetsEncryptFirstOrder(t *testing.T) {
	a, f, nodes := clusterApp(t, "lb-1", "lb-2", "lb-3")
	running := janusv1alpha1.ModuleState_MODULE_STATE_RUNNING
	f.mu.Lock()
	for i, n := range []string{"lb-1", "lb-2", "lb-3"} {
		f.nodes[n].acme = &janusv1alpha1.ACMEStatusResponse{State: running, AccountThumbprint: "thumb-" + string(rune('1'+i))}
	}
	f.mu.Unlock()
	c := newCluster(t, a, nodes, "lb-1", "lb-2", "lb-3")
	ctx := context.Background()
	if err := a.clusters.Create(ctx, c.ID, "lb-1", "admin"); err != nil {
		t.Fatal(err)
	}
	if st := a.clusters.Store.State(c.ID).ACME; st.Thumbprint != "" {
		t.Fatalf("an account known before Let's Encrypt is configured: %+v", st)
	}
	if _, err := a.clusters.ConfigureACME(ctx, c.ID, acmeTestConfig("tok", "pw"), clusters.ACMEOptions{}, "admin"); err != nil {
		t.Fatal(err)
	}
	if got := f.node(clusters.Member{Name: "lb-1"}).answeredWhenOrders; got["lb-2"] != "thumb-1" || got["lb-3"] != "thumb-1" {
		t.Errorf("when lb-1 was given its first configuration, the others answered %v - not lb-1's account", got)
	}
	if !journalSays(t, a, c.ID, "the other members answer HTTP-01 with the account lb-1 orders with, before it orders with the new configuration") {
		t.Errorf("the journal: %+v", a.clusters.Store.Entries(c.ID, 20))
	}
	// Set again, the same account: nobody told again.
	mark := f.mark()
	if _, err := a.clusters.ConfigureACME(ctx, c.ID, acmeTestConfig("tok", "pw"), clusters.ACMEOptions{}, "admin"); err != nil {
		t.Fatal(err)
	}
	if got := f.callsSince(mark); inOrder(got, "lb-2 ClusterSet lb-1,lb-2,lb-3", "lb-1 ACMEApply proxy site wild") {
		t.Errorf("told again before the same account: %v", got)
	}

	// Ordered by the Controller: its account, before lb-1 has it.
	ordered := acmeTestConfig("tok", "")
	ordered.Path = &janusv1alpha1.ACMEPath{Mode: janusv1alpha1.ACMEPathMode_ACME_PATH_MODE_CONTROLLER_ORDERED, AccountThumbprint: "ctl-thumb-569"}
	if _, err := a.clusters.ConfigureACME(ctx, c.ID, ordered, clusters.ACMEOptions{}, "admin"); err != nil {
		t.Fatal(err)
	}
	if got := f.node(clusters.Member{Name: "lb-1"}).answeredWhenOrders; got["lb-2"] != "ctl-thumb-569" || got["lb-3"] != "ctl-thumb-569" {
		t.Errorf("ordered by the Controller: the others answered %v when lb-1 had it", got)
	}

	// Back to lb-1's own account: while the Controller ordered for it, lb-1
	// answered with the Controller's - its own isn't known before it has
	// the configuration. Held - it orders nothing - while it's given it,
	// the others told, then let go.
	mark = f.mark()
	if _, err := a.clusters.ConfigureACME(ctx, c.ID, acmeTestConfig("tok", "pw"), clusters.ACMEOptions{}, "admin"); err != nil {
		t.Fatal(err)
	}
	fm := f.node(clusters.Member{Name: "lb-1"})
	if got := fm.answeredWhenOrders; got["lb-2"] != "thumb-1" || got["lb-3"] != "thumb-1" {
		t.Errorf("back to lb-1's own account: when it could order, the others answered %v", got)
	}
	if fm.heldConfig || fm.set.GetAcmeOrderer() != "lb-1" {
		t.Errorf("lb-1 not let go: held %v, told %q orders", fm.heldConfig, fm.set.GetAcmeOrderer())
	}
	if got := f.callsSince(mark); !inOrder(got, "lb-1 ClusterSet lb-1,lb-2,lb-3", "lb-1 ACMEApply proxy site wild", "lb-2 ClusterSet lb-1,lb-2,lb-3", "lb-1 ClusterSet lb-1,lb-2,lb-3") {
		t.Errorf("held, given it, the others told, let go: %v", got)
	}
}

// The way to the CA tried on every member; a renewal through the orderer.
func TestClusterLetsEncryptCheckPathAndRenew(t *testing.T) {
	a, f, c := acmeCluster(t)
	path := "/api/clusters/" + c.ID + "/letsencrypt"
	rec := call(t, a.handleClusterACMECheckPath, "POST", path+"/check-path", "POST /api/clusters/{id}/letsencrypt/check-path", map[string]any{"path": map[string]any{"mode": "ACME_PATH_MODE_PROXY", "proxy_url": "http://unreachable:3128"}})
	var out struct {
		OK      bool `json:"ok"`
		Members []struct {
			Member string `json:"member"`
			Result struct {
				OK    bool   `json:"ok"`
				Error string `json:"error"`
			} `json:"result"`
		} `json:"members"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != 200 || out.OK || len(out.Members) != 3 || out.Members[1].Result.Error != "connection refused" {
		t.Errorf("a proxy none reaches: %d %s", rec.Code, rec.Body)
	}
	if got := f.callsSince(0); !slices.Contains(got, "lb-3 ACMECheckPath proxy") {
		t.Errorf("lb-3 not asked: %v", got)
	}
	rec = call(t, a.handleClusterACMECheckPath, "POST", path+"/check-path", "POST /api/clusters/{id}/letsencrypt/check-path", map[string]any{})
	if !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Errorf("the way set: %s", rec.Body)
	}
	rec = call(t, a.handleClusterACMERenew, "POST", path+"/renew", "POST /api/clusters/{id}/letsencrypt/renew", map[string]any{"names": []string{"site"}})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"site"`) {
		t.Errorf("renew: %d %s", rec.Code, rec.Body)
	}
	if r := f.node(clusters.Member{Name: "lb-1"}).renewed; !slices.Equal(r, []string{"site"}) || f.node(clusters.Member{Name: "lb-2"}).renewed != nil {
		t.Errorf("renewed on lb-1 %v, lb-2 %v", r, f.node(clusters.Member{Name: "lb-2"}).renewed)
	}
}

// Renew on a cluster the Controller orders for: a certificate whose last
// order failed is ordered at once, as "Order what's due now" does - not
// at its next attempt (v2026.11.17's lab, #660's finding 4).
func TestClusterLetsEncryptRenewOrderedNow(t *testing.T) {
	a, _, c := acmeCluster(t)
	st, err := acmeorder.Open(filepath.Join(t.TempDir(), "acme"), testSealer{})
	if err != nil {
		t.Fatal(err)
	}
	a.ordering = &acmeOrdering{store: st, kick: make(chan struct{}, 1), nodes: map[string]orderedNode{}}
	lb1 := c.Member("lb-1").NodeID
	later := time.Now().Add(10 * time.Minute)
	for _, name := range []string{"site", "wild"} {
		if err := st.SetOrder(lb1, name, acmeorder.Order{NextAttempt: later, LastError: "the CA said no"}); err != nil {
			t.Fatal(err)
		}
	}
	rec := call(t, a.handleClusterACMERenew, "POST", "/api/clusters/"+c.ID+"/letsencrypt/renew", "POST /api/clusters/{id}/letsencrypt/renew", map[string]any{"names": []string{"site"}})
	if rec.Code != 200 {
		t.Fatalf("renew: %d %s", rec.Code, rec.Body)
	}
	if o := st.Order(lb1, "site"); !o.NextAttempt.IsZero() || o.LastError == "" {
		t.Errorf("site still waits for its next attempt: %+v", o)
	}
	if o := st.Order(lb1, "wild"); !o.NextAttempt.Equal(later) {
		t.Errorf("wild, not asked, lost its wait: %+v", o)
	}
}
