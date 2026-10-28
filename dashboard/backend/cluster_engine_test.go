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
	"net/netip"
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
	// peer up; drained: the node's own, member or not.
	learning, alone, drained bool
	// undrained: when ClusterUndrain last ran (ClusterInfo.undrained_unix).
	undrained int64
	// version and slot: what it runs; boot: when it booted; down: it
	// doesn't answer (rebooting); masters: its VRRP MASTER instances;
	// learnAfter: ClusterStatus calls before learning is over.
	version, slot string
	boot          time.Time
	down          bool
	masters       []string
	learnAfter    int
	// announceAfter: Announcing calls before it announces its routes;
	// downFor: BootTime calls it doesn't answer (rebooting).
	announceAfter, downFor int
	// acme: its letsencrypt extension; nil: not in its image.
	acme *janusv1alpha1.ACMEStatusResponse
	// set: what the last ClusterSet said.
	set *janusv1alpha1.ClusterSetRequest
	// addrs: the addresses on its interfaces besides its member address
	// (a link-local one, as a router's IPv6 route names).
	addrs []netip.Addr
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
	fm := f.node(m)
	f.mu.Lock()
	defer f.mu.Unlock()
	if fm.down {
		return nil, errors.New("connection refused")
	}
	return &janusv1alpha1.VersionResponse{SchematicId: fm.schematic, Version: fm.version, ActiveSlot: fm.slot}, nil
}

func (f *clusterFake) BootTime(_ context.Context, m clusters.Member) (time.Time, error) {
	if err := f.call(m, "BootTime", ""); err != nil {
		return time.Time{}, err
	}
	fm := f.node(m)
	f.mu.Lock()
	defer f.mu.Unlock()
	if fm.down {
		return time.Time{}, errors.New("connection refused")
	}
	if fm.downFor > 0 {
		fm.downFor--
		return time.Time{}, errors.New("connection refused")
	}
	return fm.boot, nil
}

func (f *clusterFake) Announcing(_ context.Context, m clusters.Member) (string, error) {
	if err := f.call(m, "Announcing", ""); err != nil {
		return "", err
	}
	fm := f.node(m)
	f.mu.Lock()
	defer f.mu.Unlock()
	if fm.announceAfter > 0 {
		fm.announceAfter--
		return "no BGP session established exports its routes yet", nil
	}
	return "", nil
}

func (f *clusterFake) VRRPMasters(_ context.Context, m clusters.Member) ([]string, error) {
	if err := f.call(m, "VRRPMasters", ""); err != nil {
		return nil, err
	}
	fm := f.node(m)
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(fm.masters), nil
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

func (f *clusterFake) StateTransferRequest(_ context.Context, m clusters.Member) ([]byte, error) {
	if err := f.call(m, "StateTransferRequest", ""); err != nil {
		return nil, err
	}
	return []byte("csr of " + m.Name), nil
}

func (f *clusterFake) StateExportGrant(_ context.Context, m clusters.Member, csr []byte) ([]byte, []byte, error) {
	if err := f.call(m, "StateExportGrant", string(csr)); err != nil {
		return nil, nil, err
	}
	return []byte("grant of " + m.Name), []byte("ca of " + m.Name), nil
}

// StateImportFrom copies the node at addr's configuration and files onto
// m, as the pull and the restart into it would.
func (f *clusterFake) StateImportFrom(_ context.Context, m clusters.Member, addr string, ca, cert []byte) (*janusv1alpha1.StateImportResponse, error) {
	if err := f.call(m, "StateImportFrom", addr+" "+string(cert)); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	from, ok := f.nodes[f.byAddr[addr]]
	if !ok {
		return nil, fmt.Errorf("nothing answers on %s", addr)
	}
	to := f.nodes[m.Name]
	to.cfg = slices.Clone(from.cfg)
	var names []string
	for k, v := range from.files {
		to.files[k] = slices.Clone(v)
		names = append(names, "haproxy/files/"+k)
	}
	return &janusv1alpha1.StateImportResponse{Files: append(names, "haproxy/haproxy.cfg"), Rebooting: true}, nil
}

func (f *clusterFake) ClusterGet(_ context.Context, m clusters.Member) (*janusv1alpha1.ClusterInfo, error) {
	if err := f.call(m, "ClusterGet", ""); err != nil {
		return nil, err
	}
	fm := f.node(m)
	f.mu.Lock()
	defer f.mu.Unlock()
	if fm.info == nil {
		return &janusv1alpha1.ClusterInfo{Drained: fm.drained, UndrainedUnix: fm.undrained}, nil
	}
	info := proto.Clone(fm.info).(*janusv1alpha1.ClusterInfo)
	info.MemberCertNotAfterUnix = f.notAfter.Unix()
	info.Drained, info.UndrainedUnix = fm.drained, fm.undrained
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
	fm.set = proto.Clone(req).(*janusv1alpha1.ClusterSetRequest)
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
	if fm.learnAfter > 0 {
		if fm.learnAfter--; fm.learnAfter == 0 {
			fm.learning = false
		}
	}
	return &janusv1alpha1.ClusterStatusResponse{TablesLearnt: !fm.learning,
		Peers: []*janusv1alpha1.ClusterPeer{{Name: "x", Up: !fm.alone, Tables: []*janusv1alpha1.ClusterPeerTable{{Synced: true}}}}}, nil
}

func (f *clusterFake) ClusterDrain(_ context.Context, m clusters.Member, _ time.Duration) (string, error) {
	if err := f.call(m, "ClusterDrain", ""); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nodes[m.Name].drained = true
	return "out of service", nil
}

func (f *clusterFake) ClusterUndrain(_ context.Context, m clusters.Member) error {
	if err := f.call(m, "ClusterUndrain", ""); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nodes[m.Name].drained = false
	f.nodes[m.Name].undrained = time.Now().Unix()
	return nil
}

func (f *clusterFake) ClusterPull(_ context.Context, m clusters.Member, from string, items []janusv1alpha1.ClusterItem) ([]string, error) {
	detail := from // "lb-1", "lb-1 runtime_certs" for some items only
	for _, it := range items {
		detail += " " + strings.ToLower(strings.TrimPrefix(it.String(), "CLUSTER_ITEM_"))
	}
	if err := f.call(m, "ClusterPull", detail); err != nil {
		return nil, err
	}
	// The ACME certificates: the source's, as the pull would install them.
	if slices.Contains(items, janusv1alpha1.ClusterItem_CLUSTER_ITEM_ACME_CERTS) || len(items) == 0 {
		f.mu.Lock()
		if src, to := f.nodes[from], f.nodes[m.Name]; src != nil && to != nil && src.acme != nil && to.acme != nil {
			to.acme.Certificates = nil
			for _, c := range src.acme.Certificates {
				to.acme.Certificates = append(to.acme.Certificates, proto.Clone(c).(*janusv1alpha1.ACMECertificateStatus))
			}
		}
		f.mu.Unlock()
	}
	return []string{"pulled"}, nil
}

func (f *clusterFake) ACMEStatus(_ context.Context, m clusters.Member) (*janusv1alpha1.ACMEStatusResponse, error) {
	if err := f.call(m, "ACMEStatus", ""); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if a := f.nodes[m.Name].acme; a != nil {
		out := proto.Clone(a).(*janusv1alpha1.ACMEStatusResponse)
		if s := f.nodes[m.Name].set; s != nil && s.GetAcmeOrderer() != "" && s.GetAcmeOrderer() != m.Name {
			out.OrderedBy = s.GetAcmeOrderer()
			if s.GetAcmeThumbprint() != "" {
				out.AccountThumbprint = s.GetAcmeThumbprint()
			}
		}
		return out, nil
	}
	return &janusv1alpha1.ACMEStatusResponse{State: janusv1alpha1.ModuleState_MODULE_STATE_NOT_ENABLED}, nil
}

func (f *clusterFake) RotateTickets(_ context.Context, m clusters.Member, renew bool) error {
	if renew {
		return f.call(m, "RotateTickets", "renew")
	}
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

// A member that leaves knew the TLS ticket key ring: the members that
// stay get a new one - every key replaced on the member the secrets come
// from, pulled by the others. A renewal that fails is retried by the
// next check, and said meanwhile.
func TestClusterTicketsRenewedWhenAMemberLeaves(t *testing.T) {
	a, f, nodes := clusterApp(t, "lb-1", "lb-2", "lb-3")
	c := newCluster(t, a, nodes, "lb-1", "lb-2", "lb-3")
	ctx := context.Background()
	if err := a.clusters.Create(ctx, c.ID, "lb-1", "admin"); err != nil {
		t.Fatal(err)
	}
	mark := f.mark()
	if err := a.clusters.RemoveMember(ctx, c.ID, "lb-3", false, "admin"); err != nil {
		t.Fatal(err)
	}
	if got := f.callsOf("lb-1", mark); !inOrder(got, "ClusterSet lb-1,lb-2", "RotateTickets renew") {
		t.Errorf("lb-1: %v", got)
	}
	if got := f.callsOf("lb-2", mark); !slices.ContainsFunc(got, func(c string) bool {
		return strings.HasPrefix(c, "ClusterPull lb-1") && strings.Contains(c, "tls_ticket_keys")
	}) {
		t.Errorf("lb-2 didn't pull the ring: %v", got)
	}
	if st := a.clusters.Store.State(c.ID); len(st.TicketsRenew) > 0 || st.TicketsRotated.IsZero() {
		t.Errorf("state after the renewal: %+v", st)
	}

	// lb-1 can't renew it: lb-2 is out all the same, the renewal waits for
	// the next check and the page says so.
	f.fail["lb-1 RotateTickets"] = errors.New("unavailable")
	err := a.clusters.RemoveMember(ctx, c.ID, "lb-2", false, "admin")
	if err == nil || !strings.Contains(err.Error(), "the next check tries again") {
		t.Fatalf("got %v", err)
	}
	c, _ = a.clusters.Store.Get(c.ID)
	if c.Member("lb-2") != nil || !slices.Equal(a.clusters.Store.State(c.ID).TicketsRenew, []string{"lb-2"}) {
		t.Fatalf("after a failed renewal: members %v, state %+v", c.Members, a.clusters.Store.State(c.ID))
	}
	if v := a.clusterView(c); !slices.ContainsFunc(v.Problems, func(p string) bool { return strings.Contains(p, "still the one lb-2 knew") }) {
		t.Errorf("problems: %v", v.Problems)
	}
	delete(f.fail, "lb-1 RotateTickets")
	mark = f.mark()
	a.clusters.reconcile(ctx, c)
	if got := f.callsOf("lb-1", mark); !slices.Contains(got, "RotateTickets renew") {
		t.Errorf("the check didn't renew the ring: %v", got)
	}
	if st := a.clusters.Store.State(c.ID); len(st.TicketsRenew) > 0 {
		t.Errorf("still to renew: %+v", st)
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

// A member replaced (docs/cluster.md "Recycling members"): the newcomer
// joins drained and learns before it comes into service, the old member
// is drained then, and leaves last - the traffic moving once. A failure
// takes the newcomer out again; one draining the old member puts it back
// in service first.
func TestClusterReplace(t *testing.T) {
	a, f, nodes := clusterApp(t, "lb-1", "lb-2", "lb-3", "lb-4")
	c := newCluster(t, a, nodes, "lb-1", "lb-2", "lb-3")
	ctx := context.Background()
	if err := a.clusters.Create(ctx, c.ID, "lb-1", "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.clusters.Store.Update(c.ID, func(c *clusters.Cluster) error {
		c.Member("lb-1").Vars = map[string]string{"priority": "150"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	lb4 := clusters.Member{NodeID: nodes["lb-4"].ID, Name: "lb-4", Address: "10.0.0.4"}

	// lb-4 fails on its way in: out again, lb-1 never drained.
	f.fail["lb-4 HAProxyApply"] = errors.New("haproxy -c: no")
	mark := f.mark()
	err := a.clusters.Replace(ctx, c.ID, "lb-1", lb4, clusters.ReplaceOptions{}, "admin")
	if err == nil || !strings.Contains(err.Error(), "lb-4 taken out again") {
		t.Errorf("lb-4 failing: %v", err)
	}
	if got := f.callsOf("lb-4", mark); !slices.Contains(got, "ClusterLeave") {
		t.Errorf("lb-4 not taken out: %v", got)
	}
	if slices.Contains(f.callsOf("lb-1", mark), "ClusterDrain") {
		t.Error("lb-1 drained although lb-4 never came in")
	}
	if cur, _ := a.clusters.Store.Get(c.ID); len(cur.Members) != 3 || cur.Member("lb-4") != nil {
		t.Errorf("the cluster after: %+v", cur.Members)
	}
	delete(f.fail, "lb-4 HAProxyApply")

	// lb-1 doesn't drain: back in service, lb-4 out.
	f.fail["lb-1 ClusterDrain"] = errors.New("still MASTER of front")
	mark = f.mark()
	err = a.clusters.Replace(ctx, c.ID, "lb-1", lb4, clusters.ReplaceOptions{}, "admin")
	if err == nil || !strings.Contains(err.Error(), "lb-1 didn't drain") {
		t.Errorf("lb-1 not draining: %v", err)
	}
	if !slices.Contains(f.callsOf("lb-1", mark), "ClusterUndrain") || !slices.Contains(f.callsOf("lb-4", mark), "ClusterLeave") {
		t.Errorf("lb-1 %v, lb-4 %v", f.callsOf("lb-1", mark), f.callsOf("lb-4", mark))
	}
	delete(f.fail, "lb-1 ClusterDrain")

	// For real: the order, the variables, lb-1 gone.
	mark = f.mark()
	if err := a.clusters.Replace(ctx, c.ID, "lb-1", lb4, clusters.ReplaceOptions{}, "admin"); err != nil {
		t.Fatal(err)
	}
	if !inOrder(f.callsSince(mark), "lb-4 ClusterSet lb-1,lb-2,lb-3,lb-4 +cert", "lb-4 ClusterDrain", "lb-4 HAProxyApply", "lb-4 ClusterStatus",
		"lb-4 ClusterUndrain", "lb-1 ClusterDrain", "lb-1 ClusterLeave", "lb-2 ClusterSet lb-2,lb-3,lb-4") {
		t.Errorf("the replacement's order: %v", f.callsSince(mark))
	}
	cur, _ := a.clusters.Store.Get(c.ID)
	if cur.Member("lb-1") != nil || cur.Member("lb-4") == nil || cur.Member("lb-4").Vars["priority"] != "150" {
		t.Errorf("after: %+v", cur.Members)
	}
	// lb-1 drained once, journaled once (campaigns 40, 41: twice - its
	// removal drained it again).
	if n := strings.Count(strings.Join(f.callsOf("lb-1", mark), " "), "ClusterDrain"); n != 1 {
		t.Errorf("lb-1 drained %d times: %v", n, f.callsOf("lb-1", mark))
	}
	drains := 0
	for _, e := range a.clusters.Store.Entries(c.ID, 100) {
		if e.Action == "drain" && e.Member == "lb-1" {
			drains++
		}
	}
	if drains != 1 {
		t.Errorf("lb-1's drain journaled %d times", drains)
	}
	if cur.SecretsFrom == "lb-1" {
		t.Error("the secrets still come from lb-1")
	}
}

// A member removed leaves drained - it may still run the cluster's VRRP
// or BGP configuration - and the journal says so; added again, it comes
// back in service (v2026.10.24-2's lab: a node removed sat drained, with
// nothing on it saying so, and would have joined again drained).
func TestClusterRejoinsInService(t *testing.T) {
	a, f, nodes := clusterApp(t, "lb-1", "lb-2", "lb-3")
	c := newCluster(t, a, nodes, "lb-1", "lb-2", "lb-3")
	ctx := context.Background()
	if err := a.clusters.Create(ctx, c.ID, "lb-1", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := a.clusters.RemoveMember(ctx, c.ID, "lb-3", false, "admin"); err != nil {
		t.Fatal(err)
	}
	said := func(words string) bool {
		return slices.ContainsFunc(a.clusters.Store.Entries(c.ID, 100), func(e clusters.Entry) bool { return strings.Contains(e.Message, words) })
	}
	if !f.nodes["lb-3"].drained || !said("lb-3 left the cluster, drained") {
		t.Errorf("lb-3 after its removal: drained %v, journal %+v", f.nodes["lb-3"].drained, a.clusters.Store.Entries(c.ID, 5))
	}
	mark := f.mark()
	if err := a.clusters.AddMember(ctx, c.ID, clusters.Member{NodeID: nodes["lb-3"].ID, Name: "lb-3", Address: "10.0.0.3"}, "admin"); err != nil {
		t.Fatal(err)
	}
	if got := f.callsOf("lb-3", mark); !inOrder(got, "HAProxyApply", "ClusterUndrain") || f.nodes["lb-3"].drained || !said("lb-3 was drained") {
		t.Errorf("lb-3 joined again: %v, drained %v", got, f.nodes["lb-3"].drained)
	}
	// A node that wasn't drained isn't undrained.
	if slices.Contains(f.callsOf("lb-1", 0), "ClusterUndrain") {
		t.Errorf("lb-1 undrained: %v", f.callsOf("lb-1", 0))
	}
}

// A member the cluster drained that comes back in service after a
// reboot - its release kept no drain across one - is drained again by
// the next check, and the journal says why; one undrained on the node
// itself is in service, the record following it (#356).
func TestClusterKeepsTheDrain(t *testing.T) {
	a, f, nodes := clusterApp(t, "lb-1", "lb-2")
	c := newCluster(t, a, nodes, "lb-1", "lb-2")
	ctx := context.Background()
	if err := a.clusters.Create(ctx, c.ID, "lb-1", "admin"); err != nil {
		t.Fatal(err)
	}
	c, _ = a.clusters.Store.Get(c.ID)
	if err := a.clusters.Drain(ctx, c.ID, "lb-2", "admin"); err != nil {
		t.Fatal(err)
	}
	said := func(words string) bool {
		return slices.ContainsFunc(a.clusters.Store.Entries(c.ID, 100), func(e clusters.Entry) bool { return strings.Contains(e.Message, words) })
	}
	// Rebooting: it doesn't answer - still drained as the cluster knows it.
	f.fail["lb-2 ClusterGet"] = errors.New("connection refused")
	if again := a.clusters.reconcile(ctx, c); !again {
		t.Error("a member that doesn't answer isn't checked again soon")
	}
	if st := a.clusters.Store.State(c.ID).Members["lb-2"]; !st.Drained {
		t.Fatalf("lb-2 while it reboots: %+v", st)
	}
	// Back, in service: its release kept no drain.
	delete(f.fail, "lb-2 ClusterGet")
	f.nodes["lb-2"].drained = false
	mark := f.mark()
	if again := a.clusters.reconcile(ctx, c); again {
		t.Errorf("an error: %+v", a.clusters.Store.State(c.ID).Members["lb-2"])
	}
	if !slices.Contains(f.callsOf("lb-2", mark), "ClusterDrain") || !f.nodes["lb-2"].drained {
		t.Fatalf("lb-2 not drained again: %v", f.callsOf("lb-2", mark))
	}
	if st := a.clusters.Store.State(c.ID).Members["lb-2"]; !st.Drained || st.Error != "" {
		t.Errorf("lb-2 drained again: %+v", st)
	}
	if !said("lb-2 came back undrained after a reboot") {
		t.Errorf("the journal: %+v", a.clusters.Store.Entries(c.ID, 5))
	}
	// Drained it stays: nothing more.
	mark = f.mark()
	a.clusters.reconcile(ctx, c)
	if slices.Contains(f.callsOf("lb-2", mark), "ClusterDrain") {
		t.Error("drained twice")
	}
	// The drain again failing: a problem, tried at the next check.
	f.nodes["lb-2"].drained = false
	f.fail["lb-2 ClusterDrain"] = errors.New("deadline exceeded")
	if again := a.clusters.reconcile(ctx, c); !again {
		t.Error("a failed drain isn't tried again soon")
	}
	if st := a.clusters.Store.State(c.ID).Members["lb-2"]; !st.Drained || !strings.Contains(st.Error, "draining it again failed") {
		t.Errorf("lb-2 after a failed drain: %+v", st)
	}
	view := a.clusterView(c)
	if !slices.ContainsFunc(view.Problems, func(p string) bool { return strings.Contains(p, "draining it again failed") }) {
		t.Errorf("problems: %v", view.Problems)
	}
	delete(f.fail, "lb-2 ClusterDrain")
	a.clusters.reconcile(ctx, c)
	if !f.nodes["lb-2"].drained {
		t.Fatal("not drained at the next check")
	}
	// Undrained on the node itself (janusctl cluster undrain): in
	// service, the cluster follows - never drained again.
	if err := f.ClusterUndrain(ctx, *c.Member("lb-2")); err != nil {
		t.Fatal(err)
	}
	mark = f.mark()
	a.clusters.reconcile(ctx, c)
	if slices.Contains(f.callsOf("lb-2", mark), "ClusterDrain") || f.nodes["lb-2"].drained {
		t.Fatal("an undrain on the node was fought")
	}
	if st := a.clusters.Store.State(c.ID).Members["lb-2"]; st.Drained {
		t.Errorf("the record didn't follow the node: %+v", st)
	}
	if !said("lb-2 was undrained on the node itself") {
		t.Errorf("the journal: %+v", a.clusters.Store.Entries(c.ID, 5))
	}
}

// Let's Encrypt in a cluster: the secrets' source orders, the others
// are told so with its thumbprint, and pull each renewal; a new source
// takes over.
func TestClusterLetsEncrypt(t *testing.T) {
	a, f, nodes := clusterApp(t, "lb-1", "lb-2", "lb-3")
	cert := func(name, serial string) *janusv1alpha1.ACMECertificateStatus {
		return &janusv1alpha1.ACMECertificateStatus{Name: name, State: "valid", Serial: serial}
	}
	running := janusv1alpha1.ModuleState_MODULE_STATE_RUNNING
	f.mu.Lock()
	f.nodes["lb-1"].acme = &janusv1alpha1.ACMEStatusResponse{State: running, Configured: true, AccountThumbprint: "thumb-1", Certificates: []*janusv1alpha1.ACMECertificateStatus{cert("site", "01"), cert("wild", "02")}}
	f.nodes["lb-2"].acme = &janusv1alpha1.ACMEStatusResponse{State: running, AccountThumbprint: "thumb-2"}
	f.mu.Unlock() // lb-3: no letsencrypt extension
	c := newCluster(t, a, nodes, "lb-1", "lb-2", "lb-3")
	ctx := context.Background()
	if err := a.clusters.Create(ctx, c.ID, "lb-1", "admin"); err != nil {
		t.Fatal(err)
	}
	// Made: everyone told lb-1 orders, with its thumbprint.
	for _, n := range []string{"lb-2", "lb-3"} {
		if s := f.node(clusters.Member{Name: n}).set; s.GetAcmeOrderer() != "lb-1" || s.GetAcmeThumbprint() != "thumb-1" {
			t.Errorf("%s told: %q %q", n, s.GetAcmeOrderer(), s.GetAcmeThumbprint())
		}
	}
	if !journalSays(t, a, c.ID, "ordered by lb-1: the others order nothing") {
		t.Errorf("the journal: %+v", a.clusters.Store.Entries(c.ID, 20))
	}
	// In step: a check pulls nothing.
	mark := f.mark()
	c, _ = a.clusters.Store.Get(c.ID)
	if err := a.clusters.SyncACME(ctx, c, "cluster"); err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(f.callsSince(mark), func(s string) bool { return strings.Contains(s, "ClusterPull") }) {
		t.Errorf("pulled with nothing renewed: %v", f.callsSince(mark))
	}
	// lb-1 renews "site": lb-2 pulls; lb-3, without the extension, doesn't.
	f.mu.Lock()
	f.nodes["lb-1"].acme.Certificates[0].Serial = "03"
	f.mu.Unlock()
	mark = f.mark()
	if err := a.clusters.SyncACME(ctx, c, "cluster"); err != nil {
		t.Fatal(err)
	}
	if got := f.callsSince(mark); !slices.Contains(got, "lb-2 ClusterPull lb-1 acme_config acme_certs") || slices.ContainsFunc(got, func(s string) bool { return strings.HasPrefix(s, "lb-3 ClusterPull") }) {
		t.Errorf("after a renewal: %v", got)
	}
	if !journalSays(t, a, c.ID, "lb-2 pulled lb-1's renewed certificate(s): site") {
		t.Errorf("the journal: %+v", a.clusters.Store.Entries(c.ID, 20))
	}
	// lb-2 becomes the source: told it orders, it does; its thumbprint
	// then goes to the others.
	if _, err := a.clusters.Store.Update(c.ID, func(c *clusters.Cluster) error { c.SecretsFrom = "lb-2"; return nil }); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.nodes["lb-2"].acme.Configured = true
	f.mu.Unlock()
	c, _ = a.clusters.Store.Get(c.ID)
	// One check: lb-2, still following lb-1, told first, alone; its
	// thumbprint then goes to the others, each told once - never "lb-2
	// orders" without it (they'd answer HTTP-01 with their own account
	// until the next check: v2026.10.25-3's lab).
	mark = f.mark()
	if err := a.clusters.SyncACME(ctx, c, "cluster"); err != nil {
		t.Fatal(err)
	}
	if s := f.node(clusters.Member{Name: "lb-2"}).set; s.GetAcmeOrderer() != "lb-2" {
		t.Errorf("lb-2 not told it orders: %q", s.GetAcmeOrderer())
	}
	for _, n := range []string{"lb-1", "lb-3"} {
		if s := f.node(clusters.Member{Name: n}).set; s.GetAcmeOrderer() != "lb-2" || s.GetAcmeThumbprint() != "thumb-2" {
			t.Errorf("%s after lb-2 took over: %q %q", n, s.GetAcmeOrderer(), s.GetAcmeThumbprint())
		}
	}
	got := f.callsSince(mark)
	if first := slices.IndexFunc(got, func(s string) bool { return strings.Contains(s, "ClusterSet") }); first < 0 || !strings.HasPrefix(got[first], "lb-2 ClusterSet") {
		t.Errorf("lb-2 not told first: %v", got)
	}
	for _, n := range []string{"lb-1", "lb-3"} {
		if k := len(slices.DeleteFunc(slices.Clone(got), func(s string) bool { return !strings.HasPrefix(s, n+" ClusterSet") })); k != 1 {
			t.Errorf("%s told %d times in the check, want once: %v", n, k, got)
		}
	}
	if !journalSays(t, a, c.ID, "ordered by lb-2: the others order nothing") {
		t.Errorf("the journal: %+v", a.clusters.Store.Entries(c.ID, 20))
	}
	// Each one told reloads HAProxy when its HTTP-01 answer changes: its
	// stick tables learnt back before the next one is told - members
	// reloading together lose them.
	var order []string
	for _, call := range got {
		if strings.Contains(call, " ClusterSet ") || strings.HasSuffix(call, " ClusterStatus") {
			order = append(order, strings.SplitN(call, " ", 3)[0]+" "+strings.SplitN(call, " ", 3)[1])
		}
	}
	if !inOrder(order, "lb-2 ClusterSet", "lb-2 ClusterStatus", "lb-1 ClusterSet", "lb-1 ClusterStatus", "lb-2 ClusterSet", "lb-2 ClusterStatus", "lb-3 ClusterSet", "lb-3 ClusterStatus") {
		t.Errorf("each member's tables before the next one is told: %v", order)
	}
	// One that doesn't learn them, a peer up: the others aren't told.
	a.clusters.LearnTimeout = 1500 * time.Millisecond
	if _, err := a.clusters.Store.Update(c.ID, func(c *clusters.Cluster) error { c.SecretsFrom = "lb-1"; return nil }); err != nil {
		t.Fatal(err)
	}
	c, _ = a.clusters.Store.Get(c.ID)
	f.mu.Lock()
	f.nodes["lb-1"].learning = true
	f.mu.Unlock()
	mark = f.mark()
	if err := a.clusters.SyncACME(ctx, c, "cluster"); err == nil || !strings.Contains(err.Error(), "not learnt") {
		t.Errorf("lb-1 not learning its tables: %v", err)
	}
	if calls := f.callsSince(mark); slices.ContainsFunc(calls, func(s string) bool {
		return strings.HasPrefix(s, "lb-2 ClusterSet") || strings.HasPrefix(s, "lb-3 ClusterSet")
	}) {
		t.Errorf("told the others while lb-1 hadn't its tables: %v", calls)
	}
}

func (f *clusterFake) Addresses(_ context.Context, m clusters.Member) ([]netip.Addr, error) {
	if err := f.call(m, "Addresses", ""); err != nil {
		return nil, err
	}
	out := slices.Clone(f.node(m).addrs)
	if a, err := netip.ParseAddr(m.Address); err == nil {
		out = append(out, a)
	}
	return out, nil
}
