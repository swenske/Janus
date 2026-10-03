package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/swenske/Janus/dashboard/backend/internal/hypervisor"
	"github.com/swenske/Janus/dashboard/backend/internal/machines"
	"github.com/swenske/Janus/dashboard/backend/internal/nodeproxy"
	"github.com/swenske/Janus/dashboard/backend/internal/pending"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
	"github.com/swenske/Janus/internal/nocloud"
	"github.com/swenske/Janus/internal/pki"
)

// fakeDriver is a hypervisor that keeps its machines in memory.
type fakeDriver struct {
	mu        sync.Mutex
	images    map[string][]byte
	vms       map[string]hypervisor.MachineSpec // by UUID
	destroyed []string
	ciData    []byte
	powered   []hypervisor.PowerAction
	resized   [][2]int
	nics      []hypervisor.NIC
	// hw is what MachineStatus answers for every machine, when set.
	hw  *hypervisor.MachineStatus
	ops []string
	// console, when set, is what every machine's console prints before
	// it stays open; consoles counts the consoles opened.
	console  string
	consoles int
}

func newFakeDriver() *fakeDriver {
	return &fakeDriver{images: map[string][]byte{}, vms: map[string]hypervisor.MachineSpec{}}
}

func (f *fakeDriver) HostInfo(context.Context) (*hypervisor.HostInfo, error) {
	return &hypervisor.HostInfo{Hostname: "fake", CPUs: 4}, nil
}

func (f *fakeDriver) HasImage(_ context.Context, img hypervisor.Image) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.images[img.Name]
	return ok, nil
}

func (f *fakeDriver) UploadImage(_ context.Context, img hypervisor.Image, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if int64(len(b)) != img.Size {
		return errors.New("short upload")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.images[img.Name] = b
	return nil
}

func (f *fakeDriver) Plan(spec hypervisor.MachineSpec) hypervisor.MachineRef {
	return hypervisor.MachineRef{MachineID: spec.MachineID, Name: spec.Name, Volumes: []string{spec.Name + ".qcow2"}}
}

func (f *fakeDriver) CreateMachine(_ context.Context, spec hypervisor.MachineSpec) (*hypervisor.MachineRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.images[spec.Image.Name]; !ok {
		return nil, errors.New("no base image")
	}
	ref := f.Plan(spec)
	ref.UUID = "uuid-" + spec.MachineID
	f.vms[ref.UUID] = spec
	f.ciData = spec.CIData
	return &ref, nil
}

func (f *fakeDriver) MachineStatus(_ context.Context, ref hypervisor.MachineRef) (*hypervisor.MachineStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hw != nil {
		c := *f.hw
		return &c, nil
	}
	if _, ok := f.vms[ref.UUID]; !ok {
		return &hypervisor.MachineStatus{Power: hypervisor.PowerOff}, nil
	}
	return &hypervisor.MachineStatus{Power: hypervisor.PowerRunning}, nil
}

func (f *fakeDriver) Power(_ context.Context, _ hypervisor.MachineRef, action hypervisor.PowerAction) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.powered = append(f.powered, action)
	if f.hw != nil {
		switch action {
		case hypervisor.PowerStart, hypervisor.PowerReset:
			f.hw.Power = hypervisor.PowerRunning
		case hypervisor.PowerForceOff:
			f.hw.Power = hypervisor.PowerOff
		}
	}
	return nil
}

func (f *fakeDriver) Reconfigure(_ context.Context, ref hypervisor.MachineRef, vcpus, memoryMiB int, nics []hypervisor.NIC) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resized = append(f.resized, [2]int{vcpus, memoryMiB})
	f.nics = append([]hypervisor.NIC(nil), nics...)
	f.hw = &hypervisor.MachineStatus{Power: hypervisor.PowerRunning, VCPUs: vcpus, MemoryMiB: memoryMiB}
	f.ops = append(f.ops, "reconfigure")
	return nil
}

func (f *fakeDriver) Console(ctx context.Context, _ hypervisor.MachineRef, w io.Writer) error {
	f.mu.Lock()
	f.consoles++
	out := f.console
	f.mu.Unlock()
	if out == "" {
		_, err := w.Write([]byte("-----BEGIN EC PRIVATE KEY-----\nsecret\n-----END EC PRIVATE KEY-----\nok\n"))
		return err
	}
	if _, err := w.Write([]byte(out)); err != nil {
		return err
	}
	<-ctx.Done()
	return nil
}

func (f *fakeDriver) DestroyMachine(_ context.Context, ref hypervisor.MachineRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.vms, ref.UUID)
	f.destroyed = append(f.destroyed, ref.Name)
	return nil
}

func (f *fakeDriver) Close() error { return nil }

func newTestApp(t *testing.T) (*app, *fakeDriver) {
	t.Helper()
	dir := t.TempDir()
	portRangeStart, portRangeEnd = 39500, 39599
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	pend, err := pending.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := loadOrCreateDashboardIdentity(dir, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	hvs, err := hypervisor.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ms, err := machines.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := &app{
		store: st, pending: pend, serverCert: cert, listeners: map[string]*nodeproxy.Listener{},
		suggestedRegisterAddr: "192.0.2.1:8443", dataDir: dir, controllerID: "ctl-test",
		hypervisors: hvs, machines: ms,
	}
	a.runner = newMachineRunner(a)
	t.Cleanup(func() {
		for _, n := range a.store.List() {
			_ = a.removeNode(context.Background(), n.ID)
		}
	})

	fake := newFakeDriver()
	prev := newDriver
	newDriver = func(*hypervisor.Hypervisor, string) (hypervisor.Driver, error) { return fake, nil }
	t.Cleanup(func() { newDriver = prev })
	t.Cleanup(a.runner.waitJobs)     // jobs and console readers use newDriver:
	t.Cleanup(a.runner.stopWatching) // done with them first
	return a, fake
}

func addTrustedHypervisor(t *testing.T, a *app) *hypervisor.Hypervisor {
	t.Helper()
	h := &hypervisor.Hypervisor{Name: "kvm01", Kind: hypervisor.KindLibvirt, Libvirt: &hypervisor.LibvirtConfig{
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

func call(t *testing.T, h http.HandlerFunc, method, path, pattern string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	mux := http.NewServeMux()
	mux.HandleFunc(pattern, h)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(method, path, rd))
	return rec
}

func waitPhase(t *testing.T, a *app, id string, want machines.Phase) *machines.Machine {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		m, ok := a.machines.Get(id)
		if ok && m.Phase == want {
			return m
		}
		if ok && m.Phase == machines.PhaseFailed && want != machines.PhaseFailed {
			t.Fatalf("machine failed: %s (%v)", m.Error, m.Events)
		}
		time.Sleep(20 * time.Millisecond)
	}
	m, _ := a.machines.Get(id)
	t.Fatalf("machine never reached %s: %+v", want, m)
	return nil
}

// A registration as internal/selfregister sends it, with a real
// credential.
func registration(t *testing.T, name, token string) registerRequest {
	t.Helper()
	ca, err := pki.NewCA("node " + name)
	if err != nil {
		t.Fatal(err)
	}
	cert, key, err := ca.Issue(pki.IssueOptions{CommonName: name, Roles: []string{pki.RoleAdmin}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	if err != nil {
		t.Fatal(err)
	}
	return registerRequest{Name: name, Address: "127.0.0.1:1", CACertPEM: string(ca.CertPEM), ServiceCertPEM: string(cert), ServiceKeyPEM: string(key), RegistrationToken: token}
}

func TestMachineLifecycle(t *testing.T) {
	a, fake := newTestApp(t)
	h := addTrustedHypervisor(t, a)

	image := append([]byte("QFI\xfb\x00\x00\x00\x03"), make([]byte, 4096)...)
	sum := sha256.Sum256(image)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(image) }))
	defer srv.Close()

	// Create.
	rec := call(t, a.handleMachineCreate, "POST", "/api/machines", "POST /api/machines", machines.Spec{
		Name: "lb1", HypervisorID: h.ID,
		Image: &machines.ImageSource{URL: srv.URL + "/janus-kvm.qcow2", SHA256: hex.EncodeToString(sum[:])},
		NICs:  []machines.NIC{{Network: "lab-mgmt", Name: "mgmt", Mode: "static", Addresses: []string{"10.200.10.23/24"}}},
		NTP:   []string{"10.200.10.1"},
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var v machineView
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	if v.Spec.NICs[0].MAC == "" || v.Spec.VCPUs != 2 || v.Spec.MemoryMiB != 1024 {
		t.Errorf("defaults not filled in: %+v", v.Spec)
	}
	m := waitPhase(t, a, v.ID, machines.PhaseRegistering)
	if m.Ref == nil || m.Ref.UUID == "" || m.Ref.Name != "janus-lb1" {
		t.Fatalf("ref = %+v", m.Ref)
	}

	// The NoCloud volume the machine boots with carries the token, as the
	// node will read it.
	path := filepath.Join(t.TempDir(), "cidata.iso")
	if err := os.WriteFile(path, fake.ciData, 0o600); err != nil {
		t.Fatal(err)
	}
	ci, err := nocloud.Read(path)
	if err != nil {
		t.Fatalf("read the NoCloud volume: %v", err)
	}
	if ci.ControllerAddress != "192.0.2.1:8443" || ci.RegistrationToken == "" || ci.Network.GetHostname() != "lb1" || ci.Network.GetInterfaces()[0].GetMac() != m.Spec.NICs[0].MAC || ci.Network.GetNtp().GetServers()[0] != "10.200.10.1" {
		t.Fatalf("NoCloud config: %+v", ci)
	}

	// A token that isn't the machine's: the approval queue.
	rec = call(t, a.handleRegister, "POST", "/register", "/register", registration(t, "intruder", "not-the-token"))
	if rec.Code != http.StatusCreated || strings.Contains(rec.Body.String(), `"admitted":true`) || len(a.pending.List()) != 1 {
		t.Fatalf("unknown token: %d %s, %d pending", rec.Code, rec.Body, len(a.pending.List()))
	}

	// The machine's own token: admitted at once.
	rec = call(t, a.handleRegister, "POST", "/register", "/register", registration(t, "lb1", ci.RegistrationToken))
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"admitted":true`) {
		t.Fatalf("token registration: %d %s", rec.Code, rec.Body)
	}
	m = waitPhase(t, a, v.ID, machines.PhaseReady)
	node, ok := a.store.Get(m.NodeID)
	if !ok || node.MachineID != m.ID || len(a.pending.List()) != 1 {
		t.Fatalf("node %+v (%v), %d pending", node, ok, len(a.pending.List()))
	}

	// Used once: the same token again goes to the queue.
	rec = call(t, a.handleRegister, "POST", "/register", "/register", registration(t, "lb1-again", ci.RegistrationToken))
	if strings.Contains(rec.Body.String(), `"admitted":true`) || len(a.pending.List()) != 2 {
		t.Fatalf("reused token: %s, %d pending", rec.Body, len(a.pending.List()))
	}

	// The node can't be removed on its own.
	rec = call(t, a.handleNode, "DELETE", "/api/nodes/"+node.ID, "/api/nodes/", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete the machine's node: %d %s", rec.Code, rec.Body)
	}

	// The console hides private keys.
	consoleRec := call(t, a.handleMachineConsole, "GET", "/api/machines/"+m.ID+"/console", "GET /api/machines/{id}/console", nil)
	if body := consoleRec.Body.String(); strings.Contains(body, "secret") || !strings.Contains(body, "private key hidden") || !strings.Contains(body, `ok\n`) {
		t.Fatalf("console: %s", body)
	}

	// Destroy: the virtual machine, then the records.
	rec = call(t, a.handleMachineDelete, "DELETE", "/api/machines/"+m.ID, "DELETE /api/machines/{id}", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("destroy: %d %s", rec.Code, rec.Body)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, ok := a.machines.Get(m.ID); !ok {
			break
		}
		if time.Now().After(deadline) {
			mm, _ := a.machines.Get(m.ID)
			t.Fatalf("machine never destroyed: %+v", mm)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, ok := a.store.Get(node.ID); ok {
		t.Error("the node is still registered")
	}
	if len(fake.destroyed) != 1 || fake.destroyed[0] != "janus-lb1" {
		t.Errorf("destroyed %v", fake.destroyed)
	}
	rec = call(t, a.handleMachineGet, "GET", "/api/machines/"+m.ID, "GET /api/machines/{id}", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET after destroy: %d", rec.Code)
	}
}

func TestMachineCreateRefusals(t *testing.T) {
	a, _ := newTestApp(t)
	h := addTrustedHypervisor(t, a)
	untrusted := &hypervisor.Hypervisor{Name: "kvm02", Kind: hypervisor.KindLibvirt, Libvirt: &hypervisor.LibvirtConfig{
		Host: "192.0.2.3", User: "janus-ctl", Pool: "janus", Networks: []string{"lab-mgmt"},
	}}
	if err := a.hypervisors.Add(untrusted); err != nil {
		t.Fatal(err)
	}
	nic := []machines.NIC{{Network: "lab-mgmt"}}
	for name, spec := range map[string]machines.Spec{
		"bad name":          {Name: "not a name", HypervisorID: h.ID, NICs: nic},
		"unknown hv":        {Name: "lb1", HypervisorID: "nope", NICs: nic},
		"untrusted hv":      {Name: "lb1", HypervisorID: untrusted.ID, NICs: nic},
		"no nic":            {Name: "lb1", HypervisorID: h.ID},
		"forbidden network": {Name: "lb1", HypervisorID: h.ID, NICs: []machines.NIC{{Network: "prod-lan"}}},
		"bad version":       {Name: "lb1", HypervisorID: h.ID, NICs: nic, Version: "latest"},
		"multicast MAC":     {Name: "lb1", HypervisorID: h.ID, NICs: []machines.NIC{{Network: "lab-mgmt", MAC: "01:00:5e:00:00:01"}}},
		"static no address": {Name: "lb1", HypervisorID: h.ID, NICs: []machines.NIC{{Network: "lab-mgmt", Mode: "static", Addresses: []string{"nope"}}}},
		"too little memory": {Name: "lb1", HypervisorID: h.ID, NICs: nic, MemoryMiB: 128},
		"three NTP servers": {Name: "lb1", HypervisorID: h.ID, NICs: nic, NTP: []string{"a", "b", "c"}},
		"image and version": {Name: "lb1", HypervisorID: h.ID, NICs: nic, Version: "v2026.10.02", Image: &machines.ImageSource{URL: "https://x/y", SHA256: strings.Repeat("a", 64)}},
	} {
		rec := call(t, a.handleMachineCreate, "POST", "/api/machines", "POST /api/machines", spec)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if len(a.machines.List()) != 0 {
		t.Error("a refused machine was recorded")
	}
}

func TestMachineResumeAfterRestart(t *testing.T) {
	a, _ := newTestApp(t)
	m := &machines.Machine{Spec: machines.Spec{Name: "lb1"}, Phase: machines.PhaseImage}
	if err := a.machines.Add(m); err != nil {
		t.Fatal(err)
	}
	a.runner.resume()
	got, _ := a.machines.Get(m.ID)
	if got.Phase != machines.PhaseFailed || !strings.Contains(got.Error, "restarted") {
		t.Errorf("after resume: %s %q", got.Phase, got.Error)
	}
}

// A node from an image without registration tokens waits for approval;
// approved, it's linked to the machine waiting under its name.
func TestManualApprovalLinksTheMachine(t *testing.T) {
	a, _ := newTestApp(t)
	m := &machines.Machine{Spec: machines.Spec{Name: "lb1"}, Phase: machines.PhaseRegistering, Ref: &hypervisor.MachineRef{UUID: "u", Name: "janus-lb1"}}
	if err := a.machines.Add(m); err != nil {
		t.Fatal(err)
	}
	rec := call(t, a.handleRegister, "POST", "/register", "/register", registration(t, "lb1", ""))
	if rec.Code != http.StatusCreated || len(a.pending.List()) != 1 {
		t.Fatalf("register: %d %s", rec.Code, rec.Body)
	}
	p := a.pending.List()[0]
	rec = call(t, a.handlePendingAction, "POST", "/api/pending/"+p.ID+"/approve", "/api/pending/", nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body)
	}
	got, _ := a.machines.Get(m.ID)
	node, ok := a.store.Get(got.NodeID)
	if got.Phase != machines.PhaseReady || !ok || node.MachineID != m.ID {
		t.Errorf("machine %s node %q, node %+v", got.Phase, got.NodeID, node)
	}
}

func TestHypervisorPreparation(t *testing.T) {
	a, _ := newTestApp(t)
	h := addTrustedHypervisor(t, a)
	form := map[string]any{"name": "kvm02", "libvirt": map[string]any{"host": "kvm02", "user": "janus-ctl", "pool": "janus", "networks": []string{"lan"}}}
	const pattern = "POST /api/hypervisors/preparation"

	rec := call(t, a.handleHypervisorPreparation, "POST", "/api/hypervisors/preparation", pattern, form)
	var got preparationView
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil || got.HasKey || len(got.Steps) == 0 || !strings.Contains(got.Script, "50-janus-ctl.rules") {
		t.Fatalf("before it's added: %d %s", rec.Code, rec.Body)
	}
	// Added: its key is in it.
	form["id"] = h.ID
	rec = call(t, a.handleHypervisorPreparation, "POST", "/api/hypervisors/preparation", pattern, form)
	got = preparationView{}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil || !got.HasKey || !strings.Contains(got.Script, h.AuthorizedKey()) {
		t.Fatalf("added: %d %s", rec.Code, rec.Body)
	}
	form["id"] = "nope"
	if rec = call(t, a.handleHypervisorPreparation, "POST", "/api/hypervisors/preparation", pattern, form); rec.Code != http.StatusNotFound {
		t.Errorf("unknown hypervisor: %d", rec.Code)
	}
	delete(form, "id")
	// What Add refuses, it refuses too: nothing unchecked goes in a script.
	form["libvirt"].(map[string]any)["user"] = "janus ctl; reboot"
	if rec = call(t, a.handleHypervisorPreparation, "POST", "/api/hypervisors/preparation", pattern, form); rec.Code != http.StatusBadRequest {
		t.Errorf("a user name with a space: %d %s", rec.Code, rec.Body)
	}
}

// A Proxmox hypervisor: its token's secret is written, never read back,
// and kept when not given again; its certificate is only trusted
// through .../trust - or a CA the operator gives.
func TestProxmoxHypervisorAPI(t *testing.T) {
	a, _ := newTestApp(t)
	body := map[string]any{"name": "pve1", "token_secret": "s3cr3t-0000", "proxmox": map[string]any{
		"url": "https://pve1.example.net", "node": "pve1", "token_id": "janus-ctl@pve!controller", "pool": "janus",
		"storage": "local-lvm", "image_storage": "janus-images", "networks": []string{"vmbr0.10"},
		"fingerprint": strings.Repeat("AB:", 31) + "AB",
	}}
	rec := call(t, a.handleHypervisorCreate, "POST", "/api/hypervisors", "POST /api/hypervisors", body)
	if rec.Code != http.StatusCreated || strings.Contains(rec.Body.String(), "s3cr3t") {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var v hypervisorView
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	if v.Kind != hypervisor.KindProxmox || v.Trusted || !v.HasTokenSecret || v.HostKeyFingerprint != "" || v.AuthorizedKey != "" {
		t.Fatalf("view: %+v", v)
	}
	h, _ := a.hypervisors.Get(v.ID)
	if h.TokenSecret != "s3cr3t-0000" || h.Proxmox.Fingerprint != "" {
		t.Fatalf("stored: secret %q, fingerprint %q", h.TokenSecret, h.Proxmox.Fingerprint)
	}
	if info, err := os.Stat(filepath.Join(a.dataDir, "hypervisors", v.ID, "token")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("token file: %v %v", info, err)
	}
	if raw, _ := os.ReadFile(filepath.Join(a.dataDir, "hypervisors", v.ID, "meta.json")); strings.Contains(string(raw), "s3cr3t") {
		t.Fatal("the secret is in meta.json")
	}

	// Trusted by a CA: given with the settings; the secret stays.
	caPEM, _, _ := testCA(t)
	delete(body, "token_secret")
	body["proxmox"].(map[string]any)["ca_cert"] = caPEM
	rec = call(t, a.handleHypervisorUpdate, "PATCH", "/api/hypervisors/"+v.ID, "PATCH /api/hypervisors/{id}", body)
	v = hypervisorView{}
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	if rec.Code != http.StatusOK || !v.Trusted || v.CASubject == "" || !v.HasTokenSecret {
		t.Fatalf("with a CA: %d %+v", rec.Code, v)
	}
	if h, _ = a.hypervisors.Get(v.ID); h.TokenSecret != "s3cr3t-0000" {
		t.Fatalf("secret after an update without one: %q", h.TokenSecret)
	}

	rec = call(t, a.handleHypervisorPreparation, "POST", "/api/hypervisors/preparation", "POST /api/hypervisors/preparation", body)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "pveum acl modify /pool/janus") {
		t.Fatalf("preparation: %d %s", rec.Code, rec.Body)
	}
	// What Add refuses, the preparation refuses too.
	body["proxmox"].(map[string]any)["networks"] = []string{"vmbr0.10; reboot"}
	if rec = call(t, a.handleHypervisorPreparation, "POST", "/api/hypervisors/preparation", "POST /api/hypervisors/preparation", body); rec.Code != http.StatusBadRequest {
		t.Errorf("a network with a command: %d", rec.Code)
	}
}

// testCA is a CA certificate (PEM).
func testCA(t *testing.T) (string, *x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), cert, key
}

// GitHub or the image factory failing once doesn't fail a creation: the
// image is asked for again, then found - or the creation fails after
// imageAttempts tries.
func TestResolveImageRetries(t *testing.T) {
	a, _ := newTestApp(t)
	h := addTrustedHypervisor(t, a)
	prevResolve, prevWait := resolveVMImage, imageRetryWait
	t.Cleanup(func() { resolveVMImage, imageRetryWait = prevResolve, prevWait })
	imageRetryWait = 10 * time.Millisecond
	m := &machines.Machine{Spec: machines.Spec{Name: "node1", HypervisorID: h.ID}, Phase: machines.PhaseImage}
	if err := a.machines.Add(m); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		failures int
		ok       bool
	}{{0, true}, {imageAttempts - 1, true}, {imageAttempts, false}} {
		calls := 0
		resolveVMImage = func(context.Context, string, []string, string) (*nodeproxy.VMImage, error) {
			calls++
			if calls <= tc.failures {
				return nil, errors.New(`Get "https://api.github.com/repos/swenske/Janus/releases": context deadline exceeded`)
			}
			return &nodeproxy.VMImage{State: "ready", Version: "v2026.10.03-3", Schematic: strings.Repeat("a", 64), URL: "https://example.invalid/janus-kvm.qcow2", SHA256: strings.Repeat("b", 64)}, nil
		}
		src, img, err := a.runner.resolveImage(context.Background(), m)
		if tc.ok != (err == nil) || (tc.ok && (src == nil || img.Name != "janus-base-aaaaaaaa-v2026.10.03-3.qcow2")) {
			t.Errorf("%d failures: %v %+v %v", tc.failures, src, img, err)
		}
		if want := min(tc.failures+1, imageAttempts); calls != want {
			t.Errorf("%d failures: asked %d times, want %d", tc.failures, calls, want)
		}
	}
	got, _ := a.machines.Get(m.ID)
	retried := 0
	for _, e := range got.Events {
		if strings.HasPrefix(e.Message, "couldn't find the image yet") {
			retried++
		}
	}
	if retried != 2*(imageAttempts-1) {
		t.Errorf("%d retries in the history, want %d", retried, 2*(imageAttempts-1))
	}
}
