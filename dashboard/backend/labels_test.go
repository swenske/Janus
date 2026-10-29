package main

import (
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/swenske/Janus/dashboard/backend/internal/auth"
	"github.com/swenske/Janus/dashboard/backend/internal/hypervisor"
	"github.com/swenske/Janus/dashboard/backend/internal/logforward"
	"github.com/swenske/Janus/dashboard/backend/internal/machines"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
	"github.com/swenske/Janus/internal/logship"
	"github.com/swenske/Janus/internal/pki"
)

// newTestLogForwarding is the log forwarding's settings and shipper,
// without the loop that reaches the nodes.
func newTestLogForwarding(t *testing.T) *logForwarding {
	t.Helper()
	st, err := logforward.OpenStore(t.TempDir(), plainSealer{})
	if err != nil {
		t.Fatal(err)
	}
	ship := logship.New(logship.Options{})
	t.Cleanup(ship.Close)
	return &logForwarding{store: st, ship: ship, kick: make(chan struct{}, 1), nodes: map[string]nodeLogState{}}
}

// labelledHypervisor adds a libvirt hypervisor with labels l.
func labelledHypervisor(t *testing.T, hvs *hypervisor.Store, name string, l map[string]string) *hypervisor.Hypervisor {
	t.Helper()
	h := &hypervisor.Hypervisor{Name: name, Kind: hypervisor.KindLibvirt, Labels: l, Libvirt: &hypervisor.LibvirtConfig{
		Host: "192.0.2.2", User: "janus-ctl", Pool: "janus", Networks: []string{"lan"},
	}}
	if err := hvs.Add(h); err != nil {
		t.Fatal(err)
	}
	return h
}

// machineNode adds a node with its own labels l, as a machine on
// hypervisor hvID ("": a node the Controller didn't create).
func machineNode(t *testing.T, st *store.Store, ms *machines.Store, name, hvID string, l map[string]string) *store.Node {
	t.Helper()
	ca, err := pki.NewCA(name)
	if err != nil {
		t.Fatal(err)
	}
	n := &store.Node{Name: name, Address: "127.0.0.1:1", CACertPEM: ca.CertPEM, Fleet: true, Labels: l}
	if hvID != "" {
		m := &machines.Machine{Spec: machines.Spec{Name: name, HypervisorID: hvID}, Phase: machines.PhaseReady}
		if err := ms.Add(m); err != nil {
			t.Fatal(err)
		}
		n.MachineID = m.ID
	}
	if err := st.Add(n); err != nil {
		t.Fatal(err)
	}
	if hvID != "" {
		if _, err := ms.Update(n.MachineID, func(m *machines.Machine) error { m.NodeID = n.ID; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	return n
}

// TestNodeLabels: a machine's node has its hypervisor's labels under
// its own - its own winning on a key -, read each time: a change to the
// hypervisor's follows at once; any other node has its own alone.
func TestNodeLabels(t *testing.T) {
	a, _ := newTestApp(t)
	kvm := labelledHypervisor(t, a.hypervisors, "kvm01", map[string]string{"dc": "lgs", "room": "a"})
	bare := labelledHypervisor(t, a.hypervisors, "kvm02", nil)

	edge := machineNode(t, a.store, a.machines, "edge-1", kvm.ID, map[string]string{"room": "b", "team": "web"})
	if got, want := a.nodeLabels(edge), map[string]string{"dc": "lgs", "room": "b", "team": "web"}; !maps.Equal(got, want) {
		t.Errorf("effective labels %v, want %v", got, want)
	}
	inherited, from := a.inheritedLabels(edge)
	if !maps.Equal(inherited, map[string]string{"dc": "lgs", "room": "a"}) || from == nil || *from != (labelSource{Kind: "hypervisor", ID: kvm.ID, Name: "kvm01"}) {
		t.Errorf("inherited %v from %+v", inherited, from)
	}
	// A fresh map each time.
	got := a.nodeLabels(edge)
	got["dc"] = "elsewhere"
	if a.nodeLabels(edge)["dc"] != "lgs" {
		t.Error("changing nodeLabels' answer changed the node's labels")
	}
	if h, _ := a.hypervisors.Get(kvm.ID); h.Labels["dc"] != "lgs" {
		t.Error("changing nodeLabels' answer changed the hypervisor's")
	}

	// The hypervisor's change follows at once.
	h, _ := a.hypervisors.Get(kvm.ID)
	h.Labels = map[string]string{"dc": "scw"}
	if err := a.hypervisors.Update(h); err != nil {
		t.Fatal(err)
	}
	if got, want := a.nodeLabels(edge), map[string]string{"dc": "scw", "room": "b", "team": "web"}; !maps.Equal(got, want) {
		t.Errorf("after the hypervisor's change: %v, want %v", got, want)
	}

	for _, tc := range []struct {
		name string
		n    *store.Node
	}{
		{"a node the Controller didn't create", machineNode(t, a.store, a.machines, "db-1", "", map[string]string{"team": "db"})},
		{"a machine on a hypervisor without labels", machineNode(t, a.store, a.machines, "db-2", bare.ID, map[string]string{"team": "db"})},
	} {
		if l, from := a.inheritedLabels(tc.n); l != nil || from != nil {
			t.Errorf("%s inherits %v from %+v", tc.name, l, from)
		}
		if got := a.nodeLabels(tc.n); !maps.Equal(got, map[string]string{"team": "db"}) {
			t.Errorf("%s: %v", tc.name, got)
		}
	}
	// A machine record without a hypervisor, a machine forgotten: nothing
	// inherited, nothing broken.
	lone := &machines.Machine{Spec: machines.Spec{Name: "lone"}, Phase: machines.PhaseReady}
	if err := a.machines.Add(lone); err != nil {
		t.Fatal(err)
	}
	for _, mid := range []string{lone.ID, "gone"} {
		n := &store.Node{Name: "x", MachineID: mid, Labels: map[string]string{"team": "x"}}
		if l, _ := a.inheritedLabels(n); l != nil {
			t.Errorf("machine %s: inherits %v", mid, l)
		}
		if got := a.nodeLabels(n); !maps.Equal(got, map[string]string{"team": "x"}) {
			t.Errorf("machine %s: %v", mid, got)
		}
	}
	// No hypervisor store (a Controller's tests): its own.
	b := &app{store: a.store, machines: a.machines}
	if got := b.nodeLabels(edge); !maps.Equal(got, map[string]string{"room": "b", "team": "web"}) {
		t.Errorf("without hypervisors: %v", got)
	}
}

// TestHypervisorLabelsAPI: a hypervisor's labels are given when it's
// added, checked, replaced by a change that gives them, kept by one that
// doesn't (a client from before labels), cleared by {} - each change
// pokes the log forwarding profiles; its machines' nodes show them as
// inherited_labels, apart from their own labels.
func TestHypervisorLabelsAPI(t *testing.T) {
	a, _ := newTestApp(t)
	a.logfwd = &logForwarding{kick: make(chan struct{}, 1)}
	poked := func() bool {
		select {
		case <-a.logfwd.kick:
			return true
		default:
			return false
		}
	}
	add := map[string]any{"name": "kvm01", "kind": "libvirt", "labels": map[string]string{"dc": "lgs", "rack": "12"},
		"libvirt": map[string]any{"host": "192.0.2.2", "user": "janus-ctl", "pool": "janus", "networks": []string{"lan"}}}
	bad := maps.Clone(add)
	bad["labels"] = map[string]string{"DC": "lgs"}
	if rec := call(t, a.handleHypervisorCreate, "POST", "/api/hypervisors", "POST /api/hypervisors", bad); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "label key") {
		t.Errorf("a bad label: %d %s", rec.Code, rec.Body)
	}
	rec := call(t, a.handleHypervisorCreate, "POST", "/api/hypervisors", "POST /api/hypervisors", add)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var v hypervisorView
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	if !maps.Equal(v.Labels, map[string]string{"dc": "lgs", "rack": "12"}) {
		t.Errorf("created with %v", v.Labels)
	}
	patch := func(body map[string]any) hypervisorView {
		t.Helper()
		rec := call(t, a.handleHypervisorUpdate, "PATCH", "/api/hypervisors/"+v.ID, "PATCH /api/hypervisors/{id}", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("patch %v: %d %s", body, rec.Code, rec.Body)
		}
		var out hypervisorView
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out
	}
	noLabels := maps.Clone(add)
	delete(noLabels, "labels")
	noLabels["controller_address"] = "192.0.2.1:8443"
	if got := patch(noLabels); !maps.Equal(got.Labels, map[string]string{"dc": "lgs", "rack": "12"}) || got.ControllerAddress != "192.0.2.1:8443" {
		t.Errorf("a change without labels: %+v", got)
	}
	if poked() {
		t.Error("a change without labels poked the log forwarding")
	}

	n := machineNode(t, a.store, a.machines, "edge-1", v.ID, map[string]string{"rack": "7"})
	nodesView := func() nodeView {
		t.Helper()
		rec := call(t, a.handleNodes, "GET", "/api/nodes", "GET /api/nodes", nil)
		var ns []nodeView
		if err := json.Unmarshal(rec.Body.Bytes(), &ns); err != nil || len(ns) != 1 {
			t.Fatalf("nodes: %v %s", err, rec.Body)
		}
		return ns[0]
	}
	nv := nodesView()
	if !maps.Equal(nv.Labels, map[string]string{"rack": "7"}) || !maps.Equal(nv.InheritedLabels, map[string]string{"dc": "lgs", "rack": "12"}) ||
		nv.InheritedFrom == nil || nv.InheritedFrom.Name != "kvm01" || nv.InheritedFrom.ID != v.ID {
		t.Errorf("the node's view: %+v (from %+v)", nv, nv.InheritedFrom)
	}

	withLabels := maps.Clone(add)
	withLabels["labels"] = map[string]string{"dc": "scw"}
	if got := patch(withLabels); !maps.Equal(got.Labels, map[string]string{"dc": "scw"}) {
		t.Errorf("replaced: %v", got.Labels)
	}
	if !poked() {
		t.Error("a label change didn't poke the log forwarding")
	}
	if nv := nodesView(); !maps.Equal(nv.InheritedLabels, map[string]string{"dc": "scw"}) {
		t.Errorf("the node after the hypervisor's change: %v", nv.InheritedLabels)
	}
	// The node's own PATCH answers both.
	rec = call(t, a.handleNode, "PATCH", "/api/nodes/"+n.ID, "PATCH /api/nodes/", map[string]any{"labels": map[string]string{"dc": "lgs"}})
	var own nodeView
	_ = json.Unmarshal(rec.Body.Bytes(), &own)
	if rec.Code != http.StatusOK || !maps.Equal(own.Labels, map[string]string{"dc": "lgs"}) || !maps.Equal(own.InheritedLabels, map[string]string{"dc": "scw"}) {
		t.Errorf("the node's PATCH: %d %s", rec.Code, rec.Body)
	}
	if got := a.nodeLabels(n); got["dc"] != "lgs" {
		t.Errorf("the node's own label doesn't win: %v", got)
	}

	cleared := maps.Clone(add)
	cleared["labels"] = map[string]string{}
	if got := patch(cleared); len(got.Labels) != 0 || got.Labels == nil {
		t.Errorf("cleared: %#v", got.Labels)
	}
	if nv := nodesView(); len(nv.InheritedLabels) != 0 || nv.InheritedLabels == nil || nv.InheritedFrom != nil {
		t.Errorf("the node of a hypervisor without labels: %#v from %+v", nv.InheritedLabels, nv.InheritedFrom)
	}
	bad = maps.Clone(add)
	bad["labels"] = map[string]string{"dc": "-"}
	if rec := call(t, a.handleHypervisorUpdate, "PATCH", "/api/hypervisors/"+v.ID, "PATCH /api/hypervisors/{id}", bad); rec.Code != http.StatusBadRequest {
		t.Errorf("a bad label value: %d %s", rec.Code, rec.Body)
	}

	// Persisted.
	again, err := hypervisor.Open(a.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if h, _ := again.Get(v.ID); h == nil || h.Labels != nil {
		t.Errorf("reloaded: %+v", h)
	}
}

// TestInheritedLabelsScopes: grants, scoped tokens and janusctl's
// scoped certificates reach a machine's node by its hypervisor's labels
// too - its own winning on a key -, and stop when the hypervisor's
// change; the log forwarding page lists the effective labels.
func TestInheritedLabelsScopes(t *testing.T) {
	a := newAuthApp(t)
	hvs, err := hypervisor.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.hypervisors = hvs
	root := a.login(t, "root", "root-password")
	code, body := a.req(t, "POST", "/api/users", root, map[string]any{"name": "wes", "role": "none",
		"grants": []map[string]any{{"role": "operator", "selector": map[string]string{"dc": "lgs"}, "domains": []string{"haproxy", "machines"}}}})
	if code != http.StatusCreated {
		t.Fatalf("wes: %d %s", code, body)
	}
	var made struct {
		Password string `json:"password"`
	}
	_ = json.Unmarshal([]byte(body), &made)
	if err := a.auth.ChangePassword("wes", made.Password, "wes-password"); err != nil {
		t.Fatal(err)
	}
	wes := a.login(t, "wes", "wes-password")

	kvm := labelledHypervisor(t, hvs, "kvm01", map[string]string{"dc": "lgs"})
	edge := machineNode(t, a.store, a.machines, "edge-1", kvm.ID, nil)
	other := machineNode(t, a.store, a.machines, "edge-2", kvm.ID, map[string]string{"dc": "scw"}) // its own wins
	machineNode(t, a.store, a.machines, "db-1", "", map[string]string{"team": "db"})

	names := func(cred string) []string {
		t.Helper()
		code, body := a.req(t, "GET", "/api/nodes", cred, nil)
		if code != http.StatusOK {
			t.Fatalf("nodes: %d %s", code, body)
		}
		var ns []nodeView
		_ = json.Unmarshal([]byte(body), &ns)
		var out []string
		for _, n := range ns {
			out = append(out, n.Name)
			if n.Name == "edge-1" && (len(n.Labels) != 0 || n.InheritedLabels["dc"] != "lgs" || n.InheritedFrom == nil || n.InheritedFrom.Name != "kvm01") {
				t.Errorf("edge-1's view: %+v", n)
			}
		}
		slices.Sort(out)
		return out
	}
	if got := names(wes); !slices.Equal(got, []string{"edge-1"}) {
		t.Errorf("wes sees %v", got)
	}
	// Its machine too, and its page, with what the grant gives.
	code, body = a.req(t, "GET", "/api/machines", wes, nil)
	if code != http.StatusOK || !strings.Contains(body, `"`+edge.MachineID+`"`) || strings.Contains(body, `"`+other.MachineID+`"`) {
		t.Errorf("wes's machines: %d %s", code, body)
	}
	code, body = a.req(t, "GET", "/nodes/"+edge.ID+"/api/me", wes, nil)
	if code != http.StatusOK || !strings.Contains(body, `"HAProxyService/ApplyConfig"`) || strings.Contains(body, `"SystemService/Reboot"`) {
		t.Errorf("wes on edge-1's page: %d %s", code, body)
	}
	if code, _ := a.req(t, "GET", "/nodes/"+other.ID+"/api/me", wes, nil); code != http.StatusForbidden {
		t.Errorf("wes on edge-2's page (dc=scw of its own): %d", code)
	}
	// A token scoped to dc=lgs.
	code, body = a.req(t, "POST", "/api/tokens", root, map[string]any{"name": "lgs", "scope": map[string]any{"selector": map[string]string{"dc": "lgs"}}})
	if code != http.StatusCreated {
		t.Fatalf("token: %d %s", code, body)
	}
	var tok struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal([]byte(body), &tok)
	if got := names("Bearer " + tok.Token); !slices.Equal(got, []string{"edge-1"}) {
		t.Errorf("the dc=lgs token sees %v", got)
	}
	// janusctl's scoped certificate names edge-1 alone.
	p := principal{User: "wes", Grants: []auth.Grant{{Role: auth.Operator, Selector: map[string]string{"dc": "lgs"}, Domains: []string{"haproxy"}}}}
	id, err := a.cliIdentityFor(p)
	if err != nil {
		t.Fatal(err)
	}
	edgeKey, _ := nodeCAKey(edge)
	if id.Scope == nil || len(id.Scope.Nodes) != 1 || len(id.Scope.Nodes[0].CAKeys) != 1 || !slices.Equal(id.Scope.Nodes[0].CAKeys[0], edgeKey) {
		t.Errorf("wes's janusctl scope: %+v", id.Scope)
	}

	// The log forwarding page: each node's effective labels.
	a.logfwd = newTestLogForwarding(t)
	for _, nv := range a.logForwardingView().Nodes {
		want := map[string]map[string]string{"edge-1": {"dc": "lgs"}, "edge-2": {"dc": "scw"}, "db-1": {"team": "db"}}[nv.Name]
		if !maps.Equal(nv.Labels, want) {
			t.Errorf("log forwarding's %s: %v, want %v", nv.Name, nv.Labels, want)
		}
	}

	// The hypervisor relabelled: edge-1 is out of wes's reach at once.
	h, _ := hvs.Get(kvm.ID)
	h.Labels = map[string]string{"dc": "par"}
	if err := hvs.Update(h); err != nil {
		t.Fatal(err)
	}
	if got := names(wes); len(got) != 0 {
		t.Errorf("wes after the hypervisor's change: %v", got)
	}
	if code, _ := a.req(t, "GET", "/nodes/"+edge.ID+"/api/me", wes, nil); code != http.StatusForbidden {
		t.Errorf("wes on edge-1's page after the change: %d", code)
	}
}
