package consul

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAgent is the agent's HTTP API as the registrar uses it: services,
// their checks, maintenance - refusing any other token than its own.
type fakeAgent struct {
	token string

	mu       sync.Mutex
	services map[string]agentService
	checks   map[string]agentCheck
	tokens   map[string]string // the token each service was registered with
	calls    []string
}

func newFakeAgent(t *testing.T, token string) (*fakeAgent, string) {
	t.Helper()
	a := &fakeAgent{token: token, services: map[string]agentService{}, checks: map[string]agentCheck{}, tokens: map[string]string{}}
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return a, u.Host
}

func (a *fakeAgent) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r.Header.Get("X-Consul-Token") != a.token {
		http.Error(w, "Permission denied", http.StatusForbidden)
		return
	}
	path := r.URL.Path
	if r.Method != http.MethodGet {
		a.calls = append(a.calls, r.Method+" "+path)
	}
	switch {
	case r.Method == http.MethodGet && path == "/v1/agent/services":
		_ = json.NewEncoder(w).Encode(a.services)
	case r.Method == http.MethodGet && path == "/v1/agent/checks":
		_ = json.NewEncoder(w).Encode(a.checks)
	case r.Method == http.MethodPut && path == "/v1/agent/service/register":
		var s Service
		if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		a.services[s.ID] = agentService{ID: s.ID, Service: s.Name, Tags: orEmpty(s.Tags), Meta: s.Meta, Address: s.Address, Port: s.Port}
		a.tokens[s.ID] = r.Header.Get("X-Consul-Token")
		if r.URL.Query().Get("replace-existing-checks") == "true" {
			delete(a.checks, maintenanceCheck(s.ID)) // as Consul does
		}
		if s.Check != nil {
			a.checks["service:"+s.ID] = agentCheck{CheckID: "service:" + s.ID, ServiceID: s.ID, Status: "passing"}
		}
	case r.Method == http.MethodPut && strings.HasPrefix(path, "/v1/agent/service/deregister/"):
		id := strings.TrimPrefix(path, "/v1/agent/service/deregister/")
		delete(a.services, id)
		for k, c := range a.checks {
			if c.ServiceID == id {
				delete(a.checks, k)
			}
		}
	case r.Method == http.MethodPut && strings.HasPrefix(path, "/v1/agent/service/maintenance/"):
		id := strings.TrimPrefix(path, "/v1/agent/service/maintenance/")
		if _, ok := a.services[id]; !ok {
			http.Error(w, "Unknown service ID", http.StatusNotFound)
			return
		}
		if r.URL.Query().Get("enable") == "true" {
			a.checks[maintenanceCheck(id)] = agentCheck{CheckID: maintenanceCheck(id), ServiceID: id, Status: "critical"}
		} else {
			delete(a.checks, maintenanceCheck(id))
		}
	default:
		http.NotFound(w, r)
	}
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func (a *fakeAgent) ids() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var ids []string
	for id := range a.services {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func (a *fakeAgent) inMaintenance(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.checks[maintenanceCheck(id)]
	return ok
}

func (a *fakeAgent) takeCalls() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.calls
	a.calls = nil
	return c
}

// registrarFor is a registrar on a saved layer and a fake agent.
func registrarFor(t *testing.T, toggles Toggles) (*Registrar, *fakeAgent, *Manager, *sync.Mutex, *string, *string) {
	t.Helper()
	m, _ := setupFake(t)
	layer := goodManaged()
	layer.Services = toggles
	if _, _, err := m.SetManaged(layer, false, false); err != nil {
		t.Fatal(err)
	}
	agent, addr := newFakeAgent(t, layer.AgentToken)
	var mu sync.Mutex
	config, maint := sitesConfig, ""
	r := &Registrar{Manager: m, addr: addr,
		Sources: func() (Sources, error) {
			mu.Lock()
			defer mu.Unlock()
			src := sitesSources()
			src.HAProxyConfig = config
			return src, nil
		},
		Maintenance: func() string { mu.Lock(); defer mu.Unlock(); return maint },
	}
	return r, agent, m, &mu, &config, &maint
}

func TestRegistrarReconciles(t *testing.T) {
	r, agent, _, mu, config, maint := registrarFor(t, Toggles{Janus: true, Frontends: true, URLs: true})
	// The operator's own service, registered by consul.hcl: never touched.
	agent.mu.Lock()
	agent.services["web"] = agentService{ID: "web", Service: "web", Port: 8080}
	agent.mu.Unlock()
	ctx := context.Background()
	if err := r.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	ids := agent.ids()
	if !slices.Contains(ids, "janus:janus") || !slices.Contains(ids, "janus:url:fe_https:www.example.com") || !slices.Contains(ids, "web") || slices.Contains(ids, "janus:janus-exporter") {
		t.Fatalf("after a first sync: %q", ids)
	}
	st := r.Status()
	if st.Err != "" || st.Registered != len(ids)-1 || st.At.IsZero() || st.Maintenance {
		t.Fatalf("status %+v (%d services)", st, len(ids))
	}
	agent.takeCalls()

	// Nothing changed: nothing sent.
	if err := r.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if calls := agent.takeCalls(); len(calls) > 0 {
		t.Fatalf("an unchanged sync sent %q", calls)
	}

	// The configuration loses fe_vars, someone deregistered janus:janus
	// by hand: the one goes, the other comes back - anti-entropy.
	mu.Lock()
	*config = strings.Replace(*config, "frontend fe_vars", "backend fe_vars_gone", 1)
	mu.Unlock()
	agent.mu.Lock()
	delete(agent.services, "janus:janus")
	agent.mu.Unlock()
	if err := r.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	ids = agent.ids()
	if slices.Contains(ids, "janus:haproxy:fe_vars") || slices.Contains(ids, "janus:url:fe_vars:app.example.com") || !slices.Contains(ids, "janus:janus") || !slices.Contains(ids, "web") {
		t.Fatalf("after a change: %q", ids)
	}
	calls := agent.takeCalls()
	if !slices.Contains(calls, "PUT /v1/agent/service/deregister/janus:haproxy:fe_vars") || !slices.Contains(calls, "PUT /v1/agent/service/register") {
		t.Fatalf("calls: %q", calls)
	}
	for _, c := range calls {
		if strings.Contains(c, "/web") {
			t.Fatalf("the operator's service was touched: %q", c)
		}
	}

	// Drained: every janus:* service in maintenance, the operator's not.
	mu.Lock()
	*maint = "drained (cluster member)"
	mu.Unlock()
	if err := r.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range agent.ids() {
		if agent.inMaintenance(id) != strings.HasPrefix(id, IDPrefix) {
			t.Errorf("%s: maintenance %v", id, agent.inMaintenance(id))
		}
	}
	if !r.Status().Maintenance {
		t.Error("status says not in maintenance")
	}
	mu.Lock()
	*maint = ""
	mu.Unlock()
	if err := r.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if agent.inMaintenance("janus:janus") {
		t.Error("still in maintenance after the undrain")
	}

	// Going down: maintenance at once, and it stays.
	r.GoingDown("the node is rebooting")
	if !agent.inMaintenance("janus:janus") || agent.inMaintenance("web") {
		t.Fatal("not in maintenance going down")
	}
	if err := r.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if !agent.inMaintenance("janus:janus") {
		t.Fatal("a sync took the maintenance off a node going down")
	}

	// The layer goes: its services first.
	if err := r.Forget(ctx); err != nil {
		t.Fatal(err)
	}
	if ids := agent.ids(); !slices.Equal(ids, []string{"web"}) {
		t.Fatalf("after Forget: %q", ids)
	}
}

// HAProxy stopped: its maps can't be read (the stats socket is gone),
// so what the node serves can't be either - the services registered
// still go in maintenance; once it serves again, they come out. The same
// failure while in service is an error, and nothing changes.
func TestRegistrarMaintenanceWithoutSources(t *testing.T) {
	r, agent, _, mu, _, maint := registrarFor(t, Toggles{Janus: true, Frontends: true, URLs: true})
	stopped := false
	r.Sources = func() (Sources, error) {
		mu.Lock()
		defer mu.Unlock()
		src := sitesSources()
		if stopped {
			src.Map = func(string) (map[string]string, error) {
				return nil, errors.New("dial stats socket: connection refused")
			}
		}
		return src, nil
	}
	ctx := context.Background()
	if err := r.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	ids := agent.ids()
	if !slices.Contains(ids, "janus:url:fe_https:www.example.com") {
		t.Fatalf("after a first sync: %q", ids)
	}

	// The maps can't be read, the node in service: an error, nothing changes.
	mu.Lock()
	stopped = true
	mu.Unlock()
	agent.takeCalls()
	if err := r.Sync(ctx); err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("sync without the maps, in service: %v", err)
	}
	if calls := agent.takeCalls(); len(calls) > 0 {
		t.Fatalf("a failed sync sent %q", calls)
	}

	// HAProxy stopped: every janus:* service in maintenance.
	mu.Lock()
	*maint = "HAProxy is stopped"
	mu.Unlock()
	if err := r.Sync(ctx); err != nil {
		t.Fatalf("sync without the maps, out of service: %v", err)
	}
	for _, id := range ids {
		if !agent.inMaintenance(id) {
			t.Errorf("%s not in maintenance", id)
		}
	}
	if st := r.Status(); !st.Maintenance || st.Registered != len(ids) || st.Err != "" {
		t.Errorf("status %+v (%d services)", st, len(ids))
	}
	if !slices.Equal(agent.ids(), ids) {
		t.Errorf("services changed: %q, were %q", agent.ids(), ids)
	}

	// Started again: read, out of maintenance.
	mu.Lock()
	stopped, *maint = false, ""
	mu.Unlock()
	if err := r.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if agent.inMaintenance(id) {
			t.Errorf("%s still in maintenance", id)
		}
	}
}

// Another token: every service registered again with it - the agent
// keeps each one's token for its own syncs with the servers.
func TestRegistrarNewToken(t *testing.T) {
	r, agent, m, _, _, _ := registrarFor(t, Toggles{Janus: true})
	ctx := context.Background()
	if err := r.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	layer, _ := m.Managed()
	layer.AgentToken = "9c0b8a3e-5c1d-4f5e-8a9b-1c2d3e4f5a6b"
	if _, _, err := m.SetManaged(layer, false, false); err != nil {
		t.Fatal(err)
	}
	agent.mu.Lock()
	agent.token = layer.AgentToken
	agent.mu.Unlock()
	agent.takeCalls()
	if err := r.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if agent.tokens["janus:janus"] != layer.AgentToken {
		t.Fatalf("registered with %q", agent.tokens["janus:janus"])
	}
}

// Without a layer, nothing is asked of the agent.
func TestRegistrarWithoutLayer(t *testing.T) {
	setupFake(t)
	agent, addr := newFakeAgent(t, "x")
	r := &Registrar{addr: addr, Sources: func() (Sources, error) { t.Fatal("sources read without a layer"); return Sources{}, nil }}
	if err := r.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c := agent.takeCalls(); len(c) > 0 {
		t.Fatalf("calls %q", c)
	}
}

// Run syncs at once, when woken, and again after a failure.
func TestRegistrarRun(t *testing.T) {
	r, agent, _, mu, config, _ := registrarFor(t, Toggles{Frontends: true})
	wake := make(chan struct{})
	var wakeMu sync.Mutex
	r.Wake = func() []<-chan struct{} { wakeMu.Lock(); defer wakeMu.Unlock(); return []<-chan struct{}{wake} }
	r.Every, r.Retry = time.Hour, 50*time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("never: %s (%q)", what, agent.ids())
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitFor("the first sync", func() bool { return slices.Contains(agent.ids(), "janus:haproxy:fe_https") })
	mu.Lock()
	*config += "\nfrontend fe_new\n    bind :9000\n"
	mu.Unlock()
	wakeMu.Lock()
	close(wake)
	wake = make(chan struct{})
	wakeMu.Unlock()
	waitFor("a sync when woken", func() bool { return slices.Contains(agent.ids(), "janus:haproxy:fe_new") })
	// The agent refuses for a moment (restarting): retried soon after.
	agent.mu.Lock()
	good := agent.token
	agent.token = "refuse"
	agent.mu.Unlock()
	mu.Lock()
	*config += "\nfrontend fe_newer\n    bind :9001\n"
	mu.Unlock()
	r.Poke()
	waitFor("a failed sync", func() bool { return r.Status().Err != "" })
	agent.mu.Lock()
	agent.token = good
	agent.mu.Unlock()
	waitFor("the retry", func() bool { return slices.Contains(agent.ids(), "janus:haproxy:fe_newer") && r.Status().Err == "" })
}
