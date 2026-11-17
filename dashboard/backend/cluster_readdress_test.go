package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/swenske/Janus/dashboard/backend/internal/machines"
	"github.com/swenske/Janus/dashboard/backend/internal/nodeproxy"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/clusters"
	"github.com/swenske/Janus/internal/nocloud"
)

// movingNodes: a node takes the network configuration it's given at once
// and answers at its new address - the Controller's record following
// it, as nodeproxy's confirmation does -; a node shut down answers no
// more.
type movingNodes struct {
	answeringNodes
	f    *clusterFake
	mu   sync.Mutex
	fail error
	// cutShutdown: a shutdown's answer lost - the connection closed first
	// (a machine's destruction then powers it off without waiting).
	cutShutdown bool
	applied     map[string]*janusv1alpha1.NetworkConfig // by node id
	windows     []time.Duration                         // each apply's confirmation window
	shutdown    []string                                // node names
}

func (m *movingNodes) ApplyNetwork(_ context.Context, node *store.Node, st *store.Store, cfg *janusv1alpha1.NetworkConfig, window time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	m.windows = append(m.windows, window)
	host, port, _ := net.SplitHostPort(node.Addr())
	at := ""
	for _, i := range cfg.GetInterfaces() {
		for _, a := range i.GetAddresses() {
			p := netip.MustParsePrefix(a)
			if p.Addr().String() == host {
				at = host
			} else if at == "" && p.Addr().Is4() {
				at = p.Addr().String()
			}
		}
	}
	if err := st.SetAddress(node.ID, net.JoinHostPort(at, port)); err != nil {
		return err
	}
	if m.applied == nil {
		m.applied = map[string]*janusv1alpha1.NetworkConfig{}
	}
	m.applied[node.ID] = proto.Clone(cfg).(*janusv1alpha1.NetworkConfig)
	m.f.mu.Lock()
	m.f.nodes[node.Name].netcfg = proto.Clone(cfg).(*janusv1alpha1.NetworkConfig)
	m.f.byAddr[net.JoinHostPort(at, port)] = node.Name
	delete(m.f.silent, net.JoinHostPort(at, port))
	m.f.mu.Unlock()
	return nil
}

func (m *movingNodes) Network(_ context.Context, node *store.Node) (*janusv1alpha1.NetworkConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cfg := m.applied[node.ID]; cfg != nil {
		return proto.Clone(cfg).(*janusv1alpha1.NetworkConfig), nil
	}
	return nil, errors.New("not configured by this test")
}

func (m *movingNodes) Shutdown(_ context.Context, node *store.Node) error {
	m.mu.Lock()
	m.shutdown = append(m.shutdown, node.Name)
	m.mu.Unlock()
	m.f.mu.Lock()
	if m.f.silent == nil {
		m.f.silent = map[string]bool{}
	}
	m.f.silent[node.Addr()] = true
	m.f.mu.Unlock()
	if m.cutShutdown {
		return errors.New("connection closed")
	}
	return nil
}

func (m *movingNodes) Info(context.Context, *store.Node) (*nodeproxy.NodeInfo, error) {
	return &nodeproxy.NodeInfo{}, nil
}

func useMovingNodes(t *testing.T, f *clusterFake) *movingNodes {
	t.Helper()
	mn := &movingNodes{f: f}
	prev := nodes
	nodes = mn
	t.Cleanup(func() { nodes = prev })
	return mn
}

// A member moved to another address: drained, its interface changed on
// trial - the Controller following it -, every member told (the peers,
// the templates), back in service.
func TestClusterReaddress(t *testing.T) {
	a, f, nodesByName := clusterApp(t, "lb-1", "lb-2", "lb-3")
	mn := useMovingNodes(t, f)
	c := newCluster(t, a, nodesByName, "lb-1", "lb-2", "lb-3")
	ctx := context.Background()
	if err := a.clusters.Create(ctx, c.ID, "lb-1", "admin"); err != nil {
		t.Fatal(err)
	}
	to := clusters.Addressing{Addresses: []string{"10.0.0.40/24"}, Gateway: "10.0.0.254"}

	for _, bad := range []clusters.Addressing{
		{Addresses: []string{"10.0.0.1/24"}}, // lb-1's
		{Addresses: []string{"10.0.0.2/24"}}, // its own
		{Addresses: []string{"10.0.0.40"}},
	} {
		if err := a.clusters.Readdress(ctx, c.ID, "lb-2", bad, "admin"); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}

	// The node can't be confirmed there: back in service where it was.
	mn.fail = errors.New("couldn't confirm before the node reverts")
	mark := f.mark()
	if err := a.clusters.Readdress(ctx, c.ID, "lb-2", to, "admin"); err == nil || !strings.Contains(err.Error(), "couldn't confirm") {
		t.Errorf("a move that fails: %v", err)
	}
	if !inOrder(f.callsOf("lb-2", mark), "ClusterDrain", "ClusterUndrain") {
		t.Errorf("lb-2 not back in service: %v", f.callsOf("lb-2", mark))
	}
	if cur, _ := a.clusters.Store.Get(c.ID); cur.Member("lb-2").Address != "10.0.0.2" {
		t.Errorf("lb-2's address changed: %s", cur.Member("lb-2").Address)
	}
	mn.fail = nil

	mark = f.mark()
	if err := a.clusters.Readdress(ctx, c.ID, "lb-2", to, "admin"); err != nil {
		t.Fatal(err)
	}
	if !inOrder(f.callsSince(mark), "lb-2 ClusterDrain", "lb-2 NetworkStatus", "lb-1 ClusterSet lb-1,lb-2,lb-3", "lb-3 ClusterSet lb-1,lb-2,lb-3", "lb-2 ClusterSet lb-1,lb-2,lb-3", "lb-2 ClusterUndrain") {
		t.Errorf("the order: %v", f.callsSince(mark))
	}
	cur, _ := a.clusters.Store.Get(c.ID)
	if cur.Member("lb-2").Address != "10.0.0.40" {
		t.Errorf("lb-2's member address: %s", cur.Member("lb-2").Address)
	}
	if n, _ := a.store.Get(nodesByName["lb-2"].ID); n.Addr() != "10.0.0.40:9505" {
		t.Errorf("the Controller reaches lb-2 at %s", n.Addr())
	}
	f.mu.Lock()
	set := f.nodes["lb-3"].set
	lan := f.nodes["lb-2"].netcfg.GetInterfaces()[0]
	f.mu.Unlock()
	if i := slices.IndexFunc(set.GetMembers(), func(m *janusv1alpha1.ClusterMember) bool { return m.GetName() == "lb-2" }); set.GetMembers()[i].GetAddress() != "10.0.0.40" {
		t.Errorf("lb-3 was told %v", set.GetMembers())
	}
	if strings.Join(lan.GetAddresses(), " ") != "10.0.0.40/24" || lan.GetMode() != janusv1alpha1.AddressingMode_ADDRESSING_MODE_STATIC {
		t.Errorf("lb-2's interface: %v", lan)
	}
	if st := a.clusters.Store.State(c.ID); st.Members["lb-2"].Drained || st.Members["lb-2"].Applied != cur.Desired {
		t.Errorf("lb-2's state after: %+v", st.Members["lb-2"])
	}

	// Through the API.
	rec := call(t, a.handleClusterReaddress, "POST", "/api/clusters/"+c.ID+"/members/lb-3/address", "POST /api/clusters/{id}/members/{name}/address", clusters.Addressing{Addresses: []string{"nope"}})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a bad address: %d %s", rec.Code, rec.Body)
	}
	rec = call(t, a.handleClusterReaddress, "POST", "/api/clusters/"+c.ID+"/members/lb-3/address", "POST /api/clusters/{id}/members/{name}/address", clusters.Addressing{Addresses: []string{"10.0.0.43/24"}})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("moving lb-3: %d %s", rec.Code, rec.Body)
	}
	a.clusters.wait()
	if cur, _ := a.clusters.Store.Get(c.ID); cur.Member("lb-3").Address != "10.0.0.43" || cur.Error != "" {
		t.Errorf("lb-3 after the API's move: %s %q", cur.Member("lb-3").Address, cur.Error)
	}
}

// A replacement keeping the old member's address: it joins drained on
// its own and stays so - the others serve - while the old member is
// drained, leaves and is powered off; it then moves to the old member's
// address, every member told, and comes into service there.
func TestClusterReplaceKeepingAddress(t *testing.T) {
	a, f, nodesByName := clusterApp(t, "lb-1", "lb-2", "lb-3", "lb-4")
	mn := useMovingNodes(t, f)
	c := newCluster(t, a, nodesByName, "lb-1", "lb-2")
	ctx := context.Background()
	if err := a.clusters.Create(ctx, c.ID, "lb-1", "admin"); err != nil {
		t.Fatal(err)
	}
	lb4 := clusters.Member{NodeID: nodesByName["lb-4"].ID, Name: "lb-4", Address: "10.0.0.4"}
	keep := clusters.ReplaceOptions{KeepAddress: true}

	// An address DHCP gives can't be taken over: nothing touched.
	f.nodes["lb-1"].netcfg = &janusv1alpha1.NetworkConfig{Interfaces: []*janusv1alpha1.NetworkInterface{{Name: "lan", Mode: janusv1alpha1.AddressingMode_ADDRESSING_MODE_DHCP}}}
	mark := f.mark()
	if err := a.clusters.Replace(ctx, c.ID, "lb-1", lb4, keep, "admin"); err == nil || !strings.Contains(err.Error(), "DHCP") {
		t.Errorf("a DHCP address taken over: %v", err)
	}
	if len(f.callsOf("lb-4", mark)) > 0 {
		t.Errorf("lb-4 touched: %v", f.callsOf("lb-4", mark))
	}
	f.nodes["lb-1"].netcfg = &janusv1alpha1.NetworkConfig{Interfaces: []*janusv1alpha1.NetworkInterface{{Name: "lan", Mac: "52:54:00:aa:00:01",
		Mode: janusv1alpha1.AddressingMode_ADDRESSING_MODE_STATIC, Addresses: []string{"10.0.0.1/24", "2001:db8::1/64"}, Gateway: "10.0.0.254", Gateway6: "2001:db8::fe"}}}

	// lb-1 never frees its address: lb-4 in service on its own.
	a.clusters.Release = func(context.Context, clusters.Member) error { return nil }
	a.clusters.ReleaseTimeout = 100 * time.Millisecond
	mark = f.mark()
	err := a.clusters.Replace(ctx, c.ID, "lb-1", lb4, keep, "admin")
	var ae *clusters.AddressError
	if !errors.As(err, &ae) || ae.Moved || !strings.Contains(err.Error(), "still answers at 10.0.0.1:9505") {
		t.Errorf("an address never freed: %v", err)
	}
	cur, _ := a.clusters.Store.Get(c.ID)
	if cur.Member("lb-1") != nil || cur.Member("lb-4") == nil || cur.Member("lb-4").Address != "10.0.0.4" {
		t.Errorf("members after: %+v", cur.Members)
	}
	if !inOrder(f.callsOf("lb-4", mark), "ClusterDrain", "Probe 10.0.0.1:9505", "ClusterUndrain") {
		t.Errorf("lb-4: %v", f.callsOf("lb-4", mark))
	}
	// Its address not taken, lb-1 stays one of the Controller's nodes.
	if _, ok := a.store.Get(nodesByName["lb-1"].ID); !ok {
		t.Error("lb-1 forgotten although lb-4 didn't take its address")
	}

	// For real, lb-2 replaced by lb-3 (a node of the Controller's, powered
	// off for its address).
	a.clusters.Release = a.releaseMember
	f.nodes["lb-2"].netcfg = &janusv1alpha1.NetworkConfig{Interfaces: []*janusv1alpha1.NetworkInterface{{Name: "lan", Mac: "52:54:00:aa:00:02",
		Mode: janusv1alpha1.AddressingMode_ADDRESSING_MODE_STATIC, Addresses: []string{"2001:db8::2/64", "10.0.0.2/24"}, Gateway: "10.0.0.254", Gateway6: "2001:db8::fe"}}}
	lb3 := clusters.Member{NodeID: nodesByName["lb-3"].ID, Name: "lb-3", Address: "10.0.0.3"}
	mark = f.mark()
	if err := a.clusters.Replace(ctx, c.ID, "lb-2", lb3, keep, "admin"); err != nil {
		t.Fatal(err)
	}
	calls := f.callsSince(mark)
	if !inOrder(calls, "lb-3 ClusterSet lb-2,lb-4,lb-3 +cert", "lb-3 ClusterDrain", "lb-2 ClusterDrain", "lb-2 ClusterLeave", "lb-3 Probe 10.0.0.2:9505",
		"lb-3 NetworkStatus", "lb-4 ClusterSet lb-4,lb-3", "lb-3 ClusterSet lb-4,lb-3", "lb-3 ClusterUndrain") {
		t.Errorf("the order: %v", calls)
	}
	if n := strings.Count(strings.Join(calls, "\n"), "lb-3 ClusterUndrain"); n != 1 {
		t.Errorf("lb-3 undrained %d times - in service before it had the address: %v", n, calls)
	}
	if !slices.Contains(mn.shutdown, "lb-2") {
		t.Errorf("lb-2 not powered off: %v", mn.shutdown)
	}
	cur, _ = a.clusters.Store.Get(c.ID)
	if cur.Member("lb-2") != nil || cur.Member("lb-3") == nil || cur.Member("lb-3").Address != "10.0.0.2" {
		t.Errorf("members after: %+v", cur.Members)
	}
	if n, _ := a.store.Get(nodesByName["lb-3"].ID); n.Addr() != "10.0.0.2:9505" {
		t.Errorf("the Controller reaches lb-3 at %s", n.Addr())
	}
	// lb-2, powered off, its address lb-3's: no longer one of the
	// Controller's nodes, and the journal says so.
	if _, ok := a.store.Get(nodesByName["lb-2"].ID); ok {
		t.Error("lb-2 still one of the Controller's nodes, at lb-3's address")
	}
	if !journalSays(t, a, c.ID, "lb-2 forgotten: released, its address 10.0.0.2 is lb-3's now") {
		t.Errorf("the journal doesn't say lb-2 was forgotten: %v", a.clusters.Store.Entries(c.ID, 20))
	}
	f.mu.Lock()
	lan := f.nodes["lb-3"].netcfg.GetInterfaces()[0]
	f.mu.Unlock()
	if strings.Join(lan.GetAddresses(), " ") != "10.0.0.2/24 2001:db8::2/64" || lan.GetGateway6() != "2001:db8::fe" {
		t.Errorf("lb-3's interface: %v", lan)
	}

	// A cluster of one: nothing would serve.
	c2, _ := a.clusters.Store.Get(c.ID)
	if _, err := a.clusters.Store.Update(c2.ID, func(c *clusters.Cluster) error {
		c.Members = c.Members[1:]
		c.SecretsFrom = c.Members[0].Name
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.clusters.Replace(ctx, c.ID, "lb-3", clusters.Member{NodeID: nodesByName["lb-1"].ID, Name: "lb-1", Address: "10.0.0.1"}, keep, "admin"); err == nil || !strings.Contains(err.Error(), "only member") {
		t.Errorf("a cluster of one: %v", err)
	}
}

// A cluster whose firewall template names the member's own address: a
// move would apply the ruleset for one address and confirm at the
// other - refused before anything is touched, a replacement taking an
// address over and a move alike; a template naming only the peers'
// addresses goes on.
func TestClusterTakeoverAndAFirewallOnTheAddress(t *testing.T) {
	a, f, nodesByName := clusterApp(t, "lb-1", "lb-2", "lb-3")
	useMovingNodes(t, f)
	c, err := a.clusters.Store.Create("edge", 0, []string{"haproxy", "files", "firewall"})
	if err != nil {
		t.Fatal(err)
	}
	c, err = a.clusters.Store.Update(c.ID, func(c *clusters.Cluster) error {
		c.Members = []clusters.Member{{NodeID: nodesByName["lb-1"].ID, Name: "lb-1", Address: "10.0.0.1"}, {NodeID: nodesByName["lb-2"].ID, Name: "lb-2", Address: "10.0.0.2"}}
		c.SecretsFrom = "lb-1"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	version := func(fw string) {
		t.Helper()
		v, err := a.clusters.Store.NewVersion(c.ID, "admin", "", map[string][]byte{"haproxy.cfg": []byte("global\n"), "firewall.nft": []byte(fw)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.clusters.Store.Update(c.ID, func(c *clusters.Cluster) error { c.Desired = v.Version; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	lb3 := clusters.Member{NodeID: nodesByName["lb-3"].ID, Name: "lb-3", Address: "10.0.0.3"}
	version("table inet filter {\n  chain input {\n    type filter hook input priority 0; policy drop;\n    ip daddr {{ .Member.Address }} tcp dport 9505 accept\n  }\n}\n")
	mark := f.mark()
	err = a.clusters.Replace(ctx, c.ID, "lb-2", lb3, clusters.ReplaceOptions{KeepAddress: true}, "admin")
	if err == nil || !strings.Contains(err.Error(), "firewall.nft depends on lb-3's own address") || !strings.Contains(err.Error(), "iifname") {
		t.Errorf("a takeover under a firewall on the member's address: %v", err)
	}
	if calls := f.callsSince(mark); slices.ContainsFunc(calls, func(c string) bool { return !strings.HasSuffix(c, " NetworkConfig") }) {
		t.Errorf("touched before refusing: %v", calls)
	}
	err = a.clusters.Readdress(ctx, c.ID, "lb-2", clusters.Addressing{Addresses: []string{"10.0.0.40/24"}}, "admin")
	if err == nil || !strings.Contains(err.Error(), "firewall.nft depends on lb-2's own address") {
		t.Errorf("a move under a firewall on the member's address: %v", err)
	}
	if calls := f.callsSince(mark); slices.ContainsFunc(calls, func(c string) bool { return !strings.HasSuffix(c, " NetworkConfig") }) {
		t.Errorf("touched before refusing: %v", calls)
	}

	// The peers' addresses only: the takeover goes on.
	version("table inet filter {\n  chain input {\n    type filter hook input priority 0; policy accept;\n{{- range .Peers }}\n    ip saddr {{ .Address }} tcp dport 10057 accept\n{{- end }}\n  }\n}\n")
	if err := a.clusters.Readdress(ctx, c.ID, "lb-2", clusters.Addressing{Addresses: []string{"10.0.0.40/24"}}, "admin"); err != nil {
		t.Errorf("a move under a firewall naming the peers only: %v", err)
	}
}

// A machine member with a static address, recycled keeping it: a new
// machine on a temporary address, the old one destroyed, the new one
// moved to the member's address - its record following.
func TestClusterRecycleKeepingAddress(t *testing.T) {
	a, fake := newTestApp(t)
	h := addTrustedHypervisor(t, a)
	image := append([]byte("QFI\xfb\x00\x00\x00\x03"), make([]byte, 4096)...)
	sum := sha256.Sum256(image)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(image) }))
	defer srv.Close()
	st, err := clusters.Open(a.dataDir, testSealer{})
	if err != nil {
		t.Fatal(err)
	}
	a.clusters = newClusterEngine(a, st)
	f := &clusterFake{nodes: map[string]*fakeMember{}, fail: map[string]error{}, unreach: map[[2]string]bool{}, byAddr: map[string]string{}, silent: map[string]bool{}, notAfter: time.Now().Add(clusters.MemberValidity)}
	a.clusters.Nodes = f
	t.Cleanup(a.clusters.wait)
	useMovingNodes(t, f).cutShutdown = true
	// The machine destroyed, nothing answers at its address.
	a.clusters.Release = func(ctx context.Context, m clusters.Member) error {
		err := a.releaseMember(ctx, m)
		f.mu.Lock()
		f.silent[clusters.APIAddress(m)] = true
		f.mu.Unlock()
		return err
	}
	member := func(name string) {
		f.mu.Lock()
		f.nodes[name] = &fakeMember{schematic: "s1", healthy: true, cfg: []byte("global # " + name + "\n"), files: map[string][]byte{}, secret: map[string]bool{}, modules: map[string][]byte{}}
		f.mu.Unlock()
	}
	register := func(id, name, addr string) *machines.Machine {
		t.Helper()
		waitPhase(t, a, id, machines.PhaseRegistering)
		path := filepath.Join(t.TempDir(), "cidata.iso")
		if err := os.WriteFile(path, fake.ciData, 0o600); err != nil {
			t.Fatal(err)
		}
		ci, err := nocloud.Read(path)
		if err != nil {
			t.Fatal(err)
		}
		f.mu.Lock()
		f.byAddr[addr] = name
		f.mu.Unlock()
		req := registration(t, name, ci.RegistrationToken)
		req.Address = addr
		if rec := call(t, a.handleRegister, "POST", "/register", "/register", req); rec.Code != http.StatusCreated {
			t.Fatalf("register %s: %d %s", name, rec.Code, rec.Body)
		}
		m := waitPhase(t, a, id, machines.PhaseReady)
		if err := a.store.SetFleet(m.NodeID); err != nil {
			t.Fatal(err)
		}
		return m
	}
	nicOf := func(addr string) []machines.NIC {
		return []machines.NIC{{Network: "lab-mgmt", Name: "mgmt", Mode: "static", Addresses: []string{addr}, Gateway: "10.0.0.254"}}
	}
	machine := func(name, cidr string) *machines.Machine {
		t.Helper()
		rec := call(t, a.handleMachineCreate, "POST", "/api/machines", "POST /api/machines", machines.Spec{
			Name: name, HypervisorID: h.ID,
			Image: &machines.ImageSource{URL: srv.URL + "/janus-kvm.qcow2", SHA256: hex.EncodeToString(sum[:])},
			NICs:  nicOf(cidr),
		})
		var v machineView
		_ = json.Unmarshal(rec.Body.Bytes(), &v)
		member(name)
		m := register(v.ID, name, strings.Split(cidr, "/")[0]+":9505")
		f.mu.Lock()
		f.nodes[name].netcfg = &janusv1alpha1.NetworkConfig{Interfaces: []*janusv1alpha1.NetworkInterface{{Name: "mgmt", Mac: m.Spec.NICs[0].MAC,
			Mode: janusv1alpha1.AddressingMode_ADDRESSING_MODE_STATIC, Addresses: []string{cidr}, Gateway: "10.0.0.254"}}}
		f.mu.Unlock()
		return m
	}
	lb1, lb2 := machine("lb-1", "10.0.0.11/24"), machine("lb-2", "10.0.0.12/24")
	c, err := a.clusters.Store.Create("edge", 0, []string{"haproxy", "files"})
	if err != nil {
		t.Fatal(err)
	}
	c, _ = a.clusters.Store.Update(c.ID, func(c *clusters.Cluster) error {
		c.Members = []clusters.Member{{NodeID: lb1.NodeID, Name: "lb-1", Address: "10.0.0.11"}, {NodeID: lb2.NodeID, Name: "lb-2", Address: "10.0.0.12"}}
		c.SecretsFrom = "lb-1"
		return nil
	})
	ctx := context.Background()
	if err := a.clusters.Create(ctx, c.ID, "lb-1", "admin"); err != nil {
		t.Fatal(err)
	}

	// Without a temporary address, a static one is refused at once.
	if err := a.recycle(ctx, c.ID, recycleRequest{Members: []string{"lb-1"}}, "admin"); err == nil || !strings.Contains(err.Error(), "a temporary address") {
		t.Errorf("no temporary address: %v", err)
	}
	rec := call(t, a.handleClusterRecycle, "POST", "/api/clusters/"+c.ID+"/recycle", "POST /api/clusters/{id}/recycle", recycleRequest{Members: []string{"lb-1"}, TemporaryAddress: "10.0.0.30"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a temporary address without its prefix: %d %s", rec.Code, rec.Body)
	}
	// The request says it, before any machine is made.
	made := len(a.machines.List())
	rec = call(t, a.handleClusterRecycle, "POST", "/api/clusters/"+c.ID+"/recycle", "POST /api/clusters/{id}/recycle", recycleRequest{Members: []string{"lb-1"}, KeepAddress: true})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "a temporary address") {
		t.Errorf("no temporary address, by the request: %d %s", rec.Code, rec.Body)
	}
	rec = call(t, a.handleClusterRecycle, "POST", "/api/clusters/"+c.ID+"/recycle", "POST /api/clusters/{id}/recycle", recycleRequest{Members: []string{"lb-1"}})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "keep its address") {
		t.Errorf("a static address cloned, by the request: %d %s", rec.Code, rec.Body)
	}
	if n := len(a.machines.List()); n != made {
		t.Errorf("%d machines made before the refusals", n-made)
	}

	member("edge-1")
	done := make(chan error, 1)
	go func() {
		done <- a.recycle(ctx, c.ID, recycleRequest{Members: []string{"lb-1"}, TemporaryAddress: "10.0.0.30/24"}, "admin")
	}()
	var repl *machines.Machine
	deadline := time.Now().Add(10 * time.Second)
	for repl == nil && time.Now().Before(deadline) {
		for _, m := range a.machines.List() {
			if m.Spec.Name == "edge-1" {
				repl = m
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if repl == nil {
		t.Fatal("no new machine made")
	}
	if n := repl.Spec.NICs[0]; n.Mode != "static" || strings.Join(n.Addresses, " ") != "10.0.0.30/24" || n.Gateway != "10.0.0.254" {
		t.Errorf("the new machine's interface: %+v", n)
	}
	register(repl.ID, "edge-1", "10.0.0.30:9505")
	repl, _ = a.machines.Get(repl.ID)
	f.mu.Lock()
	f.nodes["edge-1"].netcfg = &janusv1alpha1.NetworkConfig{Interfaces: []*janusv1alpha1.NetworkInterface{{Name: "mgmt", Mac: repl.Spec.NICs[0].MAC,
		Mode: janusv1alpha1.AddressingMode_ADDRESSING_MODE_STATIC, Addresses: []string{"10.0.0.30/24"}, Gateway: "10.0.0.254"}}}
	f.mu.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	cur, _ := a.clusters.Store.Get(c.ID)
	if cur.Member("lb-1") != nil || cur.Member("edge-1") == nil || cur.Member("edge-1").Address != "10.0.0.11" {
		t.Errorf("members after: %+v", cur.Members)
	}
	if _, ok := a.machines.Get(lb1.ID); ok {
		t.Error("lb-1's machine not destroyed")
	}
	fake.mu.Lock()
	destroyed := slices.Clone(fake.destroyed)
	fake.mu.Unlock()
	if !slices.Contains(destroyed, "janus-lb-1") {
		t.Errorf("destroyed %v", destroyed)
	}
	repl, _ = a.machines.Get(repl.ID)
	if n := repl.Spec.NICs[0]; strings.Join(n.Addresses, " ") != "10.0.0.11/24" || n.Mode != "static" {
		t.Errorf("the new machine's record: %+v", n)
	}
	if n, _ := a.store.Get(repl.NodeID); n.Addr() != "10.0.0.11:9505" {
		t.Errorf("the Controller reaches edge-1 at %s", n.Addr())
	}
	journal := ""
	for _, e := range a.clusters.Store.Entries(c.ID, 100) {
		journal += e.Message + "\n"
	}
	if !strings.Contains(journal, "replaced by edge-1 at its address") || !strings.Contains(journal, "every member knows edge-1 at 10.0.0.11") {
		t.Errorf("the journal: %s", journal)
	}
}

// A clone taking its member's address over: the interface holding it on
// the temporary address until then, the others as cloneSpec makes them.
func TestCloneTakingOver(t *testing.T) {
	old := machines.Spec{Name: "lb-1", NICs: []machines.NIC{
		{Name: "mgmt", Mode: "dhcp", MAC: "52:54:00:00:00:01"},
		{Name: "lan", Mode: "static", MAC: "52:54:00:00:00:02", Addresses: []string{"192.0.2.11/24"}, Gateway: "192.0.2.1"},
	}}
	s, err := cloneTakingOver(old, "edge-1", "", "192.0.2.11", "192.0.2.30/24")
	if err != nil || s.NICs[1].Addresses[0] != "192.0.2.30/24" || s.NICs[1].Gateway != "192.0.2.1" || s.NICs[1].MAC != "" || old.NICs[1].Addresses[0] != "192.0.2.11/24" {
		t.Errorf("clone: %+v %v (old %+v)", s, err, old)
	}
	if s, err := cloneTakingOver(old, "edge-1", "", "192.0.2.11", "DHCP"); err != nil || s.NICs[1].Mode != "dhcp" || s.NICs[1].Addresses != nil || s.NICs[1].Gateway != "" {
		t.Errorf("on DHCP: %+v %v", s.NICs, err)
	}
	if _, err := cloneTakingOver(old, "edge-1", "", "192.0.2.11", ""); err == nil || !strings.Contains(err.Error(), "temporary address") {
		t.Errorf("no temporary address: %v", err)
	}
	if _, err := cloneTakingOver(old, "edge-1", "", "192.0.2.11", "192.0.2.11/24"); err == nil {
		t.Error("the member's own address as the temporary one")
	}
	if _, err := cloneTakingOver(old, "edge-1", "", "192.0.2.99", "192.0.2.30/24"); err == nil || !strings.Contains(err.Error(), "isn't a static one") {
		t.Errorf("an address DHCP gave: %v", err)
	}
	old.NICs = append(old.NICs, machines.NIC{Name: "back", Mode: "static", Addresses: []string{"10.9.0.11/24"}})
	if _, err := cloneTakingOver(old, "edge-1", "", "192.0.2.11", "192.0.2.30/24"); err == nil || !strings.Contains(err.Error(), "back") {
		t.Errorf("another static interface: %v", err)
	}
}

// A member moving keeps its VRRP out from before its address changes
// until every other member names it at its new one (#515's Proxmox
// proof: unicast VRRP, the others still naming the newcomer at its old
// address - neither heard the other, both went MASTER): stopped first,
// the others re-rendered, then its own keepalived back; a move that
// fails puts it back where it was.
func TestClusterMoveKeepsVRRPOut(t *testing.T) {
	a, f, nodesByName := clusterApp(t, "lb-1", "lb-2", "lb-3")
	mn := useMovingNodes(t, f)
	c, err := a.clusters.Store.Create("edge", 0, []string{"haproxy", "files", "vrrp"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.clusters.Store.Update(c.ID, func(c *clusters.Cluster) error {
		for i, n := range []string{"lb-1", "lb-2", "lb-3"} {
			c.Members = append(c.Members, clusters.Member{NodeID: nodesByName[n].ID, Name: n, Address: fmt.Sprintf("10.0.0.%d", i+1)})
		}
		c.SecretsFrom = "lb-1"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	v, err := a.clusters.Store.NewVersion(c.ID, "admin", "", map[string][]byte{"haproxy.cfg": []byte("global\n"),
		"keepalived.conf": []byte("vrrp_instance front {\n  unicast_src_ip {{ .Member.Address }}\n  unicast_peer {\n{{- range .Peers }}\n    {{ .Address }}\n{{- end }}\n  }\n}\n")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.clusters.Store.Update(c.ID, func(c *clusters.Cluster) error { c.Desired = v.Version; return nil }); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	to := clusters.Addressing{Addresses: []string{"10.0.0.40/24"}}

	// The move fails: its keepalived back, as it was.
	mn.fail = errors.New("couldn't confirm before the node reverts")
	mark := f.mark()
	if err := a.clusters.Readdress(ctx, c.ID, "lb-2", to, "admin"); err == nil {
		t.Fatal("a failed move succeeded")
	}
	if got := f.callsOf("lb-2", mark); !inOrder(got, "ClusterDrain", "ModuleApply vrrp stop", "ModuleApply vrrp", "ClusterUndrain") {
		t.Errorf("lb-2 after a failed move: %v", got)
	}
	f.mu.Lock()
	kept := string(f.nodes["lb-2"].modules["vrrp"])
	f.mu.Unlock()
	if !strings.Contains(kept, "unicast_src_ip 10.0.0.2") {
		t.Errorf("lb-2's keepalived.conf after a failed move: %q", kept)
	}
	mn.fail = nil

	mark = f.mark()
	if err := a.clusters.Readdress(ctx, c.ID, "lb-2", to, "admin"); err != nil {
		t.Fatal(err)
	}
	calls := f.callsSince(mark)
	if !inOrder(calls, "lb-2 ClusterDrain", "lb-2 ModuleApply vrrp stop", "lb-1 ModuleApply vrrp", "lb-3 ModuleApply vrrp", "lb-2 ModuleApply vrrp", "lb-2 ClusterUndrain") {
		t.Errorf("the order: %v", calls)
	}
	// Nothing started lb-2's keepalived before the others named it at its
	// new address.
	stop := slices.Index(calls, "lb-2 ModuleApply vrrp stop")
	back := slices.Index(calls[stop+1:], "lb-2 ModuleApply vrrp") + stop + 1
	for _, o := range []string{"lb-1 ModuleApply vrrp", "lb-3 ModuleApply vrrp"} {
		if i := slices.Index(calls[stop:], o); i < 0 || stop+i > back {
			t.Errorf("%s not before lb-2's keepalived came back: %v", o, calls)
		}
	}
	f.mu.Lock()
	lb1, lb2 := string(f.nodes["lb-1"].modules["vrrp"]), string(f.nodes["lb-2"].modules["vrrp"])
	f.mu.Unlock()
	if !strings.Contains(lb1, "10.0.0.40") || !strings.Contains(lb2, "unicast_src_ip 10.0.0.40") {
		t.Errorf("keepalived.conf after: lb-1 %q, lb-2 %q", lb1, lb2)
	}
	if !slices.Contains(mn.windows, clusters.MoveWindow) {
		t.Errorf("the move's confirmation windows: %v", mn.windows)
	}
}

// A member joining a cluster whose keepalived.conf names the peers: the
// others' rendered again first - only the modules that changed, no
// HAProxy reload - so its VRRP never starts unnamed (it would hear no
// advertisement and take the virtual IP).
func TestClusterJoinRendersTheOthersFirst(t *testing.T) {
	a, f, nodesByName := clusterApp(t, "lb-1", "lb-2", "lb-3")
	c, err := a.clusters.Store.Create("edge", 0, []string{"haproxy", "files", "vrrp"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.clusters.Store.Update(c.ID, func(c *clusters.Cluster) error {
		c.Members = []clusters.Member{{NodeID: nodesByName["lb-1"].ID, Name: "lb-1", Address: "10.0.0.1"}, {NodeID: nodesByName["lb-2"].ID, Name: "lb-2", Address: "10.0.0.2"}}
		c.SecretsFrom = "lb-1"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	v, err := a.clusters.Store.NewVersion(c.ID, "admin", "", map[string][]byte{"haproxy.cfg": []byte("global\n"),
		"keepalived.conf": []byte("vrrp_instance front {\n  unicast_src_ip {{ .Member.Address }}\n  unicast_peer {\n{{- range .Peers }}\n    {{ .Address }}\n{{- end }}\n  }\n}\n")})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := a.clusters.Apply(ctx, c.ID, v.Version, nil, "admin"); err != nil {
		t.Fatal(err)
	}
	mark := f.mark()
	if err := a.clusters.AddMember(ctx, c.ID, clusters.Member{NodeID: nodesByName["lb-3"].ID, Name: "lb-3", Address: "10.0.0.3"}, "admin"); err != nil {
		t.Fatal(err)
	}
	calls := f.callsSince(mark)
	if !inOrder(calls, "lb-1 ModuleApply vrrp", "lb-2 ModuleApply vrrp", "lb-3 ModuleApply vrrp") {
		t.Errorf("the others not rendered before lb-3's VRRP: %v", calls)
	}
	for _, o := range []string{"lb-1", "lb-2"} {
		if slices.Contains(f.callsOf(o, mark), "HAProxyApply") {
			t.Errorf("%s's HAProxy reloaded for a join: %v", o, f.callsOf(o, mark))
		}
	}
	f.mu.Lock()
	lb1 := string(f.nodes["lb-1"].modules["vrrp"])
	f.mu.Unlock()
	if !strings.Contains(lb1, "10.0.0.3") {
		t.Errorf("lb-1's keepalived.conf doesn't name lb-3: %q", lb1)
	}
	// Its state says so: no drift reported for it.
	if h := a.clusters.Store.State(c.ID).Members["lb-1"].Hashes["vrrp"]; h != clusters.Sum([]byte(lb1)) {
		t.Errorf("lb-1's recorded VRRP hash %s isn't what it runs", h)
	}
}

// What a takeover or a move can't do is refused by the request itself -
// 409, no job, no member or replacement touched -, not by the background
// job once a new machine was made for it (v2026.11.17's lab: a machine
// created, then destroyed, to hear "the cluster's only member").
func TestClusterAddressRefusalsAtOnce(t *testing.T) {
	a, f, nodesByName := clusterApp(t, "lb-1", "lb-2", "lb-3")
	useMovingNodes(t, f)
	for i, n := range []string{"lb-1", "lb-2", "lb-3"} {
		f.nodes[n].netcfg = &janusv1alpha1.NetworkConfig{Interfaces: []*janusv1alpha1.NetworkInterface{{Name: "lan",
			Mode: janusv1alpha1.AddressingMode_ADDRESSING_MODE_STATIC, Addresses: []string{fmt.Sprintf("10.0.0.%d/24", i+1)}}}}
	}
	c, err := a.clusters.Store.Create("edge", 0, []string{"haproxy", "files", "firewall"})
	if err != nil {
		t.Fatal(err)
	}
	members := func(names ...string) {
		t.Helper()
		var ms []clusters.Member
		for _, n := range names {
			ms = append(ms, clusters.Member{NodeID: nodesByName[n].ID, Name: n, Address: strings.TrimSuffix(nodesByName[n].Addr(), ":9505")})
		}
		if _, err := a.clusters.Store.Update(c.ID, func(c *clusters.Cluster) error { c.Members, c.SecretsFrom = ms, names[0]; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	version := func(fw string) {
		t.Helper()
		v, err := a.clusters.Store.NewVersion(c.ID, "admin", "", map[string][]byte{"haproxy.cfg": []byte("global\n"), "firewall.nft": []byte(fw)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.clusters.Store.Update(c.ID, func(c *clusters.Cluster) error { c.Desired = v.Version; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	peersOnly := "table inet filter {\n  chain input {\n    type filter hook input priority 0; policy accept;\n{{- range .Peers }}\n    ip saddr {{ .Address }} tcp dport 10057 accept\n{{- end }}\n  }\n}\n"
	onTheAddress := "table inet filter {\n  chain input {\n    type filter hook input priority 0; policy drop;\n    ip daddr {{ .Member.Address }} tcp dport 9505 accept\n  }\n}\n"
	recycle := func(req recycleRequest) *httptest.ResponseRecorder {
		return call(t, a.handleClusterRecycle, "POST", "/api/clusters/"+c.ID+"/recycle", "POST /api/clusters/{id}/recycle", req)
	}
	readdress := func(name string, to clusters.Addressing) *httptest.ResponseRecorder {
		return call(t, a.handleClusterReaddress, "POST", "/api/clusters/"+c.ID+"/members/"+name+"/address", "POST /api/clusters/{id}/members/{name}/address", to)
	}
	takeover := recycleRequest{Members: []string{"lb-1"}, KeepAddress: true, With: map[string]clusterMemberRequest{"lb-1": {NodeID: nodesByName["lb-3"].ID, Name: "lb-3", Address: "10.0.0.3"}}}
	away := clusters.Addressing{Addresses: []string{"10.0.0.40/24"}}
	refused := func(what string, rec *httptest.ResponseRecorder, says string) {
		t.Helper()
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), says) {
			t.Errorf("%s: %d %s", what, rec.Code, rec.Body)
		}
	}
	members("lb-1", "lb-2")
	version(peersOnly)
	mark := f.mark()

	// The member's address from DHCP.
	f.nodes["lb-1"].netcfg = &janusv1alpha1.NetworkConfig{Interfaces: []*janusv1alpha1.NetworkInterface{{Name: "lan", Mode: janusv1alpha1.AddressingMode_ADDRESSING_MODE_DHCP}}}
	refused("a DHCP address taken over", recycle(takeover), "DHCP")
	f.nodes["lb-1"].netcfg = &janusv1alpha1.NetworkConfig{Interfaces: []*janusv1alpha1.NetworkInterface{{Name: "lan",
		Mode: janusv1alpha1.AddressingMode_ADDRESSING_MODE_STATIC, Addresses: []string{"10.0.0.1/24"}}}}

	// A firewall on the member's own address.
	version(onTheAddress)
	refused("a takeover under a firewall on the member's address", recycle(takeover), "iifname")
	refused("a move under a firewall on the member's address", readdress("lb-2", away), "iifname")
	tmp := takeover
	tmp.TemporaryAddress = "10.0.0.30/24"
	refused("the same with a temporary address", recycle(tmp), "firewall.nft depends on the member's own address")
	version(peersOnly)

	// The cluster's only member.
	members("lb-1")
	refused("the only member taken over", recycle(takeover), "only member")
	refused("the only member moved", readdress("lb-1", away), "only member")
	members("lb-1", "lb-2")

	if calls := f.callsSince(mark); slices.ContainsFunc(calls, func(c string) bool { return !strings.HasSuffix(c, " NetworkConfig") }) {
		t.Errorf("touched before refusing: %v", calls)
	}
	a.clusters.mu.Lock()
	busy := a.clusters.busy[c.ID]
	a.clusters.mu.Unlock()
	if busy {
		t.Error("a job started for a refusal")
	}
	// Nothing to refuse: the request goes on to its job.
	cur, _ := a.clusters.Store.Get(c.ID)
	if err := a.checkRecycle(context.Background(), cur, takeover); err != nil {
		t.Errorf("a takeover the cluster allows: %v", err)
	}
	if err := a.clusters.CheckReaddress(c.ID, "lb-2", away); err != nil {
		t.Errorf("a move the cluster allows: %v", err)
	}
}
