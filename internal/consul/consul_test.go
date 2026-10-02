package consul

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/swenske/Janus/internal/extensions"
	"github.com/swenske/Janus/internal/modcfg"
)

// fakeServices records what Manager asks of the supervisor.
type fakeServices struct {
	state string
	calls []string
}

func (f *fakeServices) StartService(id string) error { f.calls = append(f.calls, "start"); return nil }
func (f *fakeServices) StopService(id string) error  { f.calls = append(f.calls, "stop"); return nil }
func (f *fakeServices) RestartService(id string) error {
	f.calls = append(f.calls, "restart")
	return nil
}
func (f *fakeServices) State(id string) (extensions.ServiceState, error) {
	return extensions.ServiceState{ID: id, State: f.state}, nil
}

// setup points the package at temporary directories and the real consul
// (make extension-consul-amd64), or skips.
func setup(t *testing.T) (*Manager, *fakeServices) {
	t.Helper()
	bin, _ := filepath.Abs("../../build/extensions/tree-consul-amd64/usr/local/sbin/consul")
	if _, err := os.Stat(bin); err != nil {
		t.Skip("consul not built (make extension-consul-amd64)")
	}
	oldBin, oldRun, oldDir := Binary, RunDir, modcfg.Dir
	Binary, RunDir, modcfg.Dir = bin, filepath.Join(t.TempDir(), "run"), t.TempDir()
	t.Cleanup(func() { Binary, RunDir, modcfg.Dir = oldBin, oldRun, oldDir })
	svc := &fakeServices{state: "waiting"}
	m := New(svc)
	if err := m.Boot(); err != nil {
		t.Fatal(err)
	}
	return m, svc
}

const goodConfig = `datacenter = "dc1"
bind_addr  = "127.0.0.1"
retry_join = ["10.0.0.1"]
tls {
  defaults {
    ca_file = "/run/janus/consul/files/ca.pem"
  }
}
`

func TestCheckWithConsul(t *testing.T) {
	m, _ := setup(t)
	errs, err := m.Check("datacenter = \"dc1\"\nbind_addr = \"127.0.0.1\"\nbogus_key = 1\n", nil)
	if err == nil || !strings.Contains(strings.Join(errs, "\n"), "invalid config key bogus_key") {
		t.Errorf("a bad key: %q %v", errs, err)
	}
	if errs, err := m.Check(goodConfig, nil); err != nil {
		t.Errorf("a good configuration: %q %v", errs, err)
	}
	// JSON is fine too, in the same file.
	if errs, err := m.Check(`{"datacenter": "dc1", "bind_addr": "127.0.0.1"}`, nil); err != nil {
		t.Errorf("JSON: %q %v", errs, err)
	}
	if errs, _ := m.Check(goodConfig, map[string][]byte{"../x": []byte("a")}); len(errs) == 0 {
		t.Error("a file name with a path accepted")
	}
}

func TestApplyFilesAndNodeID(t *testing.T) {
	m, svc := setup(t)
	fragment, err := os.ReadFile(filepath.Join(RunDir, fragmentFile))
	if err != nil {
		t.Fatal(err)
	}
	id, _ := nodeID()
	if !uuidPattern.MatchString(id) || !strings.Contains(string(fragment), id) || !strings.Contains(string(fragment), filepath.Join(RunDir, "data")) {
		t.Fatalf("fragment %s (node ID %q)", fragment, id)
	}

	files := map[string][]byte{"ca.pem": []byte("CA"), "client.key": []byte("KEY")}
	if errs, err := m.Apply(goodConfig, files); err != nil {
		t.Fatal(errs, err)
	}
	if got, _ := os.ReadFile(filepath.Join(FilesDir(), "client.key")); string(got) != "KEY" {
		t.Errorf("the agent's file: %q", got)
	}
	if fi, err := os.Stat(filepath.Join(modcfg.Dir, savedFilesDir, "client.key")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("saved file: %v %v", fi, err)
	}
	if !slices.Equal(svc.calls, nil) {
		t.Errorf("a waiting agent was %v - it starts on the file by itself", svc.calls)
	}

	// Empty keeps the saved file, a file not given goes.
	svc.state = "running"
	if errs, err := m.Apply(goodConfig, map[string][]byte{"ca.pem": nil}); err != nil {
		t.Fatal(errs, err)
	}
	if got, _ := os.ReadFile(filepath.Join(FilesDir(), "ca.pem")); string(got) != "CA" {
		t.Errorf("ca.pem not kept: %q", got)
	}
	if _, err := os.Stat(filepath.Join(FilesDir(), "client.key")); !os.IsNotExist(err) {
		t.Error("client.key not removed")
	}
	_, _, names, _ := m.Saved()
	if !slices.Equal(names, []string{"ca.pem"}) {
		t.Errorf("saved files %v", names)
	}
	if !slices.Equal(svc.calls, []string{"restart"}) {
		t.Errorf("a running agent: %v, want a restart", svc.calls)
	}
	if _, err := m.Apply(goodConfig, map[string][]byte{"nope.pem": nil}); err == nil {
		t.Error("keeping a file that isn't saved accepted")
	}

	// The node ID survives a reboot (a new run directory).
	RunDir = filepath.Join(t.TempDir(), "run")
	if err := m.Boot(); err != nil {
		t.Fatal(err)
	}
	if again, _ := nodeID(); again != id {
		t.Errorf("node ID %s after a reboot, was %s", again, id)
	}
	if got, _ := os.ReadFile(filepath.Join(FilesDir(), "ca.pem")); string(got) != "CA" {
		t.Errorf("files not back after a reboot: %q", got)
	}

	// Empty: everything removed, the agent stopped.
	svc.calls = nil
	if _, err := m.Apply("", nil); err != nil {
		t.Fatal(err)
	}
	if _, isDefault, names, _ := m.Saved(); !isDefault || len(names) != 0 {
		t.Errorf("after removal: default %v, files %v", isDefault, names)
	}
	if !slices.Equal(svc.calls, []string{"stop"}) {
		t.Errorf("removal: %v, want a stop", svc.calls)
	}
}

func TestAgent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/agent/self":
			_, _ = w.Write([]byte(`{"Config":{"Datacenter":"janus","NodeName":"node-b","NodeID":"6a2b6f3e-1c6e-4b9a-9b0e-5f6d2a1b3c4d","Server":false,"Version":"2.0.4"}}`))
		case "/v1/status/leader":
			_, _ = w.Write([]byte(`"192.168.79.1:8300"`))
		case "/v1/agent/members":
			_, _ = w.Write([]byte(`[{"Name":"node-b","Addr":"192.168.79.2","Port":8301,"Status":1,"Tags":{"role":"node","dc":"janus"}},
				{"Name":"node-a","Addr":"192.168.79.1","Port":8301,"Status":4,"Tags":{"role":"consul","dc":"janus"}}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	old := HTTPAddr
	HTTPAddr = strings.TrimPrefix(srv.URL, "http://")
	defer func() { HTTPAddr = old }()
	a, err := (&Manager{}).Agent(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if a.NodeName != "node-b" || a.Server || a.Datacenter != "janus" || a.Leader != "192.168.79.1:8300" || a.Version != "2.0.4" {
		t.Errorf("agent %+v", a)
	}
	want := []Member{
		{Name: "node-a", Address: "192.168.79.1:8301", Status: "failed", Role: "server", Datacenter: "janus"},
		{Name: "node-b", Address: "192.168.79.2:8301", Status: "alive", Role: "client", Datacenter: "janus"},
	}
	if !slices.Equal(a.Members, want) {
		t.Errorf("members %+v", a.Members)
	}

	forbidden := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Permission denied", http.StatusForbidden)
	}))
	defer forbidden.Close()
	HTTPAddr = strings.TrimPrefix(forbidden.URL, "http://")
	if _, err := (&Manager{}).Agent(context.Background()); err == nil || !strings.Contains(err.Error(), "ACLs") {
		t.Errorf("ACL denial: %v", err)
	}
}
