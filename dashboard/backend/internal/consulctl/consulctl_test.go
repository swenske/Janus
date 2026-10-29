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
	// A node's token: its identity and the policy.
	acc, secret, err := c.CreateToken(ctx, "janus node edge-1 (n1) - Controller ctl", "edge-1", "lgs")
	if err != nil || acc == "" || secret == "" {
		t.Fatal(acc, secret, err)
	}
	tok := srv.TokenFor("edge-1")
	if tok == nil || tok.NodeIdentities[0].Datacenter != "lgs" || tok.Policies[0].Name != consulctl.PolicyName {
		t.Fatalf("token: %+v", tok)
	}
	if ts, err := c.CheckToken(ctx, acc, "janus node edge-1 (n1) - Controller ctl", "edge-1", "lgs"); err != nil || ts != consulctl.TokenOK {
		t.Fatalf("check: %v %v", ts, err)
	}
	if ts, _ := c.CheckToken(ctx, acc, "janus node edge-2 (n1) - Controller ctl", "edge-2", "lgs"); ts != consulctl.TokenDiffers {
		t.Fatalf("renamed: %v", ts)
	}
	if err := c.UpdateToken(ctx, acc, "janus node edge-2 (n1) - Controller ctl", "edge-2", "lgs"); err != nil || srv.TokenFor("edge-2") == nil || srv.TokenFor("edge-2").SecretID != secret {
		t.Fatalf("update: %v", err)
	}
	if err := c.DeleteToken(ctx, acc); err != nil || srv.TokenCount() != 0 {
		t.Fatalf("delete: %v", err)
	}
	if err := c.DeleteToken(ctx, acc); err != nil {
		t.Fatalf("deleting a token gone: %v", err)
	}
	if ts, err := c.CheckToken(ctx, acc, "x", "edge-2", "lgs"); err != nil || ts != consulctl.TokenMissing {
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
