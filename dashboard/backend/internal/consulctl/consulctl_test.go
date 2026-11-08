package consulctl_test

import (
	"context"
	"encoding/pem"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/swenske/Janus/dashboard/backend/internal/consulctl"
	"github.com/swenske/Janus/dashboard/backend/internal/consulctl/consultest"
)

const controllerToken = "11111111-2222-3333-4444-555555555555"

type plainSealer struct{}

func (plainSealer) Seal(p []byte, purpose string) ([]byte, error) {
	return append([]byte(purpose+":"), p...), nil
}
func (plainSealer) Open(s []byte, purpose string) ([]byte, error) {
	return []byte(strings.TrimPrefix(string(s), purpose+":")), nil
}

func settingsFor(srv *consultest.Server) consulctl.Settings {
	s := consulctl.Defaults()
	s.Enabled, s.Address, s.Token = true, srv.URL, controllerToken
	s.TLS.CAPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	return s
}

func TestSettingsValidate(t *testing.T) {
	s := consulctl.Defaults()
	if errs := s.Validate(); len(errs) > 0 {
		t.Fatalf("the defaults (off): %q", errs)
	}
	s.Enabled = true
	errs := strings.Join(s.Validate(), "\n")
	if !strings.Contains(errs, "address") || !strings.Contains(errs, "token") {
		t.Fatalf("enabled without address or token: %s", errs)
	}
	s.Address, s.Token = "http://consul.example:8500", "nope"
	s.RPCCAPEM, s.Selector = "junk", map[string]string{"Bad Key": "x"}
	errs = strings.Join(s.Validate(), "\n")
	for _, want := range []string{"https://", "a UUID", "rpc_ca_pem", "selector"} {
		if !strings.Contains(errs, want) {
			t.Errorf("no complaint about %s: %s", want, errs)
		}
	}
}

func TestStoreKeepsTheToken(t *testing.T) {
	dir := t.TempDir()
	st, err := consulctl.Open(dir, plainSealer{})
	if err != nil {
		t.Fatal(err)
	}
	if !st.Settings().NodeMetaFromLabels || !st.Settings().Services.URLs {
		t.Fatalf("defaults: %+v", st.Settings())
	}
	s := consulctl.Defaults()
	s.Enabled, s.Address, s.Token = true, "https://consul.example/", controllerToken
	if _, err := st.Set(s); err != nil {
		t.Fatal(err)
	}
	// The token sent empty, the same address: kept. Another address: not.
	s.Token, s.Address = "", "https://consul.example"
	got, err := st.Set(s)
	if err != nil || got.Token != controllerToken || got.Redacted().Token != "" {
		t.Fatalf("kept token: %+v %v", got, err)
	}
	s.Address = "https://other.example"
	if _, err := st.Set(s); err == nil {
		t.Fatal("another server took the saved token")
	}
	if err := st.SetToken("n1", consulctl.NodeToken{NodeName: "edge-1", AccessorID: "acc", SecretID: "sec"}); err != nil {
		t.Fatal(err)
	}
	// Sealed on disk, read back.
	raw, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	if !strings.Contains(string(raw), "sealed") {
		t.Fatalf("state.json: %s", raw)
	}
	again, err := consulctl.Open(dir, plainSealer{})
	if err != nil {
		t.Fatal(err)
	}
	if tok, ok := again.Token("n1"); !ok || tok.SecretID != "sec" || again.Settings().Token != controllerToken {
		t.Fatalf("read back: %+v %v", tok, ok)
	}
	if err := again.MarkRemoved("n1"); err != nil {
		t.Fatal(err)
	}
	if tok, _ := again.Token("n1"); !tok.Removed {
		t.Fatal("not marked removed")
	}
	if err := again.Forget("n1"); err != nil || len(again.Nodes()) != 0 {
		t.Fatalf("forgotten: %v %v", again.Nodes(), err)
	}
}

func TestClient(t *testing.T) {
	srv := consultest.New(t, controllerToken)
	c, err := consulctl.New(settingsFor(srv))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	cl, err := c.Cluster(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cl.Datacenter != "lgs" || !slices.Equal(cl.Servers, []string{"10.1.0.10"}) || cl.GossipKey != srv.GossipKey || !cl.HasGossip {
		t.Fatalf("cluster: %+v", cl)
	}
	// The policy: made once, left alone after.
	id, err := c.EnsurePolicy(ctx)
	if err != nil || id == "" || srv.Policy(consulctl.PolicyName) != consulctl.PolicyRules {
		t.Fatalf("policy: %q %v", id, err)
	}
	if again, err := c.EnsurePolicy(ctx); err != nil || again != id {
		t.Fatalf("policy again: %q %v", again, err)
	}
	// A node's token: its identity, janus-nodes, its scope's policy - and
	// the legacy one while its release doesn't name services by scope.
	if strings.Contains(consulctl.PolicyRules, "haproxy-") {
		t.Fatal("janus-nodes still lets any node write any haproxy-... service")
	}
	if err := c.EnsureScopePolicy(ctx, "kluster-prod"); err != nil || !strings.Contains(srv.Policy("janus-scope-kluster-prod"), `service_prefix "haproxy-kluster-prod-"`) {
		t.Fatalf("scope policy: %v %q", err, srv.Policy("janus-scope-kluster-prod"))
	}
	if err := c.EnsureLegacyPolicy(ctx); err != nil || !strings.Contains(srv.Policy(consulctl.LegacyPolicyName), `service_prefix "haproxy-"`) {
		t.Fatalf("legacy policy: %v", err)
	}
	spec := consulctl.TokenSpec{Description: "janus node edge-1 (n1) - Controller ctl", Node: "edge-1", Datacenter: "lgs", Scope: "kluster-prod", Legacy: true}
	acc, secret, err := c.CreateToken(ctx, spec)
	if err != nil || acc == "" || secret == "" {
		t.Fatal(acc, secret, err)
	}
	tok := srv.TokenFor("edge-1")
	var names []string
	for _, p := range tok.Policies {
		names = append(names, p.Name)
	}
	if tok == nil || tok.NodeIdentities[0].Datacenter != "lgs" || !slices.Equal(names, []string{consulctl.PolicyName, "janus-scope-kluster-prod", consulctl.LegacyPolicyName}) {
		t.Fatalf("token: %+v", tok)
	}
	if ts, err := c.CheckToken(ctx, acc, spec); err != nil || ts != consulctl.TokenOK {
		t.Fatalf("check: %v %v", ts, err)
	}
	// Its release names services by scope now: the legacy policy goes.
	scoped := spec
	scoped.Legacy = false
	if ts, _ := c.CheckToken(ctx, acc, scoped); ts != consulctl.TokenDiffers {
		t.Fatalf("the legacy policy no longer wanted: %v", ts)
	}
	if err := c.UpdateToken(ctx, acc, scoped); err != nil || len(srv.TokenFor("edge-1").Policies) != 2 {
		t.Fatalf("update: %v %+v", err, srv.TokenFor("edge-1"))
	}
	// Another scope (the node joined another cluster): differs too.
	other := scoped
	other.Scope = "edge"
	if ts, _ := c.CheckToken(ctx, acc, other); ts != consulctl.TokenDiffers {
		t.Fatalf("another scope: %v", ts)
	}
	renamed := scoped
	renamed.Description, renamed.Node = "janus node edge-2 (n1) - Controller ctl", "edge-2"
	if ts, _ := c.CheckToken(ctx, acc, renamed); ts != consulctl.TokenDiffers {
		t.Fatalf("renamed: %v", ts)
	}
	if err := c.UpdateToken(ctx, acc, renamed); err != nil || srv.TokenFor("edge-2") == nil || srv.TokenFor("edge-2").SecretID != secret {
		t.Fatalf("update: %v", err)
	}
	// The scope policies no node uses go; the others, and the rest, stay.
	if err := c.EnsureScopePolicy(ctx, "gone"); err != nil {
		t.Fatal(err)
	}
	if err := c.DropScopePolicies(ctx, map[string]bool{"janus-scope-kluster-prod": true}); err != nil {
		t.Fatal(err)
	}
	if got := srv.PolicyNames(); !slices.Equal(got, []string{consulctl.PolicyName, consulctl.LegacyPolicyName, "janus-scope-kluster-prod"}) {
		t.Fatalf("policies after the drop: %v", got)
	}
	if err := c.DeleteToken(ctx, acc); err != nil || srv.TokenCount() != 0 {
		t.Fatalf("delete: %v", err)
	}
	if err := c.DeleteToken(ctx, acc); err != nil {
		t.Fatalf("deleting a token gone: %v", err)
	}
	if ts, err := c.CheckToken(ctx, acc, consulctl.TokenSpec{Description: "x", Node: "edge-2", Datacenter: "lgs"}); err != nil || ts != consulctl.TokenMissing {
		t.Fatalf("a token gone: %v %v", ts, err)
	}
	if err := c.Deregister(ctx, "lgs", "edge-2"); err != nil || !slices.Equal(srv.DeregisteredNodes(), []string{"edge-2"}) {
		t.Fatalf("deregister: %v %v", err, srv.DeregisteredNodes())
	}
	// No gossip encryption: no key, no error.
	srv.SetGossipKey("")
	if cl, err := c.Cluster(ctx); err != nil || cl.HasGossip {
		t.Fatalf("no encryption: %+v %v", cl, err)
	}
}

func TestClientTest(t *testing.T) {
	srv := consultest.New(t, controllerToken)
	s := settingsFor(srv)
	c, _ := consulctl.New(s)
	_, checks := c.Test(context.Background())
	for _, ch := range checks {
		if !ch.OK {
			t.Errorf("%s: %s", ch.Name, ch.Detail)
		}
	}
	if len(checks) < 5 {
		t.Errorf("checks: %+v", checks)
	}
	// Another token: said in words.
	s.Token = "99999999-2222-3333-4444-555555555555"
	c, _ = consulctl.New(s)
	_, checks = c.Test(context.Background())
	if len(checks) != 1 || checks[0].OK || !strings.Contains(checks[0].Detail, "isn't allowed") {
		t.Errorf("a wrong token: %+v", checks)
	}
	// Another CA: the TLS error, not a hang.
	s = settingsFor(srv)
	s.TLS.CAPEM = ""
	c, _ = consulctl.New(s)
	if _, checks = c.Test(context.Background()); checks[0].OK || !strings.Contains(checks[0].Detail, "certificate") {
		t.Errorf("an untrusted server: %+v", checks)
	}
	if _, err := consulctl.New(consulctl.Settings{Address: "http://consul.example:8500"}); err == nil {
		t.Error("plain HTTP accepted")
	}
}

// TestMergeSecretsTrust: the token is reused only when the address and
// the TLS trust are both unchanged (security review 2026-11-05 #19).
func TestMergeSecretsTrust(t *testing.T) {
	saved := consulctl.Settings{Address: "https://consul", Token: "tok", TLS: consulctl.Trust{Fingerprint: "AA"}}
	s := consulctl.Settings{Address: "https://consul", TLS: consulctl.Trust{Fingerprint: "AA"}}
	s.MergeSecrets(saved)
	if s.Token != "tok" {
		t.Error("token dropped though nothing changed")
	}
	s = consulctl.Settings{Address: "https://consul", TLS: consulctl.Trust{Fingerprint: "BB"}}
	s.MergeSecrets(saved)
	if s.Token != "" {
		t.Error("token followed a changed TLS trust")
	}
	s = consulctl.Settings{Address: "https://other", TLS: consulctl.Trust{Fingerprint: "AA"}}
	s.MergeSecrets(saved)
	if s.Token != "" {
		t.Error("token followed a changed address")
	}
}
