package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/netconfig"

	"github.com/swenske/Janus/dashboard/backend/internal/cidata"
	"github.com/swenske/Janus/dashboard/backend/internal/hypervisor"
	"github.com/swenske/Janus/dashboard/backend/internal/machines"
	"github.com/swenske/Janus/dashboard/backend/internal/nodeproxy"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
)

// Machines: Janus nodes the Controller creates on a hypervisor itself,
// from the image to the admitted node. Creating one is a job that runs
// in the background (machineRunner) - minutes of downloading, uploading
// and booting no browser tab should have to stay open for - and whose
// progress lives with the machine (its phase and events), polled by the
// page (or a client such as a Terraform provider).

const (
	// registrationTimeout bounds the wait for a created machine to
	// announce itself. Its token stays valid for registrationTokenTTL: a
	// node that boots late is still admitted.
	registrationTimeout  = 15 * time.Minute
	registrationTokenTTL = 24 * time.Hour
	// factoryBuildTimeout bounds the wait for the image factory to build
	// a schematic's images.
	factoryBuildTimeout = 60 * time.Minute
)

// answerTimeout bounds the wait, once a machine's node is admitted, for
// it to answer the Controller (awaitAnswer); answerRetry is how often it
// is asked meanwhile.
var answerTimeout, answerRetry = 2 * time.Minute, 2 * time.Second

// vmImageFile is the release (or image factory) asset for a kind of
// hypervisor - the same disk, named as the site offers it.
func vmImageFile(h *hypervisor.Hypervisor) string {
	if h.Kind == hypervisor.KindProxmox {
		return "janus.qcow2" // image/kvm-proxmox
	}
	return "janus-kvm.qcow2" // image/kvm
}

type machineRunner struct {
	a *app

	mu   sync.Mutex
	jobs map[string]*machineJob
	// watching: machines whose console is read for their node's
	// registration (watchNode); quit stops those readers.
	watching map[string]bool
	watchers sync.WaitGroup
	quit     chan struct{}
	stopOnce sync.Once
}

type machineJob struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func newMachineRunner(a *app) *machineRunner {
	return &machineRunner{a: a, jobs: map[string]*machineJob{}, quit: make(chan struct{})}
}

// resume picks up after a restart: work that was under way is reported
// as interrupted (the operator retries or destroys), and machines still
// waiting for their node get their timeout back.
func (r *machineRunner) resume() {
	for _, m := range r.a.machines.List() {
		switch {
		case m.Phase == machines.PhaseUpdating:
			// Its node is still there: the machine stays ready.
			_, _ = r.a.machines.Update(m.ID, func(m *machines.Machine) error {
				m.Phase, m.Error = machines.PhaseReady, "update interrupted: the Controller restarted"
				m.Log("%s", m.Error)
				return nil
			})
		case m.Phase.Busy():
			_, _ = r.a.machines.Update(m.ID, func(m *machines.Machine) error {
				m.Error = fmt.Sprintf("interrupted while %s: the Controller restarted", m.Phase)
				m.Log("%s", m.Error)
				m.Phase = machines.PhaseFailed
				return nil
			})
		case m.Phase == machines.PhaseRegistering && m.NodeID != "":
			// Admitted, not yet answering when the Controller stopped.
			if node, ok := r.a.store.Get(m.NodeID); ok {
				r.awaitAnswer(m.ID, node)
			}
		case m.Phase == machines.PhaseRegistering:
			r.watchRegistration(m.ID, time.Until(m.UpdatedAt.Add(registrationTimeout)))
			r.watchNode(m.ID)
		}
	}
}

// waitJobs waits for every job running - tests, before they put back
// what the jobs use.
func (r *machineRunner) waitJobs() {
	r.mu.Lock()
	var done []chan struct{}
	for _, j := range r.jobs {
		done = append(done, j.done)
	}
	r.mu.Unlock()
	for _, d := range done {
		<-d
	}
}

// start runs fn as id's job, unless one is already running.
func (r *machineRunner) start(id string, fn func(ctx context.Context) error) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, busy := r.jobs[id]; busy {
		return false
	}
	ctx, cancel := context.WithCancel(context.Background())
	job := &machineJob{cancel: cancel, done: make(chan struct{})}
	r.jobs[id] = job
	go func() {
		defer func() {
			cancel()
			r.mu.Lock()
			delete(r.jobs, id)
			r.mu.Unlock()
			close(job.done)
		}()
		if err := fn(ctx); err != nil {
			log.Printf("machine %s: %v", id, err)
			_, _ = r.a.machines.Update(id, func(m *machines.Machine) error {
				m.Phase, m.Error = machines.PhaseFailed, err.Error()
				m.Log("failed: %v", err)
				return nil
			})
		}
	}()
	return true
}

// stop cancels id's job, if any, and waits for it to end.
func (r *machineRunner) stop(id string) {
	r.mu.Lock()
	job, ok := r.jobs[id]
	r.mu.Unlock()
	if ok {
		job.cancel()
		<-job.done
	}
}

func (r *machineRunner) logEvent(id, format string, args ...any) {
	_, _ = r.a.machines.Update(id, func(m *machines.Machine) error {
		m.Log(format, args...)
		return nil
	})
}

func (r *machineRunner) phase(id string, p machines.Phase, format string, args ...any) error {
	_, err := r.a.machines.Update(id, func(m *machines.Machine) error {
		m.Phase, m.Error = p, ""
		m.Log(format, args...)
		return nil
	})
	return err
}

// registered is called once the machine's node announced itself with
// its token and was admitted. The machine is ready once the node answers
// the Controller (awaitAnswer).
func (r *machineRunner) registered(id string, node *store.Node) {
	_, _ = r.a.machines.Update(id, func(m *machines.Machine) error {
		m.NodeID, m.Warning = node.ID, ""
		m.Log("the node registered from %s and was admitted", node.Address)
		return nil
	})
	r.awaitAnswer(id, node)
}

// awaitAnswer makes the machine ready once its node answers the
// Controller, in the background. A keyless node takes the fleet's trust
// from the answer to its registration and applies it a moment later:
// until then it refuses the Controller's certificate, and a client told
// "ready" - Terraform, applying the node's HAProxy configuration next -
// would fail. A node still silent after answerTimeout is ready all the
// same, with a warning: it was admitted.
func (r *machineRunner) awaitAnswer(id string, node *store.Node) {
	r.watchers.Add(1)
	go func() {
		defer r.watchers.Done()
		ctx, cancel := context.WithTimeout(context.Background(), answerTimeout)
		defer cancel()
		var err error
		for {
			call, done := context.WithTimeout(ctx, 10*time.Second)
			_, err = nodes.Info(call, node)
			done()
			if err == nil {
				break
			}
			select {
			case <-r.quit:
				return // picked up again at the next start (resume)
			case <-ctx.Done():
			case <-time.After(answerRetry):
			}
			if ctx.Err() != nil {
				break
			}
		}
		_, _ = r.a.machines.Update(id, func(m *machines.Machine) error {
			if m.NodeID != node.ID || m.Phase == machines.PhaseDestroying {
				return errSkip
			}
			m.Phase, m.Error = machines.PhaseReady, ""
			if err != nil {
				m.Warning = fmt.Sprintf("the node was admitted but doesn't answer the Controller: %v", err)
				m.Log("%s", m.Warning)
			} else {
				m.Log("the node answers the Controller")
			}
			return nil
		})
	}()
}

// waitingFor is the machine named name still waiting for its node, if
// any.
func (r *machineRunner) waitingFor(name string) string {
	for _, m := range r.a.machines.List() {
		if m.Spec.Name == name && m.NodeID == "" && m.Ref != nil && (m.Phase == machines.PhaseRegistering || m.Phase == machines.PhaseFailed) {
			return m.ID
		}
	}
	return ""
}

// watchRegistration fails the machine if its node hasn't registered in
// time - the virtual machine is left running, for its console to tell
// why.
func (r *machineRunner) watchRegistration(id string, timeout time.Duration) {
	if timeout < 0 {
		timeout = 0
	}
	time.AfterFunc(timeout, func() {
		_, _ = r.a.machines.Update(id, func(m *machines.Machine) error {
			if m.Phase != machines.PhaseRegistering || m.NodeID != "" {
				return nil
			}
			m.Phase = machines.PhaseFailed
			m.Error = fmt.Sprintf("the node hasn't registered within %s - its console may say why (it's still admitted if it registers later)", registrationTimeout)
			m.Log("%s", m.Error)
			return nil
		})
	})
}

// create is the creation job: image, machine, then waiting for its node.
func (r *machineRunner) create(ctx context.Context, id string) error {
	a := r.a
	m, ok := a.machines.Get(id)
	if !ok {
		return errors.New("machine vanished")
	}
	h, ok := a.hypervisors.Get(m.Spec.HypervisorID)
	if !ok {
		return fmt.Errorf("hypervisor %s no longer exists", m.Spec.HypervisorID)
	}
	drv, err := newDriver(h, a.controllerID)
	if err != nil {
		return err
	}
	defer drv.Close()

	// The image: once per schematic and version on each hypervisor.
	if err := r.phase(id, machines.PhaseImage, "looking for the image"); err != nil {
		return err
	}
	src, img, err := r.resolveImage(ctx, m)
	if err != nil {
		return err
	}
	have, err := drv.HasImage(ctx, img)
	if err != nil {
		return fmt.Errorf("check the hypervisor's images: %w", err)
	}
	if have {
		r.logEvent(id, "image %s is already on %s", img.Name, h.Name)
	} else if err := r.upload(ctx, id, drv, img, src); err != nil {
		return err
	}

	// The machine.
	if err := r.phase(id, machines.PhaseCreating, "creating the virtual machine"); err != nil {
		return err
	}
	ctlAddr := h.ControllerAddress
	if ctlAddr == "" {
		ctlAddr = a.suggestedRegisterAddr
	}
	if ctlAddr == "" {
		return errors.New("the Controller doesn't know the address its nodes reach it at: set the hypervisor's controller address (or dashboardd -advertise-address)")
	}
	token, err := a.machines.NewToken(id, registrationTokenTTL)
	if err != nil {
		return err
	}
	m, _ = a.machines.Get(id)
	netcfg, err := networkConfig(m.Spec)
	if err != nil {
		return err
	}
	var fleetRoot []byte
	if a.fleet != nil {
		fleetRoot, _ = a.fleet.RootPEM()
	}
	ci, err := cidata.Build(a.dataDir, cidata.UserData{
		ControllerAddress:   ctlAddr,
		ControllerCACert:    string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: a.serverCert.Certificate[0]})),
		RegistrationToken:   token,
		ControllerFleetRoot: string(fleetRoot),
		Network:             netcfg,
	})
	if err != nil {
		return fmt.Errorf("NoCloud volume: %w", err)
	}
	spec := hypervisor.MachineSpec{
		MachineID: id,
		Name:      h.Prefix() + m.Spec.Name,
		VCPUs:     m.Spec.VCPUs,
		MemoryMiB: m.Spec.MemoryMiB,
		Image:     img,
		CIData:    ci,
	}
	for _, n := range m.Spec.NICs {
		spec.NICs = append(spec.NICs, hypervisor.NIC{Network: n.Network, MAC: n.MAC})
	}
	plan := drv.Plan(spec)
	if _, err := a.machines.Update(id, func(m *machines.Machine) error { m.Ref = &plan; return nil }); err != nil {
		return err
	}
	ref, err := drv.CreateMachine(ctx, spec)
	if err != nil {
		return err
	}
	_, err = a.machines.Update(id, func(m *machines.Machine) error {
		m.Ref = ref
		// Already registered (a fast boot beats this update)? Then ready.
		if m.NodeID == "" {
			m.Phase, m.Warning = machines.PhaseRegistering, ""
			m.Log("%s started on %s - waiting for the node to register at %s", ref.Name, h.Name, ctlAddr)
		} else {
			m.Log("%s started on %s", ref.Name, h.Name)
		}
		return nil
	})
	if err != nil {
		return err
	}
	r.watchRegistration(id, registrationTimeout)
	r.watchNode(id)
	return nil
}

// imageSource is where an image is downloaded from.
type imageSource struct {
	URL    string
	SHA256 string
	Size   int64 // 0: unknown
}

// describeImage says what a spec's image carries beyond the default one.
func describeImage(spec machines.Spec) string {
	var parts []string
	if len(spec.Extensions) > 0 {
		parts = append(parts, strings.Join(spec.Extensions, ", "))
	}
	if spec.HAProxy != "" {
		parts = append(parts, "HAProxy "+spec.HAProxy)
	}
	if spec.Kernel != "" {
		parts = append(parts, "kernel "+spec.Kernel)
	}
	if len(parts) == 0 {
		return "the default image"
	}
	return strings.Join(parts, ", ")
}

// resolveVMImage finds a release's (or the image factory's) image - a
// variable so tests can give answers of their own.
var resolveVMImage = nodeproxy.ResolveVMImage

// imageAttempts is how many times in a row resolveImage asks before
// giving up, imageRetryWait the wait after the first failure (doubled
// after each): GitHub or the image factory answering slowly once
// shouldn't fail a machine's creation.
var (
	imageAttempts  = 3
	imageRetryWait = 20 * time.Second
)

// resolveImage finds the machine's image, waiting while the image
// factory builds it.
func (r *machineRunner) resolveImage(ctx context.Context, m *machines.Machine) (*imageSource, hypervisor.Image, error) {
	if src := m.Spec.Image; src != nil {
		name := "janus-base-sha256-" + src.SHA256[:16] + ".qcow2"
		r.logEvent(m.ID, "image %s, from %s", name, src.URL)
		return &imageSource{URL: src.URL, SHA256: src.SHA256}, hypervisor.Image{Name: name}, nil
	}
	deadline := time.Now().Add(factoryBuildTimeout)
	reported := false
	failures := 0
	for {
		h, ok := r.a.hypervisors.Get(m.Spec.HypervisorID)
		if !ok {
			return nil, hypervisor.Image{}, errors.New("its hypervisor no longer exists")
		}
		vi, err := resolveVMImage(ctx, m.Spec.Version, m.Spec.Schematic(), vmImageFile(h))
		if err != nil {
			failures++
			if failures >= imageAttempts || ctx.Err() != nil {
				return nil, hypervisor.Image{}, fmt.Errorf("find the image: %w", err)
			}
			wait := imageRetryWait << (failures - 1)
			r.logEvent(m.ID, "couldn't find the image yet (%v) - trying again in %s", err, wait)
			select {
			case <-ctx.Done():
				return nil, hypervisor.Image{}, ctx.Err()
			case <-time.After(wait):
			}
			continue
		}
		failures = 0
		switch vi.State {
		case "ready":
			if _, err := r.a.machines.Update(m.ID, func(m *machines.Machine) error {
				m.Version, m.Schematic = vi.Version, vi.Schematic
				return nil
			}); err != nil {
				return nil, hypervisor.Image{}, err
			}
			name := fmt.Sprintf("janus-base-%.8s-%s.qcow2", vi.Schematic, vi.Version)
			r.logEvent(m.ID, "image: Janus %s, schematic %.8s", vi.Version, vi.Schematic)
			return &imageSource{URL: vi.URL, SHA256: vi.SHA256, Size: vi.Size}, hypervisor.Image{Name: name}, nil
		case "building":
			if !reported {
				r.logEvent(m.ID, "the image factory is building Janus %s, schematic %.8s (%s)", vi.Version, vi.Schematic, describeImage(m.Spec))
				reported = true
			}
			if time.Now().After(deadline) {
				return nil, hypervisor.Image{}, fmt.Errorf("the image factory hasn't built the image within %s", factoryBuildTimeout)
			}
			select {
			case <-ctx.Done():
				return nil, hypervisor.Image{}, ctx.Err()
			case <-time.After(30 * time.Second):
			}
		default:
			msg := vi.Message
			if msg == "" {
				msg = "image " + vi.State
			}
			return nil, hypervisor.Image{}, errors.New(msg)
		}
	}
}

// upload downloads the image into the data directory, checks it, and
// sends it to the hypervisor: never an unchecked byte on a hypervisor.
func (r *machineRunner) upload(ctx context.Context, id string, drv hypervisor.Driver, img hypervisor.Image, src *imageSource) error {
	dir := filepath.Join(r.a.dataDir, "images")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, img.Name+".download")
	defer os.Remove(path)

	r.logEvent(id, "downloading %s", src.URL)
	size, err := download(ctx, src, path)
	if err != nil {
		return fmt.Errorf("download the image: %w", err)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	vsize, err := hypervisor.QCOW2VirtualSize(f)
	if err != nil {
		return fmt.Errorf("the image: %w", err)
	}
	img.Size, img.VirtualSize = size, vsize
	r.logEvent(id, "image checked (%d MiB) - sending it to the hypervisor", size>>20)
	if err := drv.UploadImage(ctx, img, f); err != nil {
		return fmt.Errorf("send the image to the hypervisor: %w", err)
	}
	r.logEvent(id, "image %s is on the hypervisor", img.Name)
	return nil
}

// imageClient downloads images - a variable so tests can swap it.
var imageClient = &http.Client{Timeout: 30 * time.Minute}

// maxImageSize bounds a download whose size isn't known in advance.
const maxImageSize = 4 << 30

func download(ctx context.Context, src *imageSource, path string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src.URL, nil)
	if err != nil {
		return 0, err
	}
	resp, err := imageClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("GET %s: %s", src.URL, resp.Status)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxImageSize+1))
	if err != nil {
		return 0, err
	}
	if n > maxImageSize {
		return 0, fmt.Errorf("larger than %d bytes", int64(maxImageSize))
	}
	if src.Size > 0 && n != src.Size {
		return 0, fmt.Errorf("%d bytes, %d expected", n, src.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, src.SHA256) {
		return 0, fmt.Errorf("SHA-256 %s, %s expected - not used", got, src.SHA256)
	}
	return n, f.Sync()
}

// destroy is the destruction job: the node first (a clean shutdown, so
// HAProxy and any VRRP/BGP peer see it go), then the virtual machine.
func (r *machineRunner) destroy(ctx context.Context, id string) error {
	a := r.a
	m, ok := a.machines.Get(id)
	if !ok {
		return nil
	}
	h, ok := a.hypervisors.Get(m.Spec.HypervisorID)
	if !ok {
		return fmt.Errorf("hypervisor %s no longer exists", m.Spec.HypervisorID)
	}
	drv, err := newDriver(h, a.controllerID)
	if err != nil {
		return err
	}
	defer drv.Close()

	if node, ok := a.store.Get(m.NodeID); ok && m.Ref != nil {
		sctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := nodes.Shutdown(sctx, node)
		cancel()
		if err != nil {
			r.logEvent(id, "the node didn't take a clean shutdown (%v) - powering it off", err)
		} else {
			r.logEvent(id, "the node is shutting down")
			waitPoweredOff(ctx, drv, *m.Ref, 90*time.Second)
		}
	}
	if m.Ref != nil {
		if err := drv.DestroyMachine(ctx, *m.Ref); err != nil {
			return fmt.Errorf("destroy the virtual machine: %w", err)
		}
		r.logEvent(id, "%s removed from %s", m.Ref.Name, h.Name)
	}
	return r.forget(ctx, id)
}

func waitPoweredOff(ctx context.Context, drv hypervisor.Driver, ref hypervisor.MachineRef, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		st, err := drv.MachineStatus(ctx, ref)
		if err != nil || st.Power == hypervisor.PowerOff {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

// forget drops the Controller's records of a machine and its node,
// without touching the hypervisor.
func (r *machineRunner) forget(ctx context.Context, id string) error {
	m, ok := r.a.machines.Get(id)
	if !ok {
		return nil
	}
	if m.NodeID != "" {
		if err := r.a.removeNode(ctx, m.NodeID); err != nil {
			log.Printf("machine %s: remove node %s: %v", id, m.NodeID, err)
		}
	}
	return r.a.machines.Remove(id)
}

// removeNode forgets a node: its page, its connection, its record.
func (a *app) removeNode(_ context.Context, id string) error {
	a.forgetNodePage(id)
	nodeproxy.Forget(id)
	return a.store.Remove(id)
}

// --- the machine's network: the node's NetworkConfig ---

var macRe = regexp.MustCompile(`^([0-9a-f]{2}:){5}[0-9a-f]{2}$`)

// randomMAC is a locally administered address in QEMU's usual
// 52:54:00 range.
func randomMAC() (string, error) {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("52:54:00:%02x:%02x:%02x", b[0], b[1], b[2]), nil
}

// networkConfig is the node's network configuration (its NoCloud
// user-data's): its hostname, and each NIC matched by its MAC.
func networkConfig(spec machines.Spec) (*janusv1alpha1.NetworkConfig, error) {
	cfg := &janusv1alpha1.NetworkConfig{Hostname: spec.Name}
	for _, n := range spec.NICs {
		iface := &janusv1alpha1.NetworkInterface{Name: n.Name, Mac: n.MAC, Addresses: n.Addresses, Gateway: n.Gateway}
		switch n.Mode {
		case "dhcp", "":
			iface.Mode = janusv1alpha1.AddressingMode_ADDRESSING_MODE_DHCP
		case "static":
			iface.Mode = janusv1alpha1.AddressingMode_ADDRESSING_MODE_STATIC
		case "none":
			iface.Mode = janusv1alpha1.AddressingMode_ADDRESSING_MODE_NONE
		case "disabled":
			iface.Mode = janusv1alpha1.AddressingMode_ADDRESSING_MODE_DISABLED
		default:
			return nil, fmt.Errorf("interface %s: mode %q (want static, dhcp, none or disabled)", n.Name, n.Mode)
		}
		cfg.Interfaces = append(cfg.Interfaces, iface)
	}
	if len(spec.DNS) > 0 {
		cfg.Dns = &janusv1alpha1.NetworkDNS{Servers: spec.DNS}
	}
	if len(spec.NTP) > 0 {
		cfg.Ntp = &janusv1alpha1.NetworkNTP{Servers: spec.NTP}
	}
	if err := netconfig.Validate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

var versionRe = regexp.MustCompile(`^v\d{4}\.\d{2}\.\d{2}(-\d+)?$`)
var sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)

// checkSpec validates a machine to create, filling in its defaults and
// MAC addresses.
func (a *app) checkSpec(spec *machines.Spec) error {
	spec.Name = strings.TrimSpace(spec.Name)
	if err := netconfig.ValidateHostname(spec.Name); err != nil || spec.Name == "" {
		return fmt.Errorf("name %q isn't a valid hostname", spec.Name)
	}
	h, ok := a.hypervisors.Get(spec.HypervisorID)
	if !ok {
		return fmt.Errorf("no such hypervisor %q", spec.HypervisorID)
	}
	if !h.Trusted() {
		return fmt.Errorf("hypervisor %s isn't trusted yet: confirm its host key or certificate first", h.Name)
	}
	for _, m := range a.machines.List() {
		if m.Spec.Name == spec.Name {
			return fmt.Errorf("a machine named %s already exists", spec.Name)
		}
	}
	if spec.VCPUs == 0 {
		spec.VCPUs = 2
	}
	if spec.MemoryMiB == 0 {
		spec.MemoryMiB = 1024
	}
	if spec.VCPUs < 1 || spec.VCPUs > 64 {
		return errors.New("vcpus: 1 to 64")
	}
	if spec.MemoryMiB < 512 || spec.MemoryMiB > 262144 {
		return errors.New("memory_mib: 512 to 262144")
	}
	if spec.Version != "" && !versionRe.MatchString(spec.Version) {
		return fmt.Errorf("version %q isn't a Janus release (vYYYY.MM.DD[-N])", spec.Version)
	}
	if err := spec.Schematic().Normalize(); err != nil {
		return err
	}
	if src := spec.Image; src != nil {
		src.SHA256 = strings.ToLower(strings.TrimSpace(src.SHA256))
		u, err := url.Parse(src.URL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return fmt.Errorf("image URL %q isn't an http(s) URL", src.URL)
		}
		if !sha256Re.MatchString(src.SHA256) {
			return errors.New("image sha256: 64 hexadecimal characters")
		}
		if len(spec.Extensions) > 0 || spec.Version != "" || spec.HAProxy != "" || spec.Kernel != "" {
			return errors.New("an image URL replaces version, extensions, HAProxy branch and kernel track: give one or the other")
		}
	}
	if len(spec.NICs) == 0 || len(spec.NICs) > 8 {
		return errors.New("nics: 1 to 8 network interfaces")
	}
	seen := map[string]bool{}
	for i := range spec.NICs {
		n := &spec.NICs[i]
		if !h.AllowsNetwork(n.Network) {
			return fmt.Errorf("network %q isn't one this hypervisor allows (%s)", n.Network, strings.Join(h.Networks(), ", "))
		}
		if n.Name == "" {
			n.Name = fmt.Sprintf("eth%d", i)
		}
		if n.Mode == "" {
			n.Mode = "dhcp"
		}
		if n.MAC == "" {
			mac, err := randomMAC()
			if err != nil {
				return err
			}
			n.MAC = mac
		}
		n.MAC = strings.ToLower(n.MAC)
		if hw, err := net.ParseMAC(n.MAC); err != nil || !macRe.MatchString(n.MAC) || hw[0]&1 == 1 {
			return fmt.Errorf("MAC address %q isn't a unicast Ethernet address", n.MAC)
		}
		if seen[n.MAC] {
			return fmt.Errorf("MAC address %s is used twice", n.MAC)
		}
		seen[n.MAC] = true
	}
	_, err := networkConfig(*spec)
	return err
}
