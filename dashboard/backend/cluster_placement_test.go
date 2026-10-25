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
	got, _, err := place([]hostSlot{a1, a2, b, c}, nil, 3, clusters.AntiAffinityRequired)
	if err != nil || !slices.Equal(hvs(got), []string{"h1", "h3", "h4"}) {
		t.Errorf("required, 3 hosts: %v %v", hvs(got), err)
	}
	_, _, err = place([]hostSlot{a1, a2, b}, nil, 3, clusters.AntiAffinityRequired)
	if err == nil || !strings.Contains(err.Error(), "kvm01 (kvm01 and kvm01-again: one host)") || !strings.Contains(err.Error(), "2 host(s)") {
		t.Errorf("required, 2 hosts for 3: %v", err)
	}
	// A host a member runs on isn't free.
	got, _, err = place([]hostSlot{a1, b}, map[string][]string{"A": {"lb-1"}}, 1, clusters.AntiAffinityRequired)
	if err != nil || !slices.Equal(hvs(got), []string{"h3"}) {
		t.Errorf("required, A taken: %v %v", hvs(got), err)
	}
	_, _, err = place([]hostSlot{a1}, map[string][]string{"A": {"lb-1"}}, 1, clusters.AntiAffinityRequired)
	if err == nil || !strings.Contains(err.Error(), "where lb-1 run(s)") {
		t.Errorf("required, the only host taken: %v", err)
	}
	// Preferred: hosts of their own first, then the least busy, said.
	got, warnings, err := place([]hostSlot{a1, a2, b}, nil, 3, clusters.AntiAffinityPreferred)
	if err != nil || !slices.Equal(hvs(got), []string{"h1", "h3", "h1"}) || len(warnings) != 1 {
		t.Errorf("preferred, 2 hosts for 3: %v %v %v", hvs(got), warnings, err)
	}
	// Off: the records in turn.
	got, warnings, _ = place([]hostSlot{a1, a2}, nil, 3, clusters.AntiAffinityOff)
	if !slices.Equal(hvs(got), []string{"h1", "h2", "h1"}) || warnings != nil {
		t.Errorf("off: %v %v", hvs(got), warnings)
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

// A cluster made of new machines: placed by host - two records reaching
// one host count once -, made, waited for, created from them.
func TestClusterOnHypervisors(t *testing.T) {
	a, _ := newTestApp(t)
	h1, h2 := addHypervisor(t, a, "kvm01"), addHypervisor(t, a, "kvm01-again")
	fakes := map[string]*fakeDriver{h1.ID: newFakeDriver(), h2.ID: newFakeDriver()}
	fakes[h1.ID].hostID, fakes[h2.ID].hostID = "libvirt:aaaa", "libvirt:aaaa"
	newDriver = func(h *hypervisor.Hypervisor, _ string) (hypervisor.Driver, error) { return fakes[h.ID], nil }
	image := append([]byte("QFI\xfb\x00\x00\x00\x03"), make([]byte, 4096)...)
	sum := sha256.Sum256(image)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(image) }))
	defer srv.Close()
	st, err := clusters.Open(a.dataDir, testSealer{})
	if err != nil {
		t.Fatal(err)
	}
	a.clusters = newClusterEngine(a, st)
	f := &clusterFake{nodes: map[string]*fakeMember{}, fail: map[string]error{}, unreach: map[[2]string]bool{}, byAddr: map[string]string{}, notAfter: time.Now().Add(clusters.MemberValidity)}
	a.clusters.Nodes = f
	t.Cleanup(a.clusters.wait)
	request := func(name, mode string) clusterMachinesRequest {
		return clusterMachinesRequest{Count: 2, AntiAffinity: mode, Hypervisors: []string{h1.ID, h2.ID}, Spec: machines.Spec{
			Image: &machines.ImageSource{URL: srv.URL + "/janus-kvm.qcow2", SHA256: hex.EncodeToString(sum[:])},
			NICs:  []machines.NIC{{Network: "lab-mgmt", Name: "mgmt", Mode: "dhcp"}},
		}}
	}
	create := func(name string, m clusterMachinesRequest) *httptest.ResponseRecorder {
		return call(t, a.handleClusterCreate, "POST", "/api/clusters", "POST /api/clusters", map[string]any{"name": name, "machines": m})
	}

	// Required, two records on one host: refused before anything is made.
	rec := create("edge", request("edge", ""))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "kvm01 and kvm01-again: one host") {
		t.Fatalf("required on one host: %d %s", rec.Code, rec.Body)
	}
	if n := len(a.machines.List()); n != 0 {
		t.Fatalf("%d machines made for a refused cluster", n)
	}
	// A static address can't go to every machine.
	static := request("edge", clusters.AntiAffinityPreferred)
	static.Spec.NICs[0] = machines.NIC{Network: "lab-mgmt", Name: "mgmt", Mode: "static", Addresses: []string{"192.0.2.10/24"}}
	if rec := create("edge", static); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "static address") {
		t.Errorf("static: %d %s", rec.Code, rec.Body)
	}

	// register answers machine m's registration as its node would, from
	// an address of its own, and gives it the fleet.
	addrs := 0
	register := func(m *machines.Machine) {
		t.Helper()
		addrs++
		addr := fmt.Sprintf("10.0.0.%d:9505", addrs)
		m = waitPhase(t, a, m.ID, machines.PhaseRegistering)
		fk := fakes[m.Spec.HypervisorID]
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
		f.mu.Lock()
		f.byAddr[addr] = m.Spec.Name
		f.nodes[m.Spec.Name] = &fakeMember{schematic: "s1", healthy: true, cfg: []byte("global # " + m.Spec.Name + "\n"), files: map[string][]byte{}, secret: map[string]bool{}, modules: map[string][]byte{}}
		f.mu.Unlock()
		req := registration(t, m.Spec.Name, data.RegistrationToken)
		req.Address = addr
		if rec := call(t, a.handleRegister, "POST", "/register", "/register", req); rec.Code != http.StatusCreated {
			t.Fatalf("register %s: %d %s", m.Spec.Name, rec.Code, rec.Body)
		}
		m = waitPhase(t, a, m.ID, machines.PhaseReady)
		if err := a.store.SetFleet(m.NodeID); err != nil {
			t.Fatal(err)
		}
	}
	// build creates cluster name and plays its machines' nodes.
	build := func(name string, m clusterMachinesRequest) *clusters.Cluster {
		t.Helper()
		rec := create(name, m)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("create %s: %d %s", name, rec.Code, rec.Body)
		}
		var v clusterView
		_ = json.Unmarshal(rec.Body.Bytes(), &v)
		for i := range m.Count {
			mn := fmt.Sprintf("%s-%d", name, i+1)
			var made *machines.Machine
			for deadline := time.Now().Add(10 * time.Second); made == nil && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
				for _, x := range a.machines.List() {
					if x.Spec.Name == mn {
						made = x
					}
				}
			}
			if made == nil {
				t.Fatalf("no machine %s", mn)
			}
			register(made)
		}
		deadline := time.Now().Add(20 * time.Second)
		for {
			c, _ := a.clusters.Store.Get(v.ID)
			if !a.clusters.isBusy(v.ID) && c.Phase == "" {
				if c.Error != "" {
					t.Fatalf("cluster %s: %s", name, c.Error)
				}
				return c
			}
			if time.Now().After(deadline) {
				t.Fatalf("cluster %s still %s", name, c.Phase)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	// Preferred: made all the same, both on the one host, and said.
	c := build("edgep", request("edgep", clusters.AntiAffinityPreferred))
	if len(c.Members) != 2 || c.SecretsFrom != "edgep-1" || c.Placement == nil || c.Placement.AntiAffinity != clusters.AntiAffinityPreferred {
		t.Fatalf("preferred cluster: %+v", c)
	}
	v := a.clusterView(c)
	if v.Members[0].HostID != "libvirt:aaaa" || !slices.ContainsFunc(v.Problems, func(p string) bool { return strings.Contains(p, "edgep-1 and edgep-2 run on the same host") }) {
		t.Errorf("preferred view: hosts %q %q, problems %q", v.Members[0].HostID, v.Members[1].HostID, v.Problems)
	}

	// Two hosts now: required places one machine on each.
	fakes[h2.ID].hostID = "libvirt:bbbb"
	a.hvStatus.forget(h2.ID)
	c = build("edger", request("edger", ""))
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
	var repl *machines.Machine
	for deadline := time.Now().Add(10 * time.Second); repl == nil && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		for _, m := range a.machines.List() {
			if m.Spec.Name == "edger-3" {
				repl = m
			}
		}
	}
	if repl == nil {
		t.Fatal("no replacement made")
	}
	register(repl)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if repl, _ = a.machines.Get(repl.ID); repl.Spec.HypervisorID != h1.ID {
		t.Errorf("the replacement went to %s, not the replaced member's host (the other is taken)", repl.Spec.HypervisorID)
	}
}
