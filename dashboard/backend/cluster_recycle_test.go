package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/machines"
	"github.com/swenske/Janus/internal/clusters"
	"github.com/swenske/Janus/internal/nocloud"
)

// A member that is one of the Controller's machines, recycled: a new
// machine like it (its own name, its own MAC), registered and trusting
// the fleet, takes its place - and the old machine is destroyed.
func TestClusterRecycleAMachine(t *testing.T) {
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
	f := &clusterFake{nodes: map[string]*fakeMember{}, fail: map[string]error{}, unreach: map[[2]string]bool{}, byAddr: map[string]string{}, notAfter: time.Now().Add(clusters.MemberValidity)}
	a.clusters.Nodes = f
	t.Cleanup(a.clusters.wait)
	member := func(name string) {
		f.nodes[name] = &fakeMember{schematic: "s1", healthy: true, cfg: []byte("global # " + name + "\n"), files: map[string][]byte{}, secret: map[string]bool{}, modules: map[string][]byte{}}
	}
	// register answers machine id's registration with its token, as its
	// node would from addr, and gives it the fleet.
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
		// Before the registration: a recycle waiting for this node
		// probes it once it trusts the fleet.
		f.byAddr[addr] = name
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
	machine := func(name, addr string) *machines.Machine {
		t.Helper()
		rec := call(t, a.handleMachineCreate, "POST", "/api/machines", "POST /api/machines", machines.Spec{
			Name: name, HypervisorID: h.ID,
			Image: &machines.ImageSource{URL: srv.URL + "/janus-kvm.qcow2", SHA256: hex.EncodeToString(sum[:])},
			NICs:  []machines.NIC{{Network: "lab-mgmt", Name: "mgmt", Mode: "dhcp"}},
		})
		var v machineView
		_ = json.Unmarshal(rec.Body.Bytes(), &v)
		member(name)
		return register(v.ID, name, addr)
	}
	lb1, lb2 := machine("lb-1", "10.0.0.11:9505"), machine("lb-2", "10.0.0.12:9505")
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

	// A member that isn't a machine needs its replacement named.
	rec := call(t, a.handleClusterRecycle, "POST", "/api/clusters/"+c.ID+"/recycle", "POST /api/clusters/{id}/recycle", recycleRequest{Members: []string{"nobody"}})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("an unknown member: %d %s", rec.Code, rec.Body)
	}

	member("edge-1")
	done := make(chan error, 1)
	go func() { done <- a.recycle(ctx, c.ID, recycleRequest{Members: []string{"lb-1"}}, "admin") }()
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
	register(repl.ID, "edge-1", "10.0.0.13:9505")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	cur, _ := a.clusters.Store.Get(c.ID)
	if cur.Member("lb-1") != nil || cur.Member("edge-1") == nil || cur.Member("edge-1").Address != "10.0.0.13" {
		t.Errorf("members after: %+v", cur.Members)
	}
	repl, _ = a.machines.Get(repl.ID)
	if repl.Spec.NICs[0].MAC == "" || repl.Spec.NICs[0].MAC == lb1.Spec.NICs[0].MAC || repl.Spec.HypervisorID != h.ID {
		t.Errorf("the new machine's spec: %+v (lb-1's %+v)", repl.Spec, lb1.Spec)
	}
	deadline = time.Now().Add(30 * time.Second)
	for {
		if _, ok := a.machines.Get(lb1.ID); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("lb-1's machine never destroyed")
		}
		time.Sleep(20 * time.Millisecond)
	}
	fake.mu.Lock()
	destroyed := slices.Clone(fake.destroyed)
	fake.mu.Unlock()
	if !slices.Contains(destroyed, "janus-lb-1") {
		t.Errorf("destroyed %v", destroyed)
	}
}

// A static address can't be cloned: two machines would hold it.
func TestCloneSpec(t *testing.T) {
	old := machines.Spec{Name: "lb-1", Version: "v2026.10.23", NICs: []machines.NIC{{Name: "mgmt", Mode: "dhcp", MAC: "52:54:00:00:00:01"}}, DNS: []string{"192.0.2.53"}}
	s, err := cloneSpec(old, "edge-1", "v2026.10.24")
	if err != nil || s.Name != "edge-1" || s.Version != "v2026.10.24" || s.NICs[0].MAC != "" || old.NICs[0].MAC == "" {
		t.Errorf("clone: %+v %v (old %+v)", s, err, old)
	}
	if s, _ := cloneSpec(old, "edge-1", ""); s.Version != "v2026.10.23" {
		t.Errorf("the version kept: %q", s.Version)
	}
	old.NICs = append(old.NICs, machines.NIC{Name: "lan", Mode: "static", Addresses: []string{"192.0.2.11/24"}})
	if _, err := cloneSpec(old, "edge-1", ""); err == nil || !strings.Contains(err.Error(), "static address") {
		t.Errorf("a static address cloned: %v", err)
	}
}
