package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/swenske/Janus/dashboard/backend/internal/hypervisor"
	"github.com/swenske/Janus/dashboard/backend/internal/machines"
	"github.com/swenske/Janus/internal/clusters"
	"github.com/swenske/Janus/internal/nocloud"
)

func TestPlace(t *testing.T) {
	a1 := hostSlot{Hypervisor: "h1", Name: "kvm01", HostID: "A", HostName: "kvm01"}
	a2 := hostSlot{Hypervisor: "h2", Name: "kvm01-again", HostID: "A", HostName: "kvm01"}
	b := hostSlot{Hypervisor: "h3", Name: "kvm02", HostID: "B", HostName: "kvm02"}
	c := hostSlot{Hypervisor: "h4", Name: "pve-c", HostID: "C", HostName: "pve-c"}
	hvs := func(s []hostSlot) []string {
		var out []string
		for _, x := range s {
			out = append(out, x.Hypervisor)
		}
		return out
	}
	// Required: one host each, two records on one host counted once.
	got, _, err := place([]hostSlot{a1, a2, b, c}, nil, 3, 1024, clusters.AntiAffinityRequired)
	if err != nil || !slices.Equal(hvs(got), []string{"h1", "h3", "h4"}) {
		t.Errorf("required, 3 hosts: %v %v", hvs(got), err)
	}
	_, _, err = place([]hostSlot{a1, a2, b}, nil, 3, 1024, clusters.AntiAffinityRequired)
	if err == nil || !strings.Contains(err.Error(), "kvm01 (kvm01 and kvm01-again: one host)") || !strings.Contains(err.Error(), "2 host(s)") {
		t.Errorf("required, 2 hosts for 3: %v", err)
	}
	// A host a member runs on isn't free.
	got, _, err = place([]hostSlot{a1, b}, map[string][]string{"A": {"lb-1"}}, 1, 1024, clusters.AntiAffinityRequired)
	if err != nil || !slices.Equal(hvs(got), []string{"h3"}) {
		t.Errorf("required, A taken: %v %v", hvs(got), err)
	}
	_, _, err = place([]hostSlot{a1}, map[string][]string{"A": {"lb-1"}}, 1, 1024, clusters.AntiAffinityRequired)
	if err == nil || !strings.Contains(err.Error(), "where lb-1 run(s)") {
		t.Errorf("required, the only host taken: %v", err)
	}
	// Preferred: hosts of their own first, then the least busy, said.
	got, warnings, err := place([]hostSlot{a1, a2, b}, nil, 3, 1024, clusters.AntiAffinityPreferred)
	if err != nil || !slices.Equal(hvs(got), []string{"h1", "h3", "h1"}) || len(warnings) != 1 {
		t.Errorf("preferred, 2 hosts for 3: %v %v %v", hvs(got), warnings, err)
	}
	// Off: by memory alone - one host here, its first record.
	got, warnings, _ = place([]hostSlot{a1, a2}, nil, 3, 1024, clusters.AntiAffinityOff)
	if !slices.Equal(hvs(got), []string{"h1", "h1", "h1"}) || warnings != nil {
		t.Errorf("off: %v %v", hvs(got), warnings)
	}
}

// Among the hosts a rule allows, the one with the most memory to give
// goes first - what a host keeps free (1/20) aside -, and one without
// room for the machine is never picked.
func TestPlaceByMemory(t *testing.T) {
	k1 := hostSlot{Hypervisor: "h1", Name: "kvm01", HostID: "A", HostName: "kvm01", FreeMiB: 8192, TotalMiB: 32768}
	k2 := hostSlot{Hypervisor: "h2", Name: "kvm02", HostID: "B", HostName: "kvm02", FreeMiB: 30000, TotalMiB: 32768}
	hvs := func(s []hostSlot) []string {
		var out []string
		for _, x := range s {
			out = append(out, x.Hypervisor)
		}
		return out
	}
	// Off: the emptier host until it isn't.
	got, _, err := place([]hostSlot{k1, k2}, nil, 3, 2048, clusters.AntiAffinityOff)
	if err != nil || !slices.Equal(hvs(got), []string{"h2", "h2", "h2"}) {
		t.Errorf("off: %v %v", hvs(got), err)
	}
	small := k2
	small.FreeMiB = 9000
	got, _, _ = place([]hostSlot{k1, small}, nil, 3, 2048, clusters.AntiAffinityOff)
	if !slices.Equal(hvs(got), []string{"h2", "h1", "h2"}) {
		t.Errorf("off, close: %v", hvs(got))
	}
	// Preferred: hosts of their own first - the emptier first -, then the
	// least busy, the emptier of them.
	got, warnings, _ := place([]hostSlot{k1, k2}, nil, 3, 2048, clusters.AntiAffinityPreferred)
	if !slices.Equal(hvs(got), []string{"h2", "h1", "h2"}) || len(warnings) != 1 {
		t.Errorf("preferred: %v %v", hvs(got), warnings)
	}
	// Required: a host without room is no host of its own.
	full := k1
	full.FreeMiB = 2000 // 1638 kept: 362 to give
	got, _, err = place([]hostSlot{full, k2}, nil, 1, 1024, clusters.AntiAffinityRequired)
	if err != nil || !slices.Equal(hvs(got), []string{"h2"}) {
		t.Errorf("required, kvm01 full: %v %v", hvs(got), err)
	}
	_, _, err = place([]hostSlot{full, k2}, nil, 2, 1024, clusters.AntiAffinityRequired)
	if err == nil || !strings.Contains(err.Error(), "no host has room for a machine of 1024 MiB") || !strings.Contains(err.Error(), "kvm01 2000 MiB free, 1638 kept") {
		t.Errorf("required, two hosts, one full: %v", err)
	}
	_, _, err = place([]hostSlot{full}, nil, 1, 1024, clusters.AntiAffinityOff)
	if err == nil || !strings.Contains(err.Error(), "no host has room") {
		t.Errorf("off, full: %v", err)
	}
}

func TestPlacementProblems(t *testing.T) {
	c := &clusters.Cluster{Placement: &clusters.Placement{AntiAffinity: clusters.AntiAffinityRequired}}
	views := []clusterMemberView{
		{Member: clusters.Member{Name: "edge-1"}, HostID: "A", HostName: "kvm01"},
		{Member: clusters.Member{Name: "edge-2"}, HostID: "A", HostName: "kvm01"},
		{Member: clusters.Member{Name: "edge-3"}, HostID: "B", HostName: "kvm02"},
		{Member: clusters.Member{Name: "lb-9"}},
	}
	got := placementProblems(c, views)
	if len(got) != 2 || !strings.Contains(got[0], "edge-1 and edge-2 run on the same host, kvm01") || !strings.Contains(got[1], "lb-9: on a host the Controller can't tell") {
		t.Errorf("problems: %q", got)
	}
	c.Placement.AntiAffinity = clusters.AntiAffinityOff
	if got := placementProblems(c, views); got != nil {
		t.Errorf("off: %q", got)
	}
	if got := placementProblems(&clusters.Cluster{}, views); got != nil {
		t.Errorf("a cluster made from nodes: %q", got)
	}
}

// addHypervisor adds a trusted libvirt record named name.
func addHypervisor(t *testing.T, a *app, name string) *hypervisor.Hypervisor {
	t.Helper()
	h := &hypervisor.Hypervisor{Name: name, Kind: hypervisor.KindLibvirt, Libvirt: &hypervisor.LibvirtConfig{
		Host: "192.0.2.2", User: "janus-ctl", Pool: "janus", Networks: []string{"lab-mgmt"},
	}}
	if err := a.hypervisors.Add(h); err != nil {
		t.Fatal(err)
	}
	signer, err := h.Signer()
	if err != nil {
		t.Fatal(err)
	}
	h.Libvirt.HostKey = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	if err := a.hypervisors.Update(h); err != nil {
		t.Fatal(err)
	}
	return h
}

// hvHarness is an app with two fake hypervisor records, a cluster
// engine on fake members, and what plays the machines' nodes.
type hvHarness struct {
	t      *testing.T
	a      *app
	h1, h2 *hypervisor.Hypervisor
	fakes  map[string]*fakeDriver
	f      *clusterFake
	url    string
	sum    string
	addrs  int
}

func newHVHarness(t *testing.T) *hvHarness {
	t.Helper()
	a, _ := newTestApp(t)
	x := &hvHarness{t: t, a: a, h1: addHypervisor(t, a, "kvm01"), h2: addHypervisor(t, a, "kvm01-again")}
	x.fakes = map[string]*fakeDriver{x.h1.ID: newFakeDriver(), x.h2.ID: newFakeDriver()}
	x.fakes[x.h1.ID].hostID, x.fakes[x.h2.ID].hostID = "libvirt:aaaa", "libvirt:aaaa"
	newDriver = func(h *hypervisor.Hypervisor, _ string) (hypervisor.Driver, error) { return x.fakes[h.ID], nil }
	image := append([]byte("QFI\xfb\x00\x00\x00\x03"), make([]byte, 4096)...)
	sum := sha256.Sum256(image)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(image) }))
	t.Cleanup(srv.Close)
	x.url, x.sum = srv.URL+"/janus-kvm.qcow2", hex.EncodeToString(sum[:])
	st, err := clusters.Open(a.dataDir, testSealer{})
	if err != nil {
		t.Fatal(err)
	}
	a.clusters = newClusterEngine(a, st)
	x.f = &clusterFake{nodes: map[string]*fakeMember{}, fail: map[string]error{}, unreach: map[[2]string]bool{}, byAddr: map[string]string{}, notAfter: time.Now().Add(clusters.MemberValidity)}
	a.clusters.Nodes = x.f
	t.Cleanup(a.clusters.wait)
	return x
}

func (x *hvHarness) request(mode string) clusterMachinesRequest {
	return clusterMachinesRequest{Count: 2, AntiAffinity: mode, Hypervisors: []string{x.h1.ID, x.h2.ID}, Spec: machines.Spec{
		Image: &machines.ImageSource{URL: x.url, SHA256: x.sum},
		NICs:  []machines.NIC{{Network: "lab-mgmt", Name: "mgmt", Mode: "dhcp"}},
	}}
}

func (x *hvHarness) create(name string, m clusterMachinesRequest) *httptest.ResponseRecorder {
	return call(x.t, x.a.handleClusterCreate, "POST", "/api/clusters", "POST /api/clusters", map[string]any{"name": name, "machines": m})
}

// register answers machine m's registration as its node would, from an
// address of its own, and gives it the fleet.
func (x *hvHarness) register(m *machines.Machine) {
	t := x.t
	t.Helper()
	x.addrs++
	addr := fmt.Sprintf("10.0.0.%d:9505", x.addrs)
	m = waitPhase(t, x.a, m.ID, machines.PhaseRegistering)
	fk := x.fakes[m.Spec.HypervisorID]
	fk.mu.Lock()
	ci := fk.vms[m.Ref.UUID].CIData
	fk.mu.Unlock()
	path := filepath.Join(t.TempDir(), "cidata.iso")
	if err := os.WriteFile(path, ci, 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := nocloud.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	x.f.mu.Lock()
	x.f.byAddr[addr] = m.Spec.Name
	x.f.nodes[m.Spec.Name] = &fakeMember{schematic: "s1", healthy: true, cfg: []byte("global # " + m.Spec.Name + "\n"), files: map[string][]byte{}, secret: map[string]bool{}, modules: map[string][]byte{}}
	x.f.mu.Unlock()
	req := registration(t, m.Spec.Name, data.RegistrationToken)
	req.Address = addr
	if rec := call(t, x.a.handleRegister, "POST", "/register", "/register", req); rec.Code != http.StatusCreated {
		t.Fatalf("register %s: %d %s", m.Spec.Name, rec.Code, rec.Body)
	}
	m = waitPhase(t, x.a, m.ID, machines.PhaseReady)
	if err := x.a.store.SetFleet(m.NodeID); err != nil {
		t.Fatal(err)
	}
}

// machine waits for the machine named name.
func (x *hvHarness) machine(name string) *machines.Machine {
	x.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		for _, m := range x.a.machines.List() {
			if m.Spec.Name == name {
				return m
			}
		}
	}
	x.t.Fatalf("no machine %s", name)
	return nil
}

// idle waits until no operation runs on cluster id, and returns it.
func (x *hvHarness) idle(id string) *clusters.Cluster {
	x.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		c, _ := x.a.clusters.Store.Get(id)
		if !x.a.clusters.isBusy(id) && c.Phase == "" {
			return c
		}
		if time.Now().After(deadline) {
			x.t.Fatalf("cluster %s still %s", c.Name, c.Phase)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// build creates cluster name and plays its machines' nodes.
func (x *hvHarness) build(name string, m clusterMachinesRequest) *clusters.Cluster {
	x.t.Helper()
	rec := x.create(name, m)
	if rec.Code != http.StatusAccepted {
		x.t.Fatalf("create %s: %d %s", name, rec.Code, rec.Body)
	}
	var v clusterView
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	for i := range m.Count {
		x.register(x.machine(fmt.Sprintf("%s-%d", name, i+1)))
	}
	c := x.idle(v.ID)
	if c.Error != "" {
		x.t.Fatalf("cluster %s: %s", name, c.Error)
	}
	return c
}

// A cluster made of new machines: placed by host - two records reaching
// one host count once -, made, waited for, created from them.
func TestClusterOnHypervisors(t *testing.T) {
	x := newHVHarness(t)
	a, h1, h2 := x.a, x.h1, x.h2

	// Required, two records on one host: refused before anything is made.
	rec := x.create("edge", x.request(""))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "kvm01 and kvm01-again: one host") {
		t.Fatalf("required on one host: %d %s", rec.Code, rec.Body)
	}
	if n := len(a.machines.List()); n != 0 {
		t.Fatalf("%d machines made for a refused cluster", n)
	}
	// A static address can't go to every machine.
	static := x.request(clusters.AntiAffinityPreferred)
	static.Spec.NICs[0] = machines.NIC{Network: "lab-mgmt", Name: "mgmt", Mode: "static", Addresses: []string{"192.0.2.10/24"}}
	if rec := x.create("edge", static); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "static address") {
		t.Errorf("static: %d %s", rec.Code, rec.Body)
	}

	// Preferred: made all the same, both on the one host, and said.
	c := x.build("edgep", x.request(clusters.AntiAffinityPreferred))
	if len(c.Members) != 2 || c.SecretsFrom != "edgep-1" || c.Placement == nil || c.Placement.AntiAffinity != clusters.AntiAffinityPreferred {
		t.Fatalf("preferred cluster: %+v", c)
	}
	v := a.clusterView(c)
	if v.Members[0].HostID != "libvirt:aaaa" || !slices.ContainsFunc(v.Problems, func(p string) bool { return strings.Contains(p, "edgep-1 and edgep-2 run on the same host") }) {
		t.Errorf("preferred view: hosts %q %q, problems %q", v.Members[0].HostID, v.Members[1].HostID, v.Problems)
	}

	// Two hosts now: required places one machine on each.
	x.fakes[h2.ID].hostID = "libvirt:bbbb"
	a.hvStatus.forget(h2.ID)
	c = x.build("edger", x.request(""))
	var on []string
	for _, m := range c.Members {
		on = append(on, a.machineOf(m.NodeID).Spec.HypervisorID)
	}
	if !slices.Equal(on, []string{h1.ID, h2.ID}) {
		t.Errorf("required on two hosts: %v", on)
	}
	if v := a.clusterView(c); slices.ContainsFunc(v.Problems, func(p string) bool { return strings.Contains(p, "same host") }) {
		t.Errorf("problems: %q", v.Problems)
	}

	// Recycling one: the replacement goes where no other member runs -
	// the replaced one's host, here, since it leaves.
	ctx := context.Background()
	old := c.Members[0]
	done := make(chan error, 1)
	go func() { done <- a.recycle(ctx, c.ID, recycleRequest{Members: []string{old.Name}}, "admin") }()
	repl := x.machine("edger-3")
	x.register(repl)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !journalSays(t, a, c.ID, "a new machine, edger-3, on kvm01") {
		t.Errorf("the recycle's journal doesn't name the hypervisor: %v", a.clusters.Store.Entries(c.ID, 50))
	}
	if repl, _ = a.machines.Get(repl.ID); repl.Spec.HypervisorID != h1.ID {
		t.Errorf("the replacement went to %s, not the replaced member's host (the other is taken)", repl.Spec.HypervisorID)
	}
}
