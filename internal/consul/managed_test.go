package consul

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/swenske/Janus/internal/modcfg"
)

// setupFake points the package at temporary directories and a stand-in
// consul whose validate refuses a fragment saying "refuse-me" - what
// runs everywhere, CI included; setup is the real consul's.
func setupFake(t *testing.T) (*Manager, *fakeServices) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "consul")
	script := "#!/bin/sh\n[ \"$1\" = validate ] || exit 2\nshift\nfor f; do if grep -q refuse-me \"$f\"; then echo \"Config validation failed: $f: refused\"; exit 1; fi; done\necho 'Configuration is valid!'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	oldBin, oldRun, oldDir := Binary, RunDir, modcfg.Dir
	Binary, RunDir, modcfg.Dir = bin, filepath.Join(t.TempDir(), "run"), t.TempDir()
	t.Cleanup(func() { Binary, RunDir, modcfg.Dir = oldBin, oldRun, oldDir })
	svc := &fakeServices{state: "waiting"}
	m := New(svc)
	m.Main = func() string { return "192.0.2.5" }
	if err := m.Boot(); err != nil {
		t.Fatal(err)
	}
	return m, svc
}

// testCA is a throwaway CA (its key thrown away).
const testCA = `-----BEGIN CERTIFICATE-----
MIIBiDCCAS+gAwIBAgIUGyZlUSiZ0ajC8CpQj4+ora3s7gswCgYIKoZIzj0EAwIw
GTEXMBUGA1UEAwwOY29uc3VsIHRlc3QgQ0EwIBcNMjYxMDI5MDY1ODE0WhgPMjEy
NjEwMDUwNjU4MTRaMBkxFzAVBgNVBAMMDmNvbnN1bCB0ZXN0IENBMFkwEwYHKoZI
zj0CAQYIKoZIzj0DAQcDQgAEV/RexR5p0K0oOLPmr75MVVz85PBY6+iBuf0Kj7w/
D0rASne8af2uCn+L7u+WuYVtUBLyZZOsg7XVFqz3bCPWNaNTMFEwHQYDVR0OBBYE
FA9GZCGVOjlWFR9WiI6LnVqEhfB4MB8GA1UdIwQYMBaAFA9GZCGVOjlWFR9WiI6L
nVqEhfB4MA8GA1UdEwEB/wQFMAMBAf8wCgYIKoZIzj0EAwIDRwAwRAIgGAmuPHWA
4s/OYyTpzHtTJjZrRtfUXkrdte5X8fNIbMcCIDn0fvxqadstVXemmsJIvbbO9aJP
G5PVxGAfu3TESL+t
-----END CERTIFICATE-----
`

func goodManaged() *Managed {
	return &Managed{
		ManagedBy: "janus-controller 01234567 consul abcd", Datacenter: "lgs", NodeName: "edge-1",
		RetryJoin: []string{"10.1.0.10"}, Encrypt: "pUqJrVyVRj5jsiYEkM/tFQYfWyJIv4s3XkvDwy7Cu5s=",
		AgentToken: "2b0b8a3e-5c1d-4f5e-8a9b-1c2d3e4f5a6b", NodeMeta: map[string]string{"dc": "lgs", "janus_node_id": "n1"},
		Services: Toggles{Janus: true, Exporters: true, Frontends: true, URLs: true},
	}
}

func readFragment(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(RunDir, fragmentFile))
	if err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(filepath.Join(RunDir, fragmentFile))
	if st.Mode().Perm() != 0o600 {
		t.Errorf("janus.json is %v: it holds the token", st.Mode().Perm())
	}
	var f map[string]any
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestManagedValidate(t *testing.T) {
	if errs := goodManaged().Validate(); len(errs) > 0 {
		t.Fatalf("a good layer: %q", errs)
	}
	bad := goodManaged()
	bad.Datacenter, bad.NodeName, bad.RetryJoin = "l g s", "-x", []string{"a b"}
	bad.AdvertiseAddr, bad.Encrypt, bad.AgentToken = "not-an-ip", "short", "secret"
	bad.CAPEM = "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n"
	bad.NodeMeta = map[string]string{"a.b": "x", "consul-network-segment": "y", "long": strings.Repeat("v", 600)}
	errs := strings.Join(bad.Validate(), "\n")
	for _, want := range []string{"datacenter", "node_name", "retry_join", "advertise_addr", "encrypt", "agent_token", "ca_pem", `"a.b"`, "consul- is Consul's", "node_meta long"} {
		if !strings.Contains(errs, want) {
			t.Errorf("no complaint about %s:\n%s", want, errs)
		}
	}
	if errs := (Managed{ManagedBy: "x", Datacenter: "lgs", NodeName: "n", RetryJoin: []string{"s"}}).Validate(); !slices.ContainsFunc(errs, func(e string) bool { return strings.HasPrefix(e, "agent_token") }) {
		t.Errorf("no token accepted: %q", errs)
	}
}

func TestManagedRedacted(t *testing.T) {
	m := goodManaged()
	r := m.Redacted()
	if r.Encrypt != "" || r.AgentToken != "" {
		t.Fatalf("secrets left: %+v", r)
	}
	r.NodeMeta["dc"] = "other"
	if m.NodeMeta["dc"] != "lgs" || m.AgentToken == "" {
		t.Fatal("Redacted changed the original")
	}
}

func TestSetManagedRendersAndRestarts(t *testing.T) {
	m, svc := setupFake(t)
	// Nothing of the operator's, no layer: the agent waits.
	if _, err := os.Stat(filepath.Join(RunDir, "consul.hcl")); !os.IsNotExist(err) {
		t.Fatal("a consul.hcl without any configuration")
	}
	next := goodManaged()
	next.CAPEM = testCA
	errs, restarted, err := m.SetManaged(next, false, false)
	if err != nil || !restarted {
		t.Fatalf("SetManaged = %q %v %v", errs, restarted, err)
	}
	// The agent runs on the layer alone: an empty consul.hcl it waits
	// for, the layer in janusd's fragment.
	if data, err := os.ReadFile(filepath.Join(RunDir, "consul.hcl")); err != nil || !strings.Contains(string(data), "No configuration of the operator's") {
		t.Fatalf("placeholder: %q %v", data, err)
	}
	f := readFragment(t)
	if f["datacenter"] != "lgs" || f["node_name"] != "edge-1" || f["encrypt"] != next.Encrypt || f["advertise_addr"] != "192.0.2.5" || f["bind_addr"] != "192.0.2.5" {
		t.Errorf("fragment: %v", f)
	}
	acl := f["acl"].(map[string]any)
	if acl["enabled"] != true || acl["default_policy"] != "deny" || acl["tokens"].(map[string]any)["agent"] != next.AgentToken {
		t.Errorf("acl: %v", acl)
	}
	tls := f["tls"].(map[string]any)
	if tls["defaults"].(map[string]any)["ca_file"] != managedCAPath() || tls["internal_rpc"].(map[string]any)["verify_server_hostname"] != true {
		t.Errorf("tls: %v", tls)
	}
	if f["auto_encrypt"].(map[string]any)["tls"] != true || f["node_meta"].(map[string]any)["janus_node_id"] != "n1" || f["node_id"] == "" {
		t.Errorf("fragment: %v", f)
	}
	if ca, _ := os.ReadFile(managedCAPath()); string(ca) != testCA {
		t.Errorf("CA file: %q", ca)
	}
	// Saved on STATE, 0600, secrets in.
	st, err := os.Stat(filepath.Join(modcfg.Dir, ManagedFile))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("saved layer: %v %v", st, err)
	}
	if got, _ := m.Managed(); got == nil || got.AgentToken != next.AgentToken || m.Token() != next.AgentToken {
		t.Fatalf("Managed = %+v", got)
	}

	// The same again: nothing the agent reads changed, no restart.
	svc.state, svc.calls = "running", nil
	if _, restarted, err := m.SetManaged(next, false, false); err != nil || restarted || len(svc.calls) > 0 {
		t.Fatalf("unchanged: restarted %v %v, calls %v", restarted, err, svc.calls)
	}
	// Secrets sent empty but kept: the same layer.
	keep := *next
	keep.Encrypt, keep.AgentToken = "", ""
	if _, restarted, err := m.SetManaged(&keep, true, true); err != nil || restarted {
		t.Fatalf("kept secrets: restarted %v %v", restarted, err)
	}
	if readFragment(t)["encrypt"] != next.Encrypt {
		t.Fatal("the kept key is gone")
	}
	// Sent empty, not kept: no gossip key.
	if _, restarted, err := m.SetManaged(&keep, false, true); err != nil || !restarted || !slices.Equal(svc.calls, []string{"restart"}) {
		t.Fatalf("no key: restarted %v %v, calls %v", restarted, err, svc.calls)
	}
	if _, ok := readFragment(t)["encrypt"]; ok {
		t.Fatal("the gossip key is still there")
	}

	// consul validate refuses: nothing changes.
	bad := *next
	bad.NodeMeta = map[string]string{"x": "refuse-me"}
	before, _ := os.ReadFile(filepath.Join(RunDir, fragmentFile))
	if errs, _, err := m.SetManaged(&bad, false, false); err == nil || len(errs) == 0 || !strings.Contains(errs[0], "janus.json: refused") {
		t.Fatalf("refused layer: %q %v", errs, err)
	}
	if after, _ := os.ReadFile(filepath.Join(RunDir, fragmentFile)); string(after) != string(before) {
		t.Fatal("a refused layer was written")
	}
	if left, _ := filepath.Glob(filepath.Join(RunDir, ".check-*")); len(left) > 0 {
		t.Fatalf("check files left: %v", left)
	}

	// The layer goes: no operator configuration, the agent stops.
	svc.calls = nil
	if _, _, err := m.SetManaged(nil, false, false); err != nil || !slices.Equal(svc.calls, []string{"stop"}) {
		t.Fatalf("removed: %v, calls %v", err, svc.calls)
	}
	if _, err := os.Stat(filepath.Join(RunDir, "consul.hcl")); !os.IsNotExist(err) {
		t.Fatal("the placeholder is still there")
	}
	if _, err := os.Stat(managedCAPath()); !os.IsNotExist(err) {
		t.Fatal("the CA is still there")
	}
	if _, ok := readFragment(t)["datacenter"]; ok {
		t.Fatal("the layer is still in the fragment")
	}
	if got, _ := m.Managed(); got != nil {
		t.Fatalf("still saved: %+v", got)
	}
}

// With the operator's configuration, the layer is added to it: removing
// the layer restarts the agent on the operator's alone, and removing the
// operator's keeps the agent on the layer.
func TestManagedWithOperatorConfig(t *testing.T) {
	m, svc := setupFake(t)
	if _, err := m.Apply("datacenter = \"lgs\"\n", nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.SetManaged(goodManaged(), false, false); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(RunDir, "consul.hcl")); string(data) != "datacenter = \"lgs\"\n" {
		t.Fatalf("the operator's configuration was replaced: %q", data)
	}
	svc.state, svc.calls = "running", nil
	if _, err := m.Apply("", nil); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(svc.calls, []string{"restart"}) {
		t.Fatalf("operator's removed, layer kept: calls %v", svc.calls)
	}
	if data, _ := os.ReadFile(filepath.Join(RunDir, "consul.hcl")); !strings.Contains(string(data), "No configuration") {
		t.Fatalf("no placeholder after the operator's went: %q", data)
	}
	// A reboot: the layer is put back, with the placeholder.
	os.RemoveAll(RunDir)
	if err := New(svc).Boot(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(RunDir, "consul.hcl")); err != nil {
		t.Fatal("no placeholder at boot")
	}
	if readFragment(t)["datacenter"] != "lgs" {
		t.Fatal("no layer at boot")
	}
}

func TestSetManagedRealConsul(t *testing.T) {
	m, _ := setup(t)
	m.Main = func() string { return "192.0.2.5" }
	next := goodManaged()
	next.CAPEM = testCA
	if errs, _, err := m.SetManaged(next, false, false); err != nil {
		t.Fatalf("consul validate refused the layer: %q %v", errs, err)
	}
	// A server can't take auto_encrypt.tls: consul validate says so.
	if _, err := m.Apply("server = true\nbootstrap_expect = 1\n", nil); err == nil {
		t.Fatal("a server configuration under a client's managed layer passed")
	}
}
