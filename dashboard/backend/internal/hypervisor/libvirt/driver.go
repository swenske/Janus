// Package libvirt is the Controller's hypervisor.Driver for libvirt
// (KVM/QEMU), spoken over SSH in pure Go (go-libvirt's RPC protocol on
// a forwarded Unix socket - see sshDialer).
package libvirt

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	golibvirt "github.com/digitalocean/go-libvirt"
	"golang.org/x/crypto/ssh"

	"github.com/swenske/Janus/dashboard/backend/internal/hypervisor"
)

// Driver talks to one libvirt host. Every operation opens its own
// connection (one SSH session) and closes it when done or when its
// context ends - nothing long-lived to keep healthy, and a stuck call
// never outlives its caller.
type Driver struct {
	cfg          hypervisor.LibvirtConfig
	signer       ssh.Signer
	hostKey      ssh.PublicKey
	controllerID string
}

// New returns a Driver for h, a libvirt hypervisor whose host key is
// already pinned.
func New(h *hypervisor.Hypervisor, controllerID string) (*Driver, error) {
	if h.Kind != hypervisor.KindLibvirt || h.Libvirt == nil {
		return nil, fmt.Errorf("hypervisor %s isn't a libvirt one", h.ID)
	}
	if h.Libvirt.HostKey == "" {
		return nil, errors.New("the host key hasn't been confirmed yet")
	}
	hostKey, _, _, _, err := ssh.ParseAuthorizedKey([]byte(h.Libvirt.HostKey))
	if err != nil {
		return nil, fmt.Errorf("pinned host key: %w", err)
	}
	signer, err := h.Signer()
	if err != nil {
		return nil, err
	}
	return &Driver{cfg: *h.Libvirt, signer: signer, hostKey: hostKey, controllerID: controllerID}, nil
}

func (d *Driver) Close() error { return nil }

// session is one connection.
type session struct {
	l      *golibvirt.Libvirt
	dialer *sshDialer
	done   chan struct{}
}

func (d *Driver) connect(ctx context.Context) (*session, error) {
	dialer := &sshDialer{addr: d.cfg.SSHAddress(), user: d.cfg.User, socket: d.cfg.SocketPath(), signer: d.signer, hostKey: d.hostKey}
	s := &session{l: golibvirt.NewWithDialer(dialer), dialer: dialer, done: make(chan struct{})}
	go func() {
		select {
		case <-ctx.Done():
			dialer.close()
		case <-s.done:
		}
	}()
	errc := make(chan error, 1)
	go func() { errc <- s.l.ConnectToURI(golibvirt.QEMUSystem) }()
	select {
	case err := <-errc:
		if err != nil {
			s.close()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		}
	case <-ctx.Done():
		s.close()
		return nil, ctx.Err()
	}
	return s, nil
}

func (s *session) close() {
	select {
	case <-s.done:
		return
	default:
	}
	close(s.done)
	finished := make(chan struct{})
	go func() {
		_ = s.l.Disconnect()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
	}
	s.dialer.close()
}

// run calls fn on a fresh session, returning ctx's error if it ends
// first: a libvirt stream whose connection breaks mid-way can leave
// go-libvirt waiting for good, and the caller mustn't wait with it.
func (d *Driver) run(ctx context.Context, fn func(l *golibvirt.Libvirt) error) error {
	s, err := d.connect(ctx)
	if err != nil {
		return err
	}
	errc := make(chan error, 1)
	go func() { errc <- fn(s.l) }()
	select {
	case err := <-errc:
		s.close()
		return err
	case <-ctx.Done():
		s.close()
		return ctx.Err()
	}
}

func isCode(err error, code golibvirt.ErrorNumber) bool {
	var e golibvirt.Error
	return errors.As(err, &e) && e.Code == uint32(code)
}

func version(v uint64) string {
	return fmt.Sprintf("%d.%d.%d", v/1000000, v/1000%1000, v%1000)
}

func (d *Driver) HostInfo(ctx context.Context) (*hypervisor.HostInfo, error) {
	info := &hypervisor.HostInfo{}
	err := d.run(ctx, func(l *golibvirt.Libvirt) error {
		var err error
		if info.Hostname, err = l.ConnectGetHostname(); err != nil {
			return err
		}
		hvType, err := l.ConnectGetType()
		if err != nil {
			return err
		}
		if v, err := l.ConnectGetVersion(); err == nil {
			info.HypervisorVersion = hvType + " " + version(v)
		}
		if v, err := l.ConnectGetLibVersion(); err == nil {
			info.LibraryVersion = "libvirt " + version(v)
		}
		model, memKiB, cpus, mhz, _, _, _, _, err := l.NodeGetInfo()
		if err != nil {
			return err
		}
		var mb []byte
		for _, c := range model {
			if c == 0 {
				break
			}
			mb = append(mb, byte(c))
		}
		info.CPUModel, info.CPUs, info.CPUMHz, info.MemoryTotal = string(mb), int(cpus), int(mhz), memKiB*1024
		if free, err := l.NodeGetFreeMemory(); err == nil {
			info.MemoryFree = free
		}
		info.CPUBusyNs, info.CPUTotalNs = cpuTimes(l)

		info.Storage.Name = d.cfg.Pool
		if pool, err := l.StoragePoolLookupByName(d.cfg.Pool); err != nil {
			info.Storage.Error = err.Error()
		} else if state, capacity, alloc, avail, err := l.StoragePoolGetInfo(pool); err != nil {
			info.Storage.Error = err.Error()
		} else {
			info.Storage.Active = state == uint8(golibvirt.StoragePoolRunning)
			info.Storage.Capacity, info.Storage.Allocated, info.Storage.Available = capacity, alloc, avail
		}
		for _, name := range d.cfg.Networks {
			n := hypervisor.NetworkInfo{Name: name}
			if net, err := l.NetworkLookupByName(name); err != nil {
				n.Error = err.Error()
			} else if active, err := l.NetworkIsActive(net); err != nil {
				n.Error = err.Error()
			} else {
				n.Active = active == 1
			}
			info.Networks = append(info.Networks, n)
		}
		info.RunningMachines = -1
		if n, err := l.ConnectNumOfDomains(); err == nil {
			info.RunningMachines = int(n)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return info, nil
}

// cpuTimes sums the host's CPU time counters (nanoseconds, all CPUs):
// busy is everything but idle and iowait.
func cpuTimes(l *golibvirt.Libvirt) (busy, total uint64) {
	const allCPUs = -1
	_, n, err := l.NodeGetCPUStats(allCPUs, 0, 0)
	if err != nil || n <= 0 {
		return 0, 0
	}
	params, _, err := l.NodeGetCPUStats(allCPUs, n, 0)
	if err != nil {
		return 0, 0
	}
	for _, p := range params {
		total += p.Value
		if p.Field != "idle" && p.Field != "iowait" {
			busy += p.Value
		}
	}
	return busy, total
}

func (d *Driver) HasImage(ctx context.Context, img hypervisor.Image) (bool, error) {
	found := false
	err := d.run(ctx, func(l *golibvirt.Libvirt) error {
		pool, err := l.StoragePoolLookupByName(d.cfg.Pool)
		if err != nil {
			return fmt.Errorf("storage pool %s: %w", d.cfg.Pool, err)
		}
		_, err = l.StorageVolLookupByName(pool, img.Name)
		switch {
		case err == nil:
			found = true
			return nil
		case isCode(err, golibvirt.ErrNoStorageVol):
			return nil
		default:
			return err
		}
	})
	return found, err
}

// UploadImage writes the image under a temporary name first and only
// then copies it to its real one: a base image is reused by every later
// machine, so a half-written one must never carry the name.
func (d *Driver) UploadImage(ctx context.Context, img hypervisor.Image, r io.Reader) error {
	partName := img.Name + ".part"
	return d.run(ctx, func(l *golibvirt.Libvirt) error {
		pool, err := l.StoragePoolLookupByName(d.cfg.Pool)
		if err != nil {
			return fmt.Errorf("storage pool %s: %w", d.cfg.Pool, err)
		}
		if old, err := l.StorageVolLookupByName(pool, partName); err == nil {
			_ = l.StorageVolDelete(old, 0) // left by an interrupted upload
		}
		part, err := l.StorageVolCreateXML(pool, volumeXML(partName, img.VirtualSize, "qcow2"), 0)
		if err != nil {
			return fmt.Errorf("create volume %s: %w", partName, err)
		}
		if err := l.StorageVolUpload(part, r, 0, uint64(img.Size), 0); err != nil {
			_ = l.StorageVolDelete(part, 0)
			return fmt.Errorf("upload %s: %w", img.Name, err)
		}
		_ = l.StoragePoolRefresh(pool, 0)
		if _, err := l.StorageVolCreateXMLFrom(pool, volumeXML(img.Name, img.VirtualSize, "qcow2"), part, 0); err != nil {
			_ = l.StorageVolDelete(part, 0)
			return fmt.Errorf("create volume %s: %w", img.Name, err)
		}
		if err := l.StorageVolDelete(part, 0); err != nil {
			return fmt.Errorf("remove %s: %w", partName, err)
		}
		return nil
	})
}

func (d *Driver) Plan(spec hypervisor.MachineSpec) hypervisor.MachineRef {
	return hypervisor.MachineRef{MachineID: spec.MachineID, Name: spec.Name, Volumes: []string{diskVolumeName(spec), ciDataVolumeName(spec)}}
}

func (d *Driver) CreateMachine(ctx context.Context, spec hypervisor.MachineSpec) (*hypervisor.MachineRef, error) {
	ref := &hypervisor.MachineRef{MachineID: spec.MachineID, Name: spec.Name}
	err := d.run(ctx, func(l *golibvirt.Libvirt) error {
		pool, err := l.StoragePoolLookupByName(d.cfg.Pool)
		if err != nil {
			return fmt.Errorf("storage pool %s: %w", d.cfg.Pool, err)
		}
		base, err := l.StorageVolLookupByName(pool, spec.Image.Name)
		if err != nil {
			return fmt.Errorf("base image %s: %w", spec.Image.Name, err)
		}
		_, capacity, _, err := l.StorageVolGetInfo(base)
		if err != nil {
			return fmt.Errorf("base image %s: %w", spec.Image.Name, err)
		}
		if _, err := l.DomainLookupByName(spec.Name); err == nil {
			return fmt.Errorf("a virtual machine named %s already exists on this hypervisor", spec.Name)
		}

		var created []golibvirt.StorageVol
		var dom *golibvirt.Domain
		undo := func() {
			if dom != nil {
				_ = l.DomainDestroy(*dom)
				_ = l.DomainUndefineFlags(*dom, golibvirt.DomainUndefineNvram|golibvirt.DomainUndefineManagedSave)
			}
			for _, v := range created {
				_ = l.StorageVolDelete(v, 0)
			}
		}

		diskName, ciName := diskVolumeName(spec), ciDataVolumeName(spec)
		disk, err := l.StorageVolCreateXMLFrom(pool, volumeXML(diskName, capacity, "qcow2"), base, 0)
		if err != nil {
			return fmt.Errorf("create disk %s: %w", diskName, err)
		}
		created = append(created, disk)
		ref.Volumes = append(ref.Volumes, diskName)

		ci, err := l.StorageVolCreateXML(pool, volumeXML(ciName, uint64(len(spec.CIData)), "raw"), 0)
		if err != nil {
			undo()
			return fmt.Errorf("create volume %s: %w", ciName, err)
		}
		created = append(created, ci)
		ref.Volumes = append(ref.Volumes, ciName)
		if err := l.StorageVolUpload(ci, bytes.NewReader(spec.CIData), 0, uint64(len(spec.CIData)), 0); err != nil {
			undo()
			return fmt.Errorf("upload %s: %w", ciName, err)
		}

		diskPath, err := l.StorageVolGetPath(disk)
		if err != nil {
			undo()
			return fmt.Errorf("volume %s: %w", diskName, err)
		}
		ciPath, err := l.StorageVolGetPath(ci)
		if err != nil {
			undo()
			return fmt.Errorf("volume %s: %w", ciName, err)
		}
		defined, err := l.DomainDefineXMLFlags(domainXML(spec, diskPath, ciPath, d.controllerID), golibvirt.DomainDefineValidate)
		if err != nil {
			undo()
			return fmt.Errorf("define the virtual machine: %w", err)
		}
		dom = &defined
		ref.UUID = formatUUID(defined.UUID)
		if err := l.DomainSetAutostart(defined, 1); err != nil {
			undo()
			return fmt.Errorf("enable autostart: %w", err)
		}
		if err := l.DomainCreate(defined); err != nil {
			undo()
			return fmt.Errorf("start the virtual machine: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ref, nil
}

// owned looks a machine up by its UUID and checks its ownership tag -
// the one gate every operation on a machine goes through.
func (d *Driver) owned(l *golibvirt.Libvirt, ref hypervisor.MachineRef) (golibvirt.Domain, error) {
	var dom golibvirt.Domain
	var err error
	if ref.UUID == "" {
		// Planned, never confirmed created: by name - the tag below
		// still decides whether it's ours.
		dom, err = l.DomainLookupByName(ref.Name)
	} else {
		uuid, perr := parseUUID(ref.UUID)
		if perr != nil {
			return dom, perr
		}
		dom, err = l.DomainLookupByUUID(uuid)
	}
	if err != nil {
		if isCode(err, golibvirt.ErrNoDomain) {
			return dom, hypervisor.ErrNotFound
		}
		return dom, err
	}
	meta, err := l.DomainGetMetadata(dom, int32(golibvirt.DomainMetadataElement), golibvirt.OptString{metadataNS}, golibvirt.DomainAffectCurrent)
	if err != nil {
		if isCode(err, golibvirt.ErrNoDomainMetadata) {
			return dom, hypervisor.ErrNotOwned
		}
		return dom, err
	}
	if !tagMatches(meta, d.controllerID, ref.MachineID) {
		return dom, hypervisor.ErrNotOwned
	}
	return dom, nil
}

func (d *Driver) MachineStatus(ctx context.Context, ref hypervisor.MachineRef) (*hypervisor.MachineStatus, error) {
	st := &hypervisor.MachineStatus{}
	err := d.run(ctx, func(l *golibvirt.Libvirt) error {
		dom, err := d.owned(l, ref)
		if err != nil {
			return err
		}
		state, _, maxMemKiB, _, nrCPU, cpuTime, err := domainInfo(l, dom)
		if err != nil {
			return err
		}
		st.Power, st.MemoryMiB, st.VCPUs, st.CPUTimeNs = powerState(state), int(maxMemKiB/1024), int(nrCPU), cpuTime
		return nil
	})
	if err != nil {
		return nil, err
	}
	return st, nil
}

func domainInfo(l *golibvirt.Libvirt, dom golibvirt.Domain) (state uint8, reason int32, maxMemKiB, memKiB uint64, nrCPU uint16, cpuTime uint64, err error) {
	state, maxMemKiB, memKiB, nrCPU, cpuTime, err = l.DomainGetInfo(dom)
	return
}

func powerState(s uint8) hypervisor.PowerState {
	switch golibvirt.DomainState(s) {
	case golibvirt.DomainRunning, golibvirt.DomainBlocked:
		return hypervisor.PowerRunning
	case golibvirt.DomainPaused, golibvirt.DomainPmsuspended:
		return hypervisor.PowerPaused
	case golibvirt.DomainShutdown:
		return hypervisor.PowerStopping
	case golibvirt.DomainShutoff:
		return hypervisor.PowerOff
	case golibvirt.DomainCrashed:
		return hypervisor.PowerCrashed
	}
	return hypervisor.PowerUnknown
}

func (d *Driver) Power(ctx context.Context, ref hypervisor.MachineRef, action hypervisor.PowerAction) error {
	return d.run(ctx, func(l *golibvirt.Libvirt) error {
		dom, err := d.owned(l, ref)
		if err != nil {
			return err
		}
		switch action {
		case hypervisor.PowerStart:
			err = l.DomainCreate(dom)
		case hypervisor.PowerForceOff:
			err = l.DomainDestroy(dom)
		case hypervisor.PowerReset:
			err = l.DomainReset(dom, 0)
		default:
			return fmt.Errorf("unknown power action %q", action)
		}
		// Starting a running machine or stopping a stopped one: already
		// where it was asked to be.
		if isCode(err, golibvirt.ErrOperationInvalid) && action != hypervisor.PowerReset {
			return nil
		}
		return err
	})
}

// Console streams the serial console. Without VIR_DOMAIN_CONSOLE_FORCE:
// a console someone already has open on the host (virsh console) wins,
// and this returns libvirt's error saying so.
func (d *Driver) Console(ctx context.Context, ref hypervisor.MachineRef, w io.Writer) error {
	return d.run(ctx, func(l *golibvirt.Libvirt) error {
		dom, err := d.owned(l, ref)
		if err != nil {
			return err
		}
		return l.DomainOpenConsole(dom, nil, w, 0)
	})
}

func (d *Driver) DestroyMachine(ctx context.Context, ref hypervisor.MachineRef) error {
	return d.run(ctx, func(l *golibvirt.Libvirt) error {
		pool, err := l.StoragePoolLookupByName(d.cfg.Pool)
		if err != nil {
			return fmt.Errorf("storage pool %s: %w", d.cfg.Pool, err)
		}
		// Only volumes this machine's records name, whose names carry its
		// ID - and, while the domain exists, that its disks really use.
		deletable := map[string]bool{}
		for _, v := range ref.Volumes {
			if ownsVolumeName(v, ref.MachineID) {
				deletable[v] = true
			}
		}

		dom, err := d.owned(l, ref)
		switch {
		case errors.Is(err, hypervisor.ErrNotFound):
			// Already gone (removed by hand): only its volumes are left.
		case err != nil:
			return err
		default:
			domXML, err := l.DomainGetXMLDesc(dom, 0)
			if err != nil {
				return err
			}
			files, err := domainDiskFiles(domXML)
			if err != nil {
				return fmt.Errorf("read the virtual machine's disks: %w", err)
			}
			inUse := map[string]bool{}
			for _, f := range files {
				inUse[f] = true
			}
			for v := range deletable {
				vol, err := l.StorageVolLookupByName(pool, v)
				if err != nil {
					continue // gone already, or never created
				}
				if path, err := l.StorageVolGetPath(vol); err != nil || !inUse[path] {
					delete(deletable, v)
				}
			}
			if err := l.DomainDestroy(dom); err != nil && !isCode(err, golibvirt.ErrOperationInvalid) {
				return fmt.Errorf("stop the virtual machine: %w", err)
			}
			flags := golibvirt.DomainUndefineNvram | golibvirt.DomainUndefineManagedSave | golibvirt.DomainUndefineSnapshotsMetadata | golibvirt.DomainUndefineCheckpointsMetadata
			if err := l.DomainUndefineFlags(dom, flags); err != nil && !isCode(err, golibvirt.ErrNoDomain) {
				return fmt.Errorf("remove the virtual machine: %w", err)
			}
		}

		var errs []string
		for v := range deletable {
			vol, err := l.StorageVolLookupByName(pool, v)
			if isCode(err, golibvirt.ErrNoStorageVol) {
				continue
			}
			if err == nil {
				err = l.StorageVolDelete(vol, 0)
			}
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", v, err))
			}
		}
		if len(errs) > 0 {
			return fmt.Errorf("remove volumes: %s", strings.Join(errs, "; "))
		}
		return nil
	})
}

func formatUUID(u golibvirt.UUID) string {
	h := hex.EncodeToString(u[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func parseUUID(s string) (golibvirt.UUID, error) {
	var u golibvirt.UUID
	b, err := hex.DecodeString(strings.ReplaceAll(s, "-", ""))
	if err != nil || len(b) != len(u) {
		return u, fmt.Errorf("invalid UUID %q", s)
	}
	copy(u[:], b)
	return u, nil
}
