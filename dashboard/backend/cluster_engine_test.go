package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/swenske/Janus/dashboard/backend/internal/store"
	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/clusters"
)

// fakeMember is a node as the cluster engine sees it.
type fakeMember struct {
	schematic string
	healthy   bool
	info      *janusv1alpha1.ClusterInfo
	cfg       []byte
	files     map[string][]byte
	secret    map[string]bool
	modules   map[string][]byte
	// learning: its HAProxy hasn't learnt its stick tables; alone: no
	// peer up.
	learning, alone bool
}

// clusterFake answers the cluster engine for every node, and keeps what
// it was asked, in order.
type clusterFake struct {
	mu       sync.Mutex
	nodes    map[string]*fakeMember // by node name
	calls    []string               // "node method[ detail]"
	fail     map[string]error       // "node method" -> error
	unreach  map[[2]string]bool     // node -> target name
	byAddr   map[string]string      // API address -> node name
	notAfter time.Time              // the member certificates' expiry ClusterGet reports
}

func (f *clusterFake) call(m clusters.Member, method, detail string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := m.Name + " " + method
	if detail != "" {
		c += " " + detail
	}
	f.calls = append(f.calls, c)
	return f.fail[m.Name+" "+method]
}

func (f *clusterFake) node(m clusters.Member) *fakeMember {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nodes[m.Name]
}

func (f *clusterFake) Version(_ context.Context, m clusters.Member) (*janusv1alpha1.VersionResponse, error) {
	if err := f.call(m, "Version", ""); err != nil {
		return nil, err
	}
	return &janusv1alpha1.VersionResponse{SchematicId: f.node(m).schematic}, nil
}

func (f *clusterFake) Healthy(_ context.Context, m clusters.Member) error {
	if err := f.call(m, "Healthy", ""); err != nil {
		return err
	}
	if !f.node(m).healthy {
		return errors.New("no answer on the stats socket")
	}
	return nil
}

func (f *clusterFake) Probe(_ context.Context, m clusters.Member, target string) error {
	if err := f.call(m, "Probe", target); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.unreach[[2]string{m.Name, f.byAddr[target]}] {
		return errors.New("connection timed out")
	}
	return nil
}

func (f *clusterFake) CertificateRequest(_ context.Context, m clusters.Member) ([]byte, error) {
	if err := f.call(m, "CertificateRequest", ""); err != nil {
		return nil, err
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), nil
}

func (f *clusterFake) ClusterGet(_ context.Context, m clusters.Member) (*janusv1alpha1.ClusterInfo, error) {
	if err := f.call(m, "ClusterGet", ""); err != nil {
		return nil, err
	}
	fm := f.node(m)
	f.mu.Lock()
	defer f.mu.Unlock()
	if fm.info == nil {
		return &janusv1alpha1.ClusterInfo{}, nil
	}
	info := proto.Clone(fm.info).(*janusv1alpha1.ClusterInfo)
	info.MemberCertNotAfterUnix = f.notAfter.Unix()
	return info, nil
}

func (f *clusterFake) ClusterSet(_ context.Context, m clusters.Member, req *janusv1alpha1.ClusterSetRequest) (*janusv1alpha1.ClusterInfo, error) {
	var names []string
	for _, m := range req.GetMembers() {
		names = append(names, m.GetName())
	}
	detail := strings.Join(names, ",")
	if req.GetMemberCert() != nil {
		detail += " +cert"
	}
	if err := f.call(m, "ClusterSet", detail); err != nil {
		return nil, err
	}
	fm := f.node(m)
	f.mu.Lock()
	defer f.mu.Unlock()
	drained := fm.info != nil && fm.info.GetDrained()
	fm.info = &janusv1alpha1.ClusterInfo{Member: true, Id: req.GetId(), Name: req.GetName(), MemberName: req.GetMemberName(), Managed: slices.Clone(req.GetManaged()), Drained: drained}
	return fm.info, nil
}

func (f *clusterFake) ClusterLeave(_ context.Context, m clusters.Member) error {
	if err := f.call(m, "ClusterLeave", ""); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nodes[m.Name].info = nil
	return nil
}

func (f *clusterFake) ClusterStatus(_ context.Context, m clusters.Member) (*janusv1alpha1.ClusterStatusResponse, error) {
	if err := f.call(m, "ClusterStatus", ""); err != nil {
		return nil, err
	}
	fm := f.node(m)
	f.mu.Lock()
	defer f.mu.Unlock()
	return &janusv1alpha1.ClusterStatusResponse{TablesLearnt: !fm.learning,
		Peers: []*janusv1alpha1.ClusterPeer{{Name: "x", Up: !fm.alone, Tables: []*janusv1alpha1.ClusterPeerTable{{Synced: true}}}}}, nil
}

func (f *clusterFake) ClusterDrain(_ context.Context, m clusters.Member, _ time.Duration) (string, error) {
	if err := f.call(m, "ClusterDrain", ""); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if m := f.nodes[m.Name]; m.info != nil {
		m.info.Drained = true
	}
	return "out of service", nil
}

func (f *clusterFake) ClusterUndrain(_ context.Context, m clusters.Member) error {
	return f.call(m, "ClusterUndrain", "")
}

func (f *clusterFake) ClusterPull(_ context.Context, m clusters.Member, from string, items []janusv1alpha1.ClusterItem) ([]string, error) {
	detail := from // "lb-1", "lb-1 runtime_certs" for some items only
	for _, it := range items {
		detail += " " + strings.ToLower(strings.TrimPrefix(it.String(), "CLUSTER_ITEM_"))
	}
	if err := f.call(m, "ClusterPull", detail); err != nil {
		return nil, err
	}
	return []string{"pulled"}, nil
}

func (f *clusterFake) RotateTickets(_ context.Context, m clusters.Member) error {
	return f.call(m, "RotateTickets", "")
}

func (f *clusterFake) HAProxyConfig(_ context.Context, m clusters.Member) ([]byte, error) {
	if err := f.call(m, "HAProxyConfig", ""); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nodes[m.Name].cfg, nil
}

func (f *clusterFake) HAProxyValidate(_ context.Context, m clusters.Member, cfg []byte) error {
	return f.call(m, "HAProxyValidate", "")
}

func (f *clusterFake) HAProxyApply(_ context.Context, m clusters.Member, cfg []byte) error {
	if err := f.call(m, "HAProxyApply", ""); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nodes[m.Name].cfg = cfg
	return nil
}

func (f *clusterFake) Files(_ context.Context, m clusters.Member) ([]*janusv1alpha1.HAProxyFile, error) {
	if err := f.call(m, "Files", ""); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	fm := f.nodes[m.Name]
	var out []*janusv1alpha1.HAProxyFile
	for name, b := range fm.files {
		out = append(out, &janusv1alpha1.HAProxyFile{Name: name, Sha256: clusters.Sum(b), Secret: fm.secret[name]})
	}
	return out, nil
}

func (f *clusterFake) FileGet(_ context.Context, m clusters.Member, name string) ([]byte, error) {
	if err := f.call(m, "FileGet", name); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nodes[m.Name].files[name], nil
}

func (f *clusterFake) FilePut(_ context.Context, m clusters.Member, name string, data []byte) error {
	if err := f.call(m, "FilePut", name); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nodes[m.Name].files[name] = data
	return nil
}

func (f *clusterFake) FileDelete(_ context.Context, m clusters.Member, name string) error {
	if err := f.call(m, "FileDelete", name); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.nodes[m.Name].files, name)
	return nil
}

func (f *clusterFake) CertificateUpload(_ context.Context, m clusters.Member, req *janusv1alpha1.CertificateUploadRequest) error {
	return f.call(m, "CertificateUpload", req.GetName())
}

func (f *clusterFake) ModuleConfig(_ context.Context, m clusters.Member, domain string) ([]byte, error) {
	if err := f.call(m, "ModuleConfig", domain); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nodes[m.Name].modules[domain], nil
}

func (f *clusterFake) ModuleApply(_ context.Context, m clusters.Member, domain string, cfg []byte, validateOnly bool) error {
	detail := domain
	if validateOnly {
		detail += " check"
	}
	if err := f.call(m, "ModuleApply", detail); err != nil {
		return err
	}
	if !validateOnly {
		f.mu.Lock()
		f.nodes[m.Name].modules[domain] = cfg
		f.mu.Unlock()
	}
	return nil
}

// callsOf are the calls made to node, method by method, from mark on.
func (f *clusterFake) callsOf(node string, mark int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls[mark:] {
		if rest, ok := strings.CutPrefix(c, node+" "); ok {
			out = append(out, rest)
		}
	}
	return out
}

// callsSince are every node's calls since mark, "node method[ detail]".
func (f *clusterFake) callsSince(mark int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls[mark:])
}

func (f *clusterFake) mark() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// clusterApp is an app with nodes lb-1..n, trusting the fleet, and the
// cluster engine on a fake.
func clusterApp(t *testing.T, names ...string) (*app, *clusterFake, map[string]*store.Node) {
	t.Helper()
	a, _ := newTestApp(t)
	st, err := clusters.Open(a.dataDir, testSealer{})
	if err != nil {
		t.Fatal(err)
	}
	a.clusters = newClusterEngine(a, st)
	f := &clusterFake{nodes: map[string]*fakeMember{}, fail: map[string]error{}, unreach: map[[2]string]bool{}, byAddr: map[string]string{}, notAfter: time.Now().Add(clusters.MemberValidity)}
	nodesByName := map[string]*store.Node{}
	for i, name := range names {
		n := &store.Node{Name: name, Address: fmt.Sprintf("10.0.0.%d:9505", i+1)}
		if err := a.store.Add(n); err != nil {
			t.Fatal(err)
		}
		if err := a.store.SetFleet(n.ID); err != nil {
			t.Fatal(err)
		}
		n, _ = a.store.Get(n.ID)
		nodesByName[name] = n
		f.nodes[name] = &fakeMember{schematic: "s1", healthy: true, cfg: []byte("global # " + name + "\n"), files: map[string][]byte{}, secret: map[string]bool{}, modules: map[string][]byte{}}
		f.byAddr[fmt.Sprintf("10.0.0.%d:9505", i+1)] = name
	}
	a.clusters.Nodes = f
	t.Cleanup(a.clusters.wait)
	return a, f, nodesByName
}

type testSealer struct{}

func (testSealer) Seal(p []byte, purpose string) ([]byte, error) {
	return append([]byte(purpose+"|"), p...), nil
}
func (testSealer) Open(s []byte, purpose string) ([]byte, error) {
	rest, ok := strings.CutPrefix(string(s), purpose+"|")
	if !ok {
		return nil, errors.New("another purpose")
	}
	return []byte(rest), nil
}

// newCluster records a cluster of the named nodes, lb-1 the source.
func newCluster(t *testing.T, a *app, nodes map[string]*store.Node, names ...string) *clusters.Cluster {
	t.Helper()
	c, err := a.clusters.Store.Create("edge", 0, []string{"haproxy", "files"})
	if err != nil {
		t.Fatal(err)
	}
	var members []clusters.Member
	for i, name := range names {
		members = append(members, clusters.Member{NodeID: nodes[name].ID, Name: name, Address: fmt.Sprintf("10.0.0.%d", i+1)})
	}
	c, err = a.clusters.Store.Update(c.ID, func(c *clusters.Cluster) error {
		c.Members, c.SecretsFrom = members, names[0]
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// inOrder: calls has want, in this order (other calls between them).
func inOrder(calls []string, want ...string) bool {
	i := 0
	for _, c := range calls {
		if i < len(want) && c == want[i] {
			i++
		}
	}
	return i == len(want)
}

func TestClusterCreate(t *testing.T) {
	a, f, nodes := clusterApp(t, "lb-1", "lb-2", "lb-3")
	f.nodes["lb-1"].files["maps/hosts.map"] = []byte("example.test be\n")
	f.nodes["lb-1"].files["certs/www.pem"] = []byte("key")
	f.nodes["lb-1"].secret["certs/www.pem"] = true
	c := newCluster(t, a, nodes, "lb-1", "lb-2", "lb-3")
	ctx := context.Background()
	if err := a.clusters.Create(ctx, c.ID, "lb-1", "admin"); err != nil {
		t.Fatal(err)
	}
	c, _ = a.clusters.Store.Get(c.ID)
	if c.Desired != 1 {
		t.Fatalf("desired %d", c.Desired)
	}
	v, _ := a.clusters.Store.Version(c.ID, 1)
	if string(v.Content["haproxy.cfg"]) != "global # lb-1\n" || string(v.Content["files/maps/hosts.map"]) != "example.test be\n" {
		t.Errorf("version 1 isn't lb-1's: %v", v.Files)
	}
	if _, ok := v.Content["files/certs/www.pem"]; ok {
		t.Error("a file holding a key reached the Controller")
	}
	if !strings.Contains(v.Note, "1 file(s) holding a key stay there") {
		t.Errorf("note: %q", v.Note)
	}
	for _, name := range []string{"lb-1", "lb-2", "lb-3"} {
		calls := f.callsOf(name, 0)
		if !slices.Contains(calls, "CertificateRequest") || !slices.Contains(calls, "ClusterSet lb-1,lb-2,lb-3 +cert") {
			t.Errorf("%s didn't join: %v", name, calls)
		}
		if string(f.nodes[name].cfg) != "global # lb-1\n" || string(f.nodes[name].files["maps/hosts.map"]) != "example.test be\n" {
			t.Errorf("%s doesn't run version 1", name)
		}
		pulled := slices.Contains(calls, "ClusterPull lb-1")
		if pulled != (name != "lb-1") {
			t.Errorf("%s pulled: %v", name, pulled)
		}
		if pulled && !inOrder(calls, "ClusterPull lb-1", "HAProxyApply", "ClusterPull lb-1 runtime_certs") {
			t.Errorf("%s: the secrets, the version, the certificates again - got %v", name, calls)
		}
		if st := a.clusters.Store.State(c.ID).Members[name]; st.Applied != 1 || st.Hashes["haproxy"] == "" {
			t.Errorf("%s's state: %+v", name, st)
		}
	}
	// Checked before anything: each member reached the others' API.
	if !slices.Contains(f.callsOf("lb-2", 0), "Probe 10.0.0.3:9505") {
		t.Errorf("lb-2 didn't probe lb-3: %v", f.callsOf("lb-2", 0))
	}
	if len(a.clusters.Store.Entries(c.ID, 100)) < 7 {
		t.Error("the journal says little")
	}
}

func TestClusterCreateRefusals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(f *clusterFake, a *app, nodes map[string]*store.Node)
		want  string
	}{
		{"another image", func(f *clusterFake, _ *app, _ map[string]*store.Node) { f.nodes["lb-2"].schematic = "s2" }, "another image"},
		{"HAProxy down", func(f *clusterFake, _ *app, _ map[string]*store.Node) { f.nodes["lb-2"].healthy = false }, "HAProxy doesn't answer"},
		{"a member another cluster has", func(f *clusterFake, _ *app, _ map[string]*store.Node) {
			f.nodes["lb-2"].info = &janusv1alpha1.ClusterInfo{Member: true, Id: "other", Name: "core"}
		}, "member of cluster core"},
		{"no way between them", func(f *clusterFake, _ *app, _ map[string]*store.Node) { f.unreach[[2]string{"lb-2", "lb-1"}] = true }, "doesn't reach lb-1's API"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, f, nodes := clusterApp(t, "lb-1", "lb-2")
			tc.setup(f, a, nodes)
			c := newCluster(t, a, nodes, "lb-1", "lb-2")
			err := a.clusters.Create(context.Background(), c.ID, "lb-1", "admin")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
			if slices.ContainsFunc(f.calls, func(c string) bool { return strings.Contains(c, "ClusterSet") }) {
				t.Error("a member joined although the checks refused")
			}
		})
	}
}

const keepalivedTmpl = "vrrp_instance v { priority {{ .Member.Vars.priority }} }\n"

func TestClusterApply(t *testing.T) {
	a, f, nodes := clusterApp(t, "lb-1", "lb-2", "lb-3")
	f.nodes["lb-1"].files["maps/old.map"] = []byte("old\n")
	c := newCluster(t, a, nodes, "lb-1", "lb-2", "lb-3")
	ctx := context.Background()
	if err := a.clusters.Create(ctx, c.ID, "lb-1", "admin"); err != nil {
		t.Fatal(err)
	}
	// Version 2: a new haproxy.cfg, a new map, the old one gone, VRRP.
	c, _ = a.clusters.Store.Update(c.ID, func(c *clusters.Cluster) error {
		for i := range c.Members {
			c.Members[i].Vars = map[string]string{"priority": fmt.Sprint(200 - 50*i)}
		}
		c.Managed = []string{"haproxy", "files", "vrrp"}
		return nil
	})
	v2, err := a.clusters.Store.NewVersion(c.ID, "admin", "", map[string][]byte{
		"haproxy.cfg": []byte("global # v2\n"), "files/maps/new.map": []byte("new\n"), "keepalived.conf": []byte(keepalivedTmpl),
	})
	if err != nil {
		t.Fatal(err)
	}
	mark := f.mark()
	if err := a.clusters.Apply(ctx, c.ID, v2.Version, nil, "admin"); err != nil {
		t.Fatal(err)
	}
	// Checked on the first member before any is touched; then, on each:
	// the locks back, the files, HAProxy, the old file gone, VRRP - and
	// its stick tables learnt back before the next member.
	want := []string{"HAProxyValidate", "ModuleApply vrrp check", "ClusterSet lb-1,lb-2,lb-3", "Files", "FilePut maps/new.map", "HAProxyApply", "FileDelete maps/old.map", "ModuleApply vrrp", "ClusterStatus"}
	if got := f.callsOf("lb-1", mark); !slices.Equal(got, want) {
		t.Errorf("lb-1:\n got  %v\n want %v", got, want)
	}
	for i, name := range []string{"lb-1", "lb-2", "lb-3"} {
		if got := string(f.nodes[name].modules["vrrp"]); got != fmt.Sprintf("vrrp_instance v { priority %d }\n", 200-50*i) {
			t.Errorf("%s's keepalived.conf: %q", name, got)
		}
	}
	if c, _ = a.clusters.Store.Get(c.ID); c.Desired != 2 {
		t.Errorf("desired %d", c.Desired)
	}
	// Version 3 fails on lb-2: lb-1 runs it, lb-2 and lb-3 version 2.
	v3, _ := a.clusters.Store.NewVersion(c.ID, "admin", "", map[string][]byte{
		"haproxy.cfg": []byte("global # v3\n"), "files/maps/new.map": []byte("new\n"), "keepalived.conf": []byte(keepalivedTmpl),
	})
	f.fail["lb-2 HAProxyApply"] = errors.New("haproxy.cfg rejected: unknown keyword")
	err = a.clusters.Apply(ctx, c.ID, v3.Version, nil, "admin")
	if err == nil || !strings.Contains(err.Error(), "partially applied - lb-2") || !strings.Contains(err.Error(), "on lb-1") {
		t.Fatalf("got %v", err)
	}
	st := a.clusters.Store.State(c.ID)
	if st.Members["lb-1"].Applied != 3 || st.Members["lb-2"].Applied != 2 || st.Members["lb-3"].Applied != 2 || st.Members["lb-2"].Error == "" {
		t.Errorf("state after a failure: %+v", st.Members)
	}
	if string(f.nodes["lb-3"].cfg) != "global # v2\n" {
		t.Error("lb-3 was touched after lb-2 failed")
	}
	if c, _ = a.clusters.Store.Get(c.ID); c.Desired != 2 {
		t.Errorf("a failed apply moved the desired version: %d", c.Desired)
	}
	// A version without the template a managed module needs: refused whole.
	v4, _ := a.clusters.Store.NewVersion(c.ID, "admin", "", map[string][]byte{"haproxy.cfg": []byte("global\n")})
	mark = f.mark()
	if err := a.clusters.Apply(ctx, c.ID, v4.Version, nil, "admin"); err == nil || !strings.Contains(err.Error(), "configures no vrrp") {
		t.Errorf("got %v", err)
	}
	if f.mark() != mark {
		t.Error("a refused version touched a member")
	}
}

func TestClusterReconcile(t *testing.T) {
	a, f, nodes := clusterApp(t, "lb-1", "lb-2")
	c := newCluster(t, a, nodes, "lb-1", "lb-2")
	ctx := context.Background()
	if err := a.clusters.Create(ctx, c.ID, "lb-1", "admin"); err != nil {
		t.Fatal(err)
	}
	c, _ = a.clusters.Store.Get(c.ID)
	// Someone changed lb-2's haproxy.cfg after releasing its lock.
	f.nodes["lb-2"].cfg = []byte("global # by hand\n")
	f.nodes["lb-2"].info.Managed = []string{"files"}
	if again := a.clusters.reconcile(ctx, c); again {
		t.Errorf("an error: %+v", a.clusters.Store.State(c.ID))
	}
	st := a.clusters.Store.State(c.ID).Members["lb-2"]
	if !slices.Equal(st.Drift, []string{"haproxy"}) || !slices.Equal(st.Released, []string{"haproxy"}) || st.PeersUp != 1 || !st.TablesSynced {
		t.Errorf("lb-2: %+v", st)
	}
	if string(f.nodes["lb-2"].cfg) != "global # by hand\n" {
		t.Error("the loop overwrote a drift")
	}
	if st1 := a.clusters.Store.State(c.ID).Members["lb-1"]; len(st1.Drift) > 0 {
		t.Errorf("lb-1 drifted: %+v", st1)
	}
	view := a.clusterView(c)
	if !slices.ContainsFunc(view.Problems, func(p string) bool { return strings.Contains(p, "lb-2 has drifted: haproxy") }) {
		t.Errorf("problems: %v", view.Problems)
	}
	// The next apply takes it back, and the lock with it.
	if err := a.clusters.Apply(ctx, c.ID, 1, nil, "admin"); err != nil {
		t.Fatal(err)
	}
	if string(f.nodes["lb-2"].cfg) != "global # lb-1\n" || !slices.Contains(f.nodes["lb-2"].info.Managed, "haproxy") {
		t.Error("the apply didn't take lb-2 back")
	}
	a.clusters.reconcile(ctx, c)
	if st := a.clusters.Store.State(c.ID).Members["lb-2"]; len(st.Drift) > 0 || len(st.Released) > 0 {
		t.Errorf("after the apply: %+v", st)
	}
	// A member certificate past two thirds: renewed.
	f.notAfter = time.Now().Add(clusters.MemberValidity / 4)
	mark := f.mark()
	a.clusters.reconcile(ctx, c)
	if !slices.Contains(f.callsOf("lb-1", mark), "ClusterSet lb-1,lb-2 +cert") {
		t.Errorf("not renewed: %v", f.callsOf("lb-1", mark))
	}
	// The ticket ring: rotated on lb-1, pulled by lb-2, once a day.
	if !slices.Contains(f.callsOf("lb-1", 0), "RotateTickets") || !slices.Contains(f.callsOf("lb-2", 0), "ClusterPull lb-1 tls_ticket_keys") {
		t.Error("the ticket ring wasn't rotated")
	}
}

func TestClusterMembers(t *testing.T) {
	a, f, nodes := clusterApp(t, "lb-1", "lb-2", "lb-3")
	c := newCluster(t, a, nodes, "lb-1", "lb-2")
	ctx := context.Background()
	if err := a.clusters.Create(ctx, c.ID, "lb-1", "admin"); err != nil {
		t.Fatal(err)
	}
	// lb-3 joins: checked, signed, the others told, the secrets - the
	// version may name a file holding a key -, the version, the
	// certificates again.
	mark := f.mark()
	if err := a.clusters.AddMember(ctx, c.ID, clusters.Member{NodeID: nodes["lb-3"].ID, Name: "lb-3", Address: "10.0.0.3"}, "admin"); err != nil {
		t.Fatal(err)
	}
	if got := f.callsOf("lb-1", mark); !slices.Contains(got, "ClusterSet lb-1,lb-2,lb-3") {
		t.Errorf("lb-1 wasn't told of lb-3: %v", got)
	}
	got3 := f.callsOf("lb-3", mark)
	if !inOrder(got3, "CertificateRequest", "ClusterSet lb-1,lb-2,lb-3 +cert", "ClusterPull lb-1", "HAProxyApply", "ClusterPull lb-1 runtime_certs") {
		t.Errorf("lb-3: %v", got3)
	}
	// lb-1 leaves: drained, out, the secrets from another, the others told.
	mark = f.mark()
	if err := a.clusters.RemoveMember(ctx, c.ID, "lb-1", false, "admin"); err != nil {
		t.Fatal(err)
	}
	if got := f.callsOf("lb-1", mark); !slices.Equal(got, []string{"ClusterDrain", "ClusterLeave"}) {
		t.Errorf("lb-1: %v", got)
	}
	c, _ = a.clusters.Store.Get(c.ID)
	if c.Member("lb-1") != nil || c.SecretsFrom != "lb-2" {
		t.Errorf("after lb-1 left: %+v", c)
	}
	if !slices.Contains(f.callsOf("lb-3", mark), "ClusterSet lb-2,lb-3") {
		t.Errorf("lb-3 wasn't told: %v", f.callsOf("lb-3", mark))
	}
	// A member that doesn't drain stays, unless forced.
	f.fail["lb-2 ClusterDrain"] = errors.New("still MASTER of front")
	if err := a.clusters.RemoveMember(ctx, c.ID, "lb-2", false, "admin"); err == nil || !strings.Contains(err.Error(), "force") {
		t.Errorf("got %v", err)
	}
	if err := a.clusters.RemoveMember(ctx, c.ID, "lb-2", true, "admin"); err != nil {
		t.Errorf("forced: %v", err)
	}
	// The last member: the cluster goes instead.
	if err := a.clusters.RemoveMember(ctx, c.ID, "lb-3", false, "admin"); err == nil {
		t.Error("the last member removed")
	}
	if err := a.clusters.Remove(ctx, c.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.clusters.Store.Get(c.ID); ok || f.nodes["lb-3"].info != nil {
		t.Error("the cluster stayed")
	}
}

// A rolling apply waits, on each member, until its HAProxy has learnt the
// stick tables back from its peers before reloading the next: reloads
// inside HAProxy's resync window would leave no one to teach the others.
func TestClusterApplyWaitsForTheTables(t *testing.T) {
	a, f, nodes := clusterApp(t, "lb-1", "lb-2", "lb-3")
	c := newCluster(t, a, nodes, "lb-1", "lb-2", "lb-3")
	ctx := context.Background()
	if err := a.clusters.Create(ctx, c.ID, "lb-1", "admin"); err != nil {
		t.Fatal(err)
	}
	a.clusters.LearnTimeout = 1500 * time.Millisecond
	files := map[string][]byte{"haproxy.cfg": []byte("global # v2\n")}
	v, err := a.clusters.Store.NewVersion(c.ID, "admin", "v2", files)
	if err != nil {
		t.Fatal(err)
	}
	mark := f.mark()
	if err := a.clusters.Apply(ctx, c.ID, v.Version, nil, "admin"); err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, call := range f.callsSince(mark) {
		if strings.HasSuffix(call, " HAProxyApply") || strings.HasSuffix(call, " ClusterStatus") {
			order = append(order, call)
		}
	}
	if !inOrder(order, "lb-1 HAProxyApply", "lb-1 ClusterStatus", "lb-2 HAProxyApply", "lb-2 ClusterStatus", "lb-3 HAProxyApply", "lb-3 ClusterStatus") {
		t.Errorf("each member's tables before the next one's reload: %v", order)
	}

	// lb-2 doesn't learn while a peer is up: the apply stops there.
	f.nodes["lb-2"].learning = true
	v3, _ := a.clusters.Store.NewVersion(c.ID, "admin", "v3", map[string][]byte{"haproxy.cfg": []byte("global # v3\n")})
	mark = f.mark()
	err = a.clusters.Apply(ctx, c.ID, v3.Version, nil, "admin")
	if err == nil || !strings.Contains(err.Error(), "not learnt") || !strings.Contains(err.Error(), "partially applied - lb-2") {
		t.Errorf("lb-2 not learning: %v", err)
	}
	if slices.Contains(f.callsOf("lb-3", mark), "HAProxyApply") {
		t.Error("lb-3 reloaded while lb-2 hadn't its tables")
	}
	// With no peer up there's no one to learn from: on with the next.
	f.nodes["lb-2"].alone = true
	if err := a.clusters.Apply(ctx, c.ID, v3.Version, nil, "admin"); err != nil {
		t.Errorf("lb-2 alone: %v", err)
	}
	found := false
	for _, e := range a.clusters.Store.Entries(c.ID, 50) {
		if strings.Contains(e.Message, "lb-2 has no peer up") {
			found = true
		}
	}
	if !found {
		t.Error("the journal doesn't say lb-2 had no peer to learn from")
	}
}
