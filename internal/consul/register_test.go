package consul

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swenske/Janus/internal/haproxyflow"
	"github.com/swenske/Janus/internal/haproxystat"
)

// fakeAgent is the agent's HTTP API as the registrar uses it: services,
// their checks - a TTL check critical once its TTL passes unfed -,
// maintenance; refusing any other token than its own.
type fakeAgent struct {
	token string
	addr  string

	mu       sync.Mutex
	services map[string]agentService
	checks   map[string]agentCheck
	ttls     map[string]time.Duration // a TTL check's TTL
	fed      map[string]time.Time     // when it was last given a status
	first    map[string]string        // the status it was registered with
	tokens   map[string]string        // the token each service was registered with
	calls    []string
}

func newFakeAgent(t *testing.T, token string) (*fakeAgent, string) {
	t.Helper()
	a := &fakeAgent{token: token, services: map[string]agentService{}, checks: map[string]agentCheck{}, tokens: map[string]string{},
		ttls: map[string]time.Duration{}, fed: map[string]time.Time{}, first: map[string]string{}}
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	a.addr = u.Host
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
		for id, ttl := range a.ttls {
			if c, ok := a.checks[id]; ok && time.Since(a.fed[id]) > ttl {
				c.Status, c.Output = "critical", "TTL expired"
				a.checks[id] = c
			}
		}
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
			id := cmpOr(s.Check.CheckID, "service:"+s.ID)
			status := "passing"
			if s.Check.TTL != "" {
				ttl, err := time.ParseDuration(s.Check.TTL)
				if err != nil || s.Check.Interval != "" || s.Check.TCP != "" {
					http.Error(w, "bad TTL check", http.StatusBadRequest)
					return
				}
				status = cmpOr(s.Check.Status, "critical") // Consul's default
				a.ttls[id], a.fed[id], a.first[id] = ttl, time.Now(), status
			}
			a.checks[id] = agentCheck{CheckID: id, ServiceID: s.ID, Status: status, Notes: s.Check.Notes}
		}
	case r.Method == http.MethodPut && strings.HasPrefix(path, "/v1/agent/service/deregister/"):
		id := strings.TrimPrefix(path, "/v1/agent/service/deregister/")
		delete(a.services, id)
		for k, c := range a.checks {
			if c.ServiceID == id {
				delete(a.checks, k)
				delete(a.ttls, k)
			}
		}
	case r.Method == http.MethodPut && strings.HasPrefix(path, "/v1/agent/check/update/"):
		id := strings.TrimPrefix(path, "/v1/agent/check/update/")
		c, ok := a.checks[id]
		if _, ttl := a.ttls[id]; !ok || !ttl {
			http.Error(w, "Unknown check ID", http.StatusNotFound)
			return
		}
		var u struct{ Status, Output string }
		if err := json.NewDecoder(r.Body).Decode(&u); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		c.Status, c.Output = u.Status, u.Output
		a.checks[id], a.fed[id] = c, time.Now()
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

// registrations are calls but a TTL check's status.
func registrations(calls []string) []string {
	return slices.DeleteFunc(calls, func(c string) bool { return strings.HasPrefix(c, "PUT /v1/agent/check/update/") })
}

// check is a check as the agent has it now.
func (a *fakeAgent) check(t *testing.T, id string) agentCheck {
	t.Helper()
	a.mu.Lock()
	token := a.token
	a.mu.Unlock()
	var all map[string]agentCheck
	if err := (&agentClient{token: token, addr: a.addr}).do(context.Background(), http.MethodGet, "/v1/agent/checks", nil, &all); err != nil {
		t.Fatal(err)
	}
	return all[id]
}

// showStat is show stat's CSV for cfg: a FRONTEND row per frontend or
// listen section, OPEN unless status says, and a backend row named like
// each (which must never count).
func showStat(cfg string, status map[string]string) []byte {
	var b strings.Builder
	b.WriteString("# pxname,svname,qcur,scur,slim,status,type,\n")
	for _, f := range haproxyflow.Parse(cfg).Frontends {
		fmt.Fprintf(&b, "%s,FRONTEND,,3,4000,%s,0,\n", f.Name, cmpOr(status[f.Name], "OPEN"))
		fmt.Fprintf(&b, "%s,BACKEND,0,0,,DOWN,1,\n", f.Name)
	}
	return []byte(b.String())
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
		ShowStat: func() ([]byte, error) { mu.Lock(); defer mu.Unlock(); return showStat(config, nil), nil },
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

	// Nothing changed: nothing registered (the frontends' checks fed).
	if err := r.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if calls := registrations(agent.takeCalls()); len(calls) > 0 {
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
	if calls := registrations(agent.takeCalls()); len(calls) > 0 {
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

// The frontends' checks: HAProxy's own state of each (show stat's
// FRONTEND row), never a connection - registered with it, fed after each
// sync and every TTLEvery; unfed (janusd stopped), they expire critical.
func TestRegistrarFeedsFrontends(t *testing.T) {
	old := FrontendTTL
	FrontendTTL = 300 * time.Millisecond
	t.Cleanup(func() { FrontendTTL = old })
	r, agent, _, mu, config, _ := registrarFor(t, Toggles{Janus: true, Exporters: true, Frontends: true})
	status := map[string]string{"fe_http": "STOP", "fe_vars": "FULL"}
	var statErr error
	r.ShowStat = func() ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		if statErr != nil {
			return nil, statErr
		}
		return showStat(*config, status), nil
	}
	ctx := context.Background()
	if err := r.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]struct{ status, output string }{
		"service:janus:haproxy:fe_https":          {"passing", "OPEN, 3 sessions of 4000"},
		"service:janus:haproxy:fe_http":           {"critical", "STOP, 3 sessions of 4000"},
		"service:janus:haproxy:fe_vars":           {"warning", "FULL, 3 sessions of 4000"},
		"service:janus:haproxy-exporter:fe_stats": {"passing", "OPEN, 3 sessions of 4000"},
		"service:janus:haproxy:raw":               {"passing", "OPEN, 3 sessions of 4000"},
	} {
		c := agent.check(t, id)
		if c.Status != want.status || c.Output != want.output {
			t.Errorf("%s: %s %q, want %s %q", id, c.Status, c.Output, want.status, want.output)
		}
		agent.mu.Lock()
		first := agent.first[id]
		agent.mu.Unlock()
		if first != want.status {
			t.Errorf("%s registered %s, not its state %s", id, first, want.status)
		}
	}
	// janusd's API keeps its TCP check.
	agent.mu.Lock()
	_, ttl := agent.ttls["service:janus:janus"]
	agent.mu.Unlock()
	if c := agent.check(t, "service:janus:janus"); c.Status != "passing" || ttl {
		t.Errorf("janus: %+v, TTL %v", c, ttl)
	}

	// Fed every TTLEvery while janusd runs: a frontend that fills up, then
	// the statistics unreadable, follow - and none expires.
	r.TTLEvery, r.Every = 50*time.Millisecond, time.Hour
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { r.Run(rctx); close(done) }()
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			cancel()
			<-done
		}
	}
	defer stop()
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("never: %s", what)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	mu.Lock()
	status["fe_https"] = "FULL"
	mu.Unlock()
	waitFor("fe_https full", func() bool { return agent.check(t, "service:janus:haproxy:fe_https").Status == "warning" })
	time.Sleep(2 * FrontendTTL)
	if c := agent.check(t, "service:janus:haproxy:raw"); c.Status != "passing" {
		t.Errorf("fed, raw expired: %+v", c)
	}
	mu.Lock()
	statErr = errors.New("dial stats socket: connection refused")
	mu.Unlock()
	waitFor("the statistics unreadable", func() bool {
		c := agent.check(t, "service:janus:haproxy:raw")
		return c.Status == "critical" && strings.Contains(c.Output, "HAProxy's statistics: dial stats socket")
	})
	mu.Lock()
	statErr = nil
	mu.Unlock()
	waitFor("readable again", func() bool { return agent.check(t, "service:janus:haproxy:raw").Status == "passing" })

	// janusd stops: nothing feeds them, they expire critical.
	stop()
	time.Sleep(2 * FrontendTTL)
	if c := agent.check(t, "service:janus:haproxy:fe_https"); c.Status != "critical" || c.Output != "TTL expired" {
		t.Errorf("unfed: %+v", c)
	}
}

func TestFrontendState(t *testing.T) {
	tbl, err := haproxystat.Parse([]byte("# pxname,svname,scur,slim,status,type,\n" +
		"fe_a,FRONTEND,12,4000,OPEN,0,\nfe_a,BACKEND,0,,DOWN,1,\nfe_b,FRONTEND,4000,4000,FULL,0,\n" +
		"fe_c,FRONTEND,0,,STOP,0,\nbe_only,BACKEND,0,,UP,1,\nfe_d,FRONTEND,,,OPEN,0,\n"))
	if err != nil {
		t.Fatal(err)
	}
	for proxy, want := range map[string][2]string{
		"fe_a": {"passing", "OPEN, 12 sessions of 4000"}, "fe_b": {"warning", "FULL, 4000 sessions of 4000"},
		"fe_c": {"critical", "STOP, 0 sessions"}, "fe_d": {"passing", "OPEN"},
		"be_only": {"critical", "HAProxy has no frontend be_only"}, "gone": {"critical", "HAProxy has no frontend gone"},
	} {
		if st, out := frontendState(tbl, proxy); st != want[0] || out != want[1] {
			t.Errorf("%s: %s %q, want %s %q", proxy, st, out, want[0], want[1])
		}
	}
}
