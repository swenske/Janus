package proxmox

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/hypervisor"
)

// TestLive drives a real Proxmox VE node with a token prepared as
// docs/hypervisors.md says - skipped unless JANUS_PVE_URL is set. It
// creates one machine in the token's pool and removes it, and reads -
// only reads - one virtual machine outside the pool
// (JANUS_PVE_FOREIGN_VMID) to check the token can't see it.
//
//	JANUS_PVE_URL=https://pve:8006 JANUS_PVE_NODE=pve JANUS_PVE_TOKEN_ID=janus-ctl@pve!controller
//	JANUS_PVE_TOKEN_SECRET=... JANUS_PVE_FINGERPRINT=AA:BB:... JANUS_PVE_POOL=janus
//	JANUS_PVE_STORAGE=local-lvm JANUS_PVE_IMAGE_STORAGE=janus-images JANUS_PVE_NETWORK=vmbr0.10
//	JANUS_PVE_VMIDS=9100-9189 JANUS_PVE_IMAGE=janus.qcow2 JANUS_PVE_FOREIGN_VMID=100
func TestLive(t *testing.T) {
	env := func(k string) string { return os.Getenv("JANUS_PVE_" + k) }
	if env("URL") == "" {
		t.Skip("JANUS_PVE_URL unset: no Proxmox to test against")
	}
	h := &hypervisor.Hypervisor{Kind: hypervisor.KindProxmox, Name: "live", TokenSecret: env("TOKEN_SECRET"), Proxmox: &hypervisor.ProxmoxConfig{
		URL: env("URL"), Node: env("NODE"), TokenID: env("TOKEN_ID"), Fingerprint: env("FINGERPRINT"), Pool: env("POOL"),
		Storage: env("STORAGE"), ImageStorage: env("IMAGE_STORAGE"), Networks: []string{env("NETWORK")}, VMIDs: env("VMIDS"),
	}}
	if err := h.Validate(); err != nil {
		t.Fatal(err)
	}
	d, err := New(h, "ctl-live-test")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	host, err := d.HostInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("host: %s, %s, %d CPUs, %d running machines it may see, pool storage %s %v, networks %+v",
		host.Hostname, host.HypervisorVersion, host.CPUs, host.RunningMachines, host.Storage.Name, host.Storage.Active, host.Networks)
	if host.Storage.Error != "" || !host.Storage.Active || len(host.Networks) != 1 || host.Networks[0].Error != "" {
		t.Fatalf("host: %+v", host)
	}

	img := hypervisor.Image{Name: "janus-base-live-test.qcow2"}
	raw, err := os.ReadFile(env("IMAGE"))
	if err != nil {
		t.Fatal(err)
	}
	img.Size = int64(len(raw))
	if err := d.UploadImage(ctx, img, bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	if ok, err := d.HasImage(ctx, img); !ok || err != nil {
		t.Fatalf("HasImage after upload: %v %v", ok, err)
	}
	defer func() { _ = d.deleteVolume(context.Background(), d.volume("import", img.Name)) }()

	spec := hypervisor.MachineSpec{
		MachineID: "live" + time.Now().Format("150405"), Name: "janus-live-test", VCPUs: 1, MemoryMiB: 512, Image: img,
		CIData: bytes.Repeat([]byte{0}, 64<<10),
		NICs:   []hypervisor.NIC{{Network: env("NETWORK"), MAC: "52:54:00:4a:4e:01"}},
	}
	ref, err := d.CreateMachine(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	destroyed := false
	defer func() {
		if !destroyed {
			_ = d.DestroyMachine(context.Background(), *ref)
		}
	}()
	t.Logf("created VM %s", ref.UUID)

	// Its console shows the boot.
	var out lockedBuffer
	cctx, ccancel := context.WithTimeout(ctx, 40*time.Second)
	go func() { _ = d.Console(cctx, *ref, &out) }()
	time.Sleep(2 * time.Second)
	if err := d.Power(ctx, *ref, hypervisor.PowerReset); err != nil {
		t.Fatal(err)
	}
	for cctx.Err() == nil && !strings.Contains(out.String(), "Linux version") {
		time.Sleep(time.Second)
	}
	ccancel()
	if !strings.Contains(out.String(), "Linux version") {
		t.Errorf("no boot on the console: %q", out.String())
	}

	st, err := d.MachineStatus(ctx, *ref)
	if err != nil || st.Power != hypervisor.PowerRunning || st.VCPUs != 1 || st.MemoryMiB != 512 {
		t.Fatalf("status: %+v %v", st, err)
	}
	if err := d.Power(ctx, *ref, hypervisor.PowerForceOff); err != nil {
		t.Fatal(err)
	}
	nics := append(spec.NICs, hypervisor.NIC{Network: env("NETWORK"), MAC: "52:54:00:4a:4e:02"})
	if err := d.Reconfigure(ctx, *ref, 2, 768, nics); err != nil {
		t.Fatal(err)
	}
	vmid, c, err := d.owned(ctx, *ref)
	if err != nil || c.Cores != 2 || c.memoryMiB() != 768 || c.raw["net1"] == nil {
		t.Fatalf("after Reconfigure: %d %+v %v", vmid, c, err)
	}
	if err := d.Reconfigure(ctx, *ref, 2, 768, spec.NICs); err != nil {
		t.Fatal(err)
	}
	if _, c, _ = d.owned(ctx, *ref); c.raw["net1"] != nil {
		t.Errorf("net1 still there: %v", c.raw["net1"])
	}

	// Another Controller's record of the same machine: not ours.
	other := *ref
	other.MachineID = "someone-else"
	if _, err := d.MachineStatus(ctx, other); !errors.Is(err, hypervisor.ErrNotOwned) {
		t.Errorf("another machine ID: %v, want ErrNotOwned", err)
	}
	// A machine outside the pool: invisible.
	if id := env("FOREIGN_VMID"); id != "" {
		if _, err := d.MachineStatus(ctx, hypervisor.MachineRef{MachineID: spec.MachineID, UUID: id}); !errors.Is(err, hypervisor.ErrNotFound) {
			t.Errorf("VM %s outside the pool: %v, want ErrNotFound", id, err)
		}
	}

	if err := d.DestroyMachine(ctx, *ref); err != nil {
		t.Fatal(err)
	}
	destroyed = true
	if _, err := d.MachineStatus(ctx, *ref); !errors.Is(err, hypervisor.ErrNotFound) {
		t.Errorf("after DestroyMachine: %v", err)
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
