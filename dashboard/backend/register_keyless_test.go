package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swenske/Janus/dashboard/backend/internal/fleet"
	"github.com/swenske/Janus/dashboard/backend/internal/machines"
	"github.com/swenske/Janus/dashboard/backend/internal/secrets"
	"github.com/swenske/Janus/internal/nocloud"
	"github.com/swenske/Janus/internal/pki"
)

// withFleet gives a its fleet, set up and confirmed.
func withFleet(t *testing.T, a *app) {
	t.Helper()
	key, err := secrets.LoadOrCreate("", a.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	f, err := fleet.Open(filepath.Join(a.dataDir, "fleet"), key, a.controllerID)
	if err != nil {
		t.Fatal(err)
	}
	pass, err := f.Setup()
	if err != nil {
		t.Fatal(err)
	}
	kit, err := f.RecoveryKit()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Confirm(kit, pass); err != nil {
		t.Fatal(err)
	}
	a.fleet = f
	a.trust = newTrustTracker()
}

func keylessRegistration(t *testing.T, name, token string) registerRequest {
	t.Helper()
	ca, err := pki.NewCA("node " + name)
	if err != nil {
		t.Fatal(err)
	}
	return registerRequest{Protocol: 2, Name: name, Address: "127.0.0.1:1", CACertPEM: string(ca.CertPEM), RegistrationToken: token, PollSecret: strings.Repeat("ab", 32)}
}

func poll(t *testing.T, a *app, id, secret string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /register/{id}", a.handleRegisterPoll)
	r := httptest.NewRequest("GET", "/register/"+id, nil)
	r.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	return rec
}

// TestRegisterKeyless: a node announcing itself with no key waits for
// approval, polling with its secret, then gets the fleet's trust - and
// is recorded trusting the fleet, with no credential kept for it.
func TestRegisterKeyless(t *testing.T) {
	a, _ := newTestApp(t)

	// No fleet yet: the node is told to announce with a credential.
	if rec := call(t, a.handleRegister, "POST", "/register", "/register", keylessRegistration(t, "lb1", "")); rec.Code != http.StatusConflict {
		t.Fatalf("without a fleet: %d %s", rec.Code, rec.Body)
	}

	withFleet(t, a)
	req := keylessRegistration(t, "lb1", "")
	rec := call(t, a.handleRegister, "POST", "/register", "/register", req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("announce: %d %s", rec.Code, rec.Body)
	}
	var ann struct{ ID string }
	_ = json.Unmarshal(rec.Body.Bytes(), &ann)
	pend := a.pending.List()
	if len(pend) != 1 || !pend[0].Keyless() || pend[0].ServiceKeyPEM != nil {
		t.Fatalf("pending: %+v", pend)
	}
	if _, err := os.Stat(filepath.Join(a.dataDir, "pending", ann.ID, "service.key")); !os.IsNotExist(err) {
		t.Errorf("a key on disk for a keyless announcement: %v", err)
	}
	list := call(t, a.handlePendingList, "GET", "/api/pending", "/api/pending", nil)
	if !strings.Contains(list.Body.String(), `"ca_fingerprint":"`) || !strings.Contains(list.Body.String(), `"keyless":true`) {
		t.Errorf("pending list: %s", list.Body)
	}

	if rec := poll(t, a, ann.ID, req.PollSecret); rec.Code != http.StatusAccepted {
		t.Errorf("before approval: %d", rec.Code)
	}
	if rec := poll(t, a, ann.ID, "wrong"); rec.Code != http.StatusForbidden {
		t.Errorf("another secret: %d", rec.Code)
	}

	if rec := call(t, a.handlePendingAction, "POST", "/api/pending/"+ann.ID+"/approve", "/api/pending/", nil); rec.Code != http.StatusCreated {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body)
	}
	nodes := a.store.List()
	if len(nodes) != 1 || !nodes[0].TrustsFleet() {
		t.Fatalf("admitted: %+v", nodes)
	}
	if cert, key := nodes[0].ServiceCredential(); cert != nil || key != nil {
		t.Error("a credential kept for a keyless node")
	}
	if len(a.pending.List()) != 0 {
		t.Error("still listed as pending once approved")
	}
	rec = poll(t, a, ann.ID, req.PollSecret)
	if rec.Code != http.StatusOK {
		t.Fatalf("after approval: %d %s", rec.Code, rec.Body)
	}
	var tr keylessTrust
	if err := json.Unmarshal(rec.Body.Bytes(), &tr); err != nil {
		t.Fatal(err)
	}
	node, err := pki.OpenFleet(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Set([]byte(tr.RootCert), tr.Bundle); err != nil {
		t.Errorf("the trust doesn't apply on a node: %v", err)
	}

	a.pending.RemoveApproved(nodes[0].ID)
	if rec := poll(t, a, ann.ID, req.PollSecret); rec.Code != http.StatusNotFound {
		t.Errorf("once its node took the trust: %d", rec.Code)
	}

	// Rejected: the node hears 404.
	rec = call(t, a.handleRegister, "POST", "/register", "/register", keylessRegistration(t, "lb2", ""))
	_ = json.Unmarshal(rec.Body.Bytes(), &ann)
	if rec := call(t, a.handlePendingAction, "POST", "/api/pending/"+ann.ID+"/reject", "/api/pending/", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("reject: %d", rec.Code)
	}
	if rec := poll(t, a, ann.ID, req.PollSecret); rec.Code != http.StatusNotFound {
		t.Errorf("rejected: %d", rec.Code)
	}

	for name, bad := range map[string]registerRequest{
		"no poll secret": {Protocol: 2, Name: "x", Address: "1.2.3.4:9505", CACertPEM: req.CACertPEM},
		"not a CA":       {Protocol: 2, Name: "x", Address: "1.2.3.4:9505", CACertPEM: "junk", PollSecret: req.PollSecret},
	} {
		if rec := call(t, a.handleRegister, "POST", "/register", "/register", bad); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", name, rec.Code)
		}
	}
}

// TestMachineKeylessToken: a machine this Controller created announces
// itself with no key and its token - admitted at once, with the fleet's
// trust, and no credential kept for it.
func TestMachineKeylessToken(t *testing.T) {
	a, fake := newTestApp(t)
	withFleet(t, a)
	h := addTrustedHypervisor(t, a)
	image := append([]byte("QFI\xfb\x00\x00\x00\x03"), make([]byte, 4096)...)
	sum := sha256.Sum256(image)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(image) }))
	defer srv.Close()
	rec := call(t, a.handleMachineCreate, "POST", "/api/machines", "POST /api/machines", machines.Spec{
		Name: "lb1", HypervisorID: h.ID,
		Image: &machines.ImageSource{URL: srv.URL + "/janus-kvm.qcow2", SHA256: hex.EncodeToString(sum[:])},
		NICs:  []machines.NIC{{Network: "lab-mgmt", Name: "mgmt", Mode: "dhcp"}},
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var v machineView
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	waitPhase(t, a, v.ID, machines.PhaseRegistering)
	path := filepath.Join(t.TempDir(), "cidata.iso")
	if err := os.WriteFile(path, fake.ciData, 0o600); err != nil {
		t.Fatal(err)
	}
	ci, err := nocloud.Read(path)
	if err != nil {
		t.Fatal(err)
	}

	rec = call(t, a.handleRegister, "POST", "/register", "/register", keylessRegistration(t, "lb1", ci.RegistrationToken))
	if rec.Code != http.StatusCreated {
		t.Fatalf("keyless token registration: %d %s", rec.Code, rec.Body)
	}
	var ans struct {
		Admitted bool
		Trust    *keylessTrust
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ans); err != nil || !ans.Admitted || ans.Trust == nil {
		t.Fatalf("answer: %s", rec.Body)
	}
	node, err := pki.OpenFleet(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Set([]byte(ans.Trust.RootCert), ans.Trust.Bundle); err != nil {
		t.Errorf("the trust doesn't apply on a node: %v", err)
	}
	m := waitPhase(t, a, v.ID, machines.PhaseReady)
	n, ok := a.store.Get(m.NodeID)
	if !ok || !n.TrustsFleet() {
		t.Fatalf("node %+v", n)
	}
	if cert, key := n.ServiceCredential(); cert != nil || key != nil {
		t.Error("a credential kept for the machine's keyless node")
	}
}
