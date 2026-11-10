package main

import (
	"cmp"
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/swenske/Janus/dashboard/backend/internal/consulctl"
	"github.com/swenske/Janus/dashboard/backend/internal/consulctl/consultest"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/clusters"
)

const testConsulToken = "11111111-2222-3333-4444-555555555555"

// fakeConsulNode is a node as ConsulManagedGet/Set see it.
type fakeConsulNode struct {
	notEnabled, old bool
	// dropsScope: a release before scoped names (it drops the layer's
	// scope); saysScope: one that reports what its services are named
	// after (ConsulManagedGetResponse.scope) - cluster its cluster's name.
	dropsScope, saysScope bool
	cluster               string
	managed               *janusv1alpha1.ConsulManaged // what it was set, secrets in
	sets                  int
}

type fakeConsulNodes struct {
	mu    sync.Mutex
	nodes map[string]*fakeConsulNode // by node ID
}

func (f *fakeConsulNodes) node(n *store.Node) *fakeConsulNode {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn, ok := f.nodes[n.ID]
	if !ok {
		fn = &fakeConsulNode{managed: &janusv1alpha1.ConsulManaged{}}
		f.nodes[n.ID] = fn
	}
	return fn
}

func (f *fakeConsulNodes) ManagedGet(_ context.Context, n *store.Node) (*janusv1alpha1.ConsulManagedGetResponse, error) {
	fn := f.node(n)
	f.mu.Lock()
	defer f.mu.Unlock()
	if fn.old {
		return nil, status.Error(codes.Unimplemented, "unknown method ConsulManagedGet")
	}
	if fn.notEnabled {
		return &janusv1alpha1.ConsulManagedGetResponse{State: janusv1alpha1.ModuleState_MODULE_STATE_NOT_ENABLED, Managed: &janusv1alpha1.ConsulManaged{}}, nil
	}
	m := proto.Clone(fn.managed).(*janusv1alpha1.ConsulManaged)
	m.HasEncrypt, m.HasAgentToken, m.Encrypt, m.AgentToken = m.Encrypt != "", m.AgentToken != "", "", ""
	resp := &janusv1alpha1.ConsulManagedGetResponse{State: janusv1alpha1.ModuleState_MODULE_STATE_RUNNING, Managed: m}
	if fn.saysScope && m.GetManagedBy() != "" {
		resp.Scope = cmp.Or(m.GetScope(), fn.cluster, m.GetNodeName()) // consul.ScopeOf
	}
	return resp, nil
}

func (f *fakeConsulNodes) ManagedSet(_ context.Context, n *store.Node, m *janusv1alpha1.ConsulManaged) (*janusv1alpha1.ConsulManagedSetResponse, error) {
	fn := f.node(n)
	f.mu.Lock()
	defer f.mu.Unlock()
	fn.managed = proto.Clone(m).(*janusv1alpha1.ConsulManaged)
	if fn.dropsScope {
		fn.managed.Scope = ""
	}
	fn.sets++
	return &janusv1alpha1.ConsulManagedSetResponse{Accepted: true, Restarted: true}, nil
}

func (f *fakeConsulNodes) get(id string) (m *janusv1alpha1.ConsulManaged, sets int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn := f.nodes[id]
	if fn == nil {
		return &janusv1alpha1.ConsulManaged{}, 0
	}
	return proto.Clone(fn.managed).(*janusv1alpha1.ConsulManaged), fn.sets
}

func consulTestApp(t *testing.T) (*app, *fakeConsulNodes, *consultest.Server) {
	t.Helper()
	a, _ := newTestApp(t)
	st, err := consulctl.Open(t.TempDir(), plainSealer{})
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeConsulNodes{nodes: map[string]*fakeConsulNode{}}
	a.consul = &consulMgmt{store: st, nodes: fake, kick: make(chan struct{}, 1), state: map[string]consulNodeState{}}
	srv := consultest.New(t, testConsulToken)
	return a, fake, srv
}

func enableConsul(t *testing.T, a *app, srv *consultest.Server, selector map[string]string) {
	t.Helper()
	s := consulctl.Defaults()
	s.Enabled, s.Address, s.Token, s.Selector = true, srv.URL, testConsulToken, selector
	s.TLS.CAPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	s.RPCCAPEM = s.TLS.CAPEM
	if _, err := a.consul.store.Set(s); err != nil {
		t.Fatal(err)
	}
}

func addConsulNode(t *testing.T, a *app, name, addr string, l map[string]string) *store.Node {
	t.Helper()
	n := &store.Node{Name: name, Address: addr, Labels: l}
	if err := a.store.Add(n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestConsulPass(t *testing.T) {
	a, nodes, srv := consulTestApp(t)
	ctx := context.Background()
	edge := addConsulNode(t, a, "edge-1", "192.0.2.11:9505", map[string]string{"dc": "lgs", "topology.kubernetes.io/zone": "a"})
	bare := addConsulNode(t, a, "bare", "192.0.2.12:9505", nil)
	nodes.node(bare).notEnabled = true
	old := addConsulNode(t, a, "old", "192.0.2.13:9505", nil)
	nodes.node(old).old = true

	// Off: nothing asked of Consul or given to the nodes.
	a.consulPass(ctx)
	if m, _ := nodes.get(edge.ID); m.GetManagedBy() != "" || srv.TokenCount() != 0 {
		t.Fatal("something done while off")
	}
	if st := a.consul.state[edge.ID]; st.State != "off" {
		t.Fatalf("off: %+v", st)
	}

	enableConsul(t, a, srv, nil)
	if again := a.consulPass(ctx); !again {
		t.Error("an old node, and no retry soon")
	}
	tok := srv.TokenFor("edge-1")
	if tok == nil || tok.Description != "janus node edge-1 ("+edge.ID+") - Controller ctl-test" {
		t.Fatalf("edge-1's token: %+v", tok)
	}
	m, sets := nodes.get(edge.ID)
	if sets != 1 || m.GetDatacenter() != "lgs" || m.GetNodeName() != "edge-1" || !slices.Equal(m.GetRetryJoin(), []string{"10.1.0.10"}) ||
		m.GetEncrypt() != srv.GossipKey || m.GetAgentToken() != tok.SecretID || m.GetAdvertiseAddr() != "192.0.2.11" || m.GetCaPem() == "" ||
		!strings.HasPrefix(m.GetManagedBy(), "janus-controller ctl-test consul ") || !m.GetServices().GetUrls() {
		t.Fatalf("edge-1's layer (%d sets): %v", sets, m)
	}
	wantMeta := map[string]string{"dc": "lgs", "topology_kubernetes_io_zone": "a", "janus_node_id": edge.ID}
	if got := m.GetNodeMeta(); len(got) != len(wantMeta) || got["dc"] != "lgs" || got["topology_kubernetes_io_zone"] != "a" || got["janus_node_id"] != edge.ID {
		t.Fatalf("node meta %v", got)
	}
	if srv.Policy(consulctl.PolicyName) == "" {
		t.Fatal("no janus-nodes policy")
	}
	// Its HAProxy services named after itself (no cluster): the layer
	// says so, its token may write haproxy-edge-1-... - and any haproxy-...
	// until its layer carries the scope back (a release before it).
	if m.GetScope() != "edge-1" || !strings.Contains(srv.Policy("janus-scope-edge-1"), `"haproxy-edge-1-"`) {
		t.Fatalf("scope %q, its policy %q", m.GetScope(), srv.Policy("janus-scope-edge-1"))
	}
	policiesOf := func(node string) []string {
		var names []string
		for _, p := range srv.TokenFor(node).Policies {
			names = append(names, p.Name)
		}
		return names
	}
	if got := policiesOf("edge-1"); !slices.Equal(got, []string{consulctl.PolicyName, "janus-scope-edge-1", consulctl.LegacyPolicyName}) {
		t.Fatalf("the first pass's token: %v", got)
	}
	for id, want := range map[string]string{bare.ID: "no extension", old.ID: "unsupported", edge.ID: "applied"} {
		if st := a.consul.state[id]; st.State != want {
			t.Errorf("%s: %+v, want %s", id, st, want)
		}
	}
	if srv.TokenFor("bare") != nil || srv.TokenFor("old") != nil {
		t.Fatal("a token for a node without the layer")
	}

	// The same again: nothing pushed, no token made - the node carries
	// its scope now, the legacy right goes.
	a.consulPass(ctx)
	if _, sets := nodes.get(edge.ID); sets != 1 || srv.TokenCount() != 1 {
		t.Fatalf("a pass with nothing changed pushed (%d sets, %d tokens)", sets, srv.TokenCount())
	}
	if got := policiesOf("edge-1"); !slices.Equal(got, []string{consulctl.PolicyName, "janus-scope-edge-1"}) {
		t.Fatalf("once the node names its services by scope: %v", got)
	}
	// The member's state and services, from Consul.
	srv.SetMember("edge-1", 1, "janus", "haproxy-edge-1-fe", "haproxy-edge-1-url", "haproxy-edge-1-url")
	a.consulPass(ctx)
	if st := a.consul.state[edge.ID]; st.Member != "alive" || st.Services != 4 || st.URLs != 2 {
		t.Fatalf("member: %+v", st)
	}

	// Its token deleted in Consul: made again, the layer pushed with it.
	srv.DeleteTokenOf("edge-1")
	a.consulPass(ctx)
	tok2 := srv.TokenFor("edge-1")
	if m, sets := nodes.get(edge.ID); tok2 == nil || tok2.SecretID == tok.SecretID || m.GetAgentToken() != tok2.SecretID || sets != 2 {
		t.Fatalf("token again: %+v, layer %v (%d sets)", tok2, m, sets)
	}
	// A new gossip key: pushed.
	srv.SetGossipKey("Hv6ISSlSIpf6yh+ozcm/XG2ye8gcE8c2gsVfLmmH9Fs=")
	a.consulPass(ctx)
	if m, _ := nodes.get(edge.ID); m.GetEncrypt() != "Hv6ISSlSIpf6yh+ozcm/XG2ye8gcE8c2gsVfLmmH9Fs=" {
		t.Fatal("the new gossip key wasn't pushed")
	}

	// Consul unreachable: errors, the layers left as they are.
	srv.Accept("refused")
	if again := a.consulPass(ctx); !again {
		t.Error("Consul unreachable, and no retry soon")
	}
	if st := a.consul.state[edge.ID]; st.State != "error" || !strings.Contains(st.Error, "Consul isn't reachable") {
		t.Fatalf("unreachable: %+v", st)
	}
	if m, _ := nodes.get(edge.ID); m.GetManagedBy() == "" {
		t.Fatal("the layer was taken off while Consul was away")
	}
	srv.Accept(testConsulToken)

	// No longer picked: the layer off, the token deleted, the node out of
	// the catalog.
	enableConsul(t, a, srv, map[string]string{"dc": "scw"})
	a.consulPass(ctx)
	if m, _ := nodes.get(edge.ID); m.GetManagedBy() != "" {
		t.Fatalf("the layer is still there: %v", m)
	}
	if srv.TokenFor("edge-1") != nil || !slices.Contains(srv.DeregisteredNodes(), "edge-1") {
		t.Fatalf("token %v, deregistered %v", srv.TokenFor("edge-1"), srv.DeregisteredNodes())
	}
	if st := a.consul.state[edge.ID]; st.State != "removed" {
		t.Fatalf("not picked: %+v", st)
	}
	a.consulPass(ctx)
	if st := a.consul.state[edge.ID]; st.State != "not picked" {
		t.Fatalf("not picked, after: %+v", st)
	}

	// Picked again, then removed from the Controller: its layer taken off
	// at once, its token deleted by the next pass.
	enableConsul(t, a, srv, map[string]string{"dc": "lgs"})
	a.consulPass(ctx)
	if srv.TokenFor("edge-1") == nil {
		t.Fatal("no token picked again")
	}
	// A pass between the layer's removal and the node's: nothing given
	// back.
	a.consulNodeRemoved(edge)
	a.consulPass(ctx)
	if m, _ := nodes.get(edge.ID); m.GetManagedBy() != "" {
		t.Fatal("a pass gave the layer back to a node being removed")
	}
	if err := a.removeNode(ctx, edge.ID); err != nil {
		t.Fatal(err)
	}
	if m, _ := nodes.get(edge.ID); m.GetManagedBy() != "" {
		t.Fatal("the layer stays on a node removed")
	}
	a.consulPass(ctx)
	if srv.TokenFor("edge-1") != nil || len(a.consul.store.Nodes()) != 0 {
		t.Fatalf("a removed node's token: %v, records %v", srv.TokenFor("edge-1"), a.consul.store.Nodes())
	}
}

// Two nodes named alike: one name in Consul, the second refused.
func TestConsulSameName(t *testing.T) {
	a, nodes, srv := consulTestApp(t)
	enableConsul(t, a, srv, nil)
	one := addConsulNode(t, a, "edge 1", "192.0.2.11:9505", nil)
	two := addConsulNode(t, a, "edge/1", "192.0.2.12:9505", nil)
	a.consulPass(context.Background())
	states := []string{a.consul.state[one.ID].State, a.consul.state[two.ID].State}
	slices.Sort(states)
	if !slices.Equal(states, []string{"applied", "error"}) || srv.TokenCount() != 1 {
		t.Fatalf("states %v, %d tokens", states, srv.TokenCount())
	}
	if m, _ := nodes.get(one.ID); m.GetManagedBy() != "" && m.GetNodeName() != "edge-1" {
		t.Fatalf("name %q", m.GetNodeName())
	}
}

// Another Controller's layer is left alone.
func TestConsulAnotherController(t *testing.T) {
	a, nodes, srv := consulTestApp(t)
	enableConsul(t, a, srv, nil)
	n := addConsulNode(t, a, "edge-1", "192.0.2.11:9505", nil)
	nodes.node(n).managed = &janusv1alpha1.ConsulManaged{ManagedBy: "janus-controller 99999999 consul abcd"}
	a.consulPass(context.Background())
	if st := a.consul.state[n.ID]; st.State != "another controller" || srv.TokenCount() != 0 {
		t.Fatalf("%+v", st)
	}
}

func TestConsulAPI(t *testing.T) {
	a, _, srv := consulTestApp(t)
	call := func(method, path, body string) (int, map[string]any) {
		t.Helper()
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		switch {
		case method == http.MethodGet && path == "/api/consul":
			a.handleConsul(w, r)
		case method == http.MethodPut:
			a.handleConsulSet(w, r)
		case path == "/api/consul/test":
			a.handleConsulTest(w, r)
		}
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	ca := strings.ReplaceAll(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})), "\n", `\n`)
	body := `{"enabled": true, "address": "` + srv.URL + `", "tls": {"ca_pem": "` + ca + `"}, "token": "` + testConsulToken + `",
		"selector": "dc=lgs", "services": {"janus": true, "urls": true}, "node_meta_from_labels": true}`
	if code, out := call(http.MethodPut, "/api/consul", body); code != http.StatusOK {
		t.Fatalf("PUT: %d %v", code, out)
	}
	code, out := call(http.MethodGet, "/api/consul", "")
	set := out["settings"].(map[string]any)
	if code != http.StatusOK || set["has_token"] != true || set["token"] != nil || set["selector"] != "dc=lgs" {
		t.Fatalf("GET: %d %v", code, out)
	}
	// The test, the token left out: the saved one.
	draft := strings.Replace(body, `"token": "`+testConsulToken+`",`, "", 1)
	if code, out := call(http.MethodPost, "/api/consul/test", draft); code != http.StatusOK || out["ok"] != true {
		t.Fatalf("test: %d %v", code, out)
	}
	if code, _ := call(http.MethodPut, "/api/consul", `{"enabled": true, "address": "http://consul:8500", "selector": "Bad Key=x"}`); code != http.StatusBadRequest {
		t.Fatalf("a bad PUT: %d", code)
	}
}

// A machine's node: its hypervisor's labels under its own, picked by
// the selector and in its node metadata.
func TestConsulInheritedLabels(t *testing.T) {
	a, nodes, srv := consulTestApp(t)
	kvm := labelledHypervisor(t, a.hypervisors, "kvm01", map[string]string{"dc": "lgs", "room": "a"})
	edge := machineNode(t, a.store, a.machines, "edge-1", kvm.ID, map[string]string{"room": "b"})
	other := machineNode(t, a.store, a.machines, "other", "", map[string]string{"room": "b"})
	enableConsul(t, a, srv, map[string]string{"dc": "lgs"})
	a.consulPass(context.Background())
	m, _ := nodes.get(edge.ID)
	if meta := m.GetNodeMeta(); meta["dc"] != "lgs" || meta["room"] != "b" {
		t.Fatalf("node meta %v", meta)
	}
	if st := a.consul.state[other.ID]; st.State != "not picked" {
		t.Fatalf("a node without dc=lgs: %+v", st)
	}
}

// A release without the managed layer is "unsupported" only when the
// selector picks it - else it's off or not picked, and no retry soon
// for it (v2026.10.29-2's lab: every old node read "unsupported").
func TestConsulOldNodeNotPicked(t *testing.T) {
	a, nodes, srv := consulTestApp(t)
	ctx := context.Background()
	elsewhere := addConsulNode(t, a, "elsewhere", "192.0.2.21:9505", map[string]string{"dc": "scw"})
	nodes.node(elsewhere).old = true
	a.consulPass(ctx)
	if st := a.consul.state[elsewhere.ID]; st.State != "off" {
		t.Fatalf("off: %+v", st)
	}

	enableConsul(t, a, srv, map[string]string{"dc": "lgs"})
	if again := a.consulPass(ctx); again {
		t.Error("an old node the selector doesn't pick, and a retry soon")
	}
	if st := a.consul.state[elsewhere.ID]; st.State != "not picked" || st.Error != "" {
		t.Fatalf("an old node the selector doesn't pick: %+v", st)
	}

	here := addConsulNode(t, a, "here", "192.0.2.22:9505", map[string]string{"dc": "lgs"})
	nodes.node(here).old = true
	if again := a.consulPass(ctx); !again {
		t.Error("an old node picked, and no retry soon")
	}
	if st := a.consul.state[here.ID]; st.State != "unsupported" {
		t.Fatalf("an old node picked: %+v", st)
	}
	if srv.TokenCount() != 0 {
		t.Fatal("a token for a node without the layer")
	}
}

// TestConsulScopeIsTheCluster: a member of one of the Controller's
// clusters names its HAProxy services after the cluster - the layer's
// scope, its token's scope policy; out of the cluster, after itself, and
// the cluster's policy, used by no node any more, goes (security review
// 2026-11-05 #28).
func TestConsulScopeIsTheCluster(t *testing.T) {
	a, nodes, srv := consulTestApp(t)
	ctx := context.Background()
	edge := addConsulNode(t, a, "edge-1", "192.0.2.11:9505", nil)
	st, err := clusters.Open(a.dataDir, testSealer{})
	if err != nil {
		t.Fatal(err)
	}
	a.clusters = newClusterEngine(a, st)
	c, err := st.Create("kluster-prod", 0, []string{"haproxy"})
	if err != nil {
		t.Fatal(err)
	}
	setMembers := func(m []clusters.Member) {
		if _, err := st.Update(c.ID, func(c *clusters.Cluster) error { c.Members = m; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	setMembers([]clusters.Member{{NodeID: edge.ID, Name: "edge-1", Address: "192.0.2.11"}})
	enableConsul(t, a, srv, nil)
	policies := func() []string {
		var names []string
		for _, p := range srv.TokenFor("edge-1").Policies {
			names = append(names, p.Name)
		}
		return names
	}
	a.consulPass(ctx)
	a.consulPass(ctx)
	if m, _ := nodes.get(edge.ID); m.GetScope() != "kluster-prod" || !slices.Equal(policies(), []string{consulctl.PolicyName, "janus-scope-kluster-prod"}) {
		t.Fatalf("a member: scope %q, token %v", m.GetScope(), policies())
	}
	setMembers(nil)
	for range 3 {
		a.consulPass(ctx)
	}
	if m, _ := nodes.get(edge.ID); m.GetScope() != "edge-1" || !slices.Equal(policies(), []string{consulctl.PolicyName, "janus-scope-edge-1"}) {
		t.Fatalf("out of the cluster: scope %q, token %v", m.GetScope(), policies())
	}
	if srv.Policy("janus-scope-kluster-prod") != "" {
		t.Errorf("the cluster's policy kept with no member: %v", srv.PolicyNames())
	}
}

// TestConsulScopeAfterTheNodesUpdate is the order the notes give: the
// Controller updated first, a node of a release before scoped names
// given the layer (it drops the scope, keeps the layer's managed_by),
// then the node updated - the layer given again, once, and the older
// right taken off its token.
func TestConsulScopeAfterTheNodesUpdate(t *testing.T) {
	a, nodes, srv := consulTestApp(t)
	ctx := context.Background()
	edge := addConsulNode(t, a, "edge-1", "192.0.2.11:9505", nil)
	nodes.node(edge).dropsScope = true
	enableConsul(t, a, srv, nil)
	policies := func() []string {
		var names []string
		for _, p := range srv.TokenFor("edge-1").Policies {
			names = append(names, p.Name)
		}
		return names
	}
	legacy := []string{consulctl.PolicyName, "janus-scope-edge-1", consulctl.LegacyPolicyName}
	for range 3 {
		a.consulPass(ctx)
	}
	// The older release: given the layer once - not every pass, it would
	// drop the scope again -, its token keeps the older right.
	if m, sets := nodes.get(edge.ID); sets != 1 || m.GetScope() != "" || !slices.Equal(policies(), legacy) {
		t.Fatalf("an older release: %d sets, scope %q, token %v", sets, m.GetScope(), policies())
	}
	if st := a.consul.state[edge.ID]; st.State != "applied" || !st.Legacy {
		t.Fatalf("an older release: %+v", st)
	}

	// Updated: the same layer kept (no scope in it), the node naming its
	// services after itself - the layer given again with the scope, the
	// older right gone.
	nodes.node(edge).dropsScope, nodes.node(edge).saysScope = false, true
	a.consulPass(ctx)
	if m, sets := nodes.get(edge.ID); sets != 2 || m.GetScope() != "edge-1" {
		t.Fatalf("updated: %d sets, scope %q", sets, m.GetScope())
	}
	if got := policies(); !slices.Equal(got, []string{consulctl.PolicyName, "janus-scope-edge-1"}) {
		t.Fatalf("updated: token %v", got)
	}
	for range 2 {
		a.consulPass(ctx)
	}
	if _, sets := nodes.get(edge.ID); sets != 2 {
		t.Fatalf("given again after it carried the scope: %d sets", sets)
	}
	if st := a.consul.state[edge.ID]; st.State != "applied" || st.Legacy || st.Scope != "edge-1" {
		t.Fatalf("updated: %+v", st)
	}
}

// TestConsulScopeNamedOtherwise: a node that says it names its services
// after something else than the Controller's scope keeps the older right
// until its layer carries the Controller's.
func TestConsulScopeNamedOtherwise(t *testing.T) {
	a, nodes, srv := consulTestApp(t)
	ctx := context.Background()
	edge := addConsulNode(t, a, "edge-1", "192.0.2.11:9505", nil)
	enableConsul(t, a, srv, nil)
	a.consulPass(ctx) // layer given (scope edge-1)
	// Its layer's scope lost (restored from before), a cluster of its own
	// the Controller doesn't know.
	fn := nodes.node(edge)
	nodes.mu.Lock()
	fn.managed.Scope, fn.saysScope, fn.cluster = "", true, "elsewhere"
	nodes.mu.Unlock()
	a.consulPass(ctx)
	if st := a.consul.state[edge.ID]; !st.Legacy {
		t.Fatalf("named after another scope, and no older right: %+v", st)
	}
	if m, sets := nodes.get(edge.ID); sets != 2 || m.GetScope() != "edge-1" {
		t.Fatalf("not given again: %d sets, scope %q", sets, m.GetScope())
	}
	a.consulPass(ctx)
	if st := a.consul.state[edge.ID]; st.Legacy {
		t.Fatalf("its layer carries the scope, the older right kept: %+v", st)
	}
	if !slices.Equal(func() (n []string) {
		for _, p := range srv.TokenFor("edge-1").Policies {
			n = append(n, p.Name)
		}
		return
	}(), []string{consulctl.PolicyName, "janus-scope-edge-1"}) {
		t.Fatal("the token kept the older right")
	}
}
