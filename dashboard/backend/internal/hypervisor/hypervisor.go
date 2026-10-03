// Package hypervisor is what the Controller knows about the hypervisors
// it creates Janus nodes on: their configuration, kept as plain files
// under the data directory like internal/store's nodes (Store), and the
// Driver each kind implements (libvirt today, in the libvirt
// subpackage).
//
// A Controller only ever acts on the virtual machines it created: every
// one carries an ownership tag (this Controller's ID and the machine's
// own ID), and a Driver checks it before any operation on a machine -
// see Driver's own doc comment.
package hypervisor

import (
	"context"
	"errors"
	"io"
	"time"
)

// Kind is the type of hypervisor - which Driver talks to it.
type Kind string

const KindLibvirt Kind = "libvirt"

// ErrNotOwned is returned for a virtual machine that doesn't carry this
// Controller's ownership tag for the expected machine - another
// Controller's, or one nobody tagged. Nothing is done to it.
var ErrNotOwned = errors.New("this virtual machine wasn't created by this Controller")

// ErrNotFound is returned for a virtual machine that no longer exists.
var ErrNotFound = errors.New("no such virtual machine")

// Driver is one connection's worth of operations on a hypervisor.
// Every method that takes a MachineRef first checks the machine is this
// Controller's own (ErrNotOwned otherwise) - the checks live in each
// Driver, the one place that sees the hypervisor's own objects.
type Driver interface {
	// HostInfo describes the host: read-only, nothing about other
	// virtual machines beyond a count.
	HostInfo(ctx context.Context) (*HostInfo, error)
	// HasImage reports whether the base image is already on the
	// hypervisor (a previous machine uploaded it).
	HasImage(ctx context.Context, img Image) (bool, error)
	// UploadImage stores a base image from r (img.Size bytes, a qcow2
	// file). It only appears under img.Name once fully written; the
	// caller checks the content's hash while it streams and cancels
	// ctx on a mismatch.
	UploadImage(ctx context.Context, img Image, r io.Reader) error
	// Plan is the MachineRef CreateMachine will return, before it runs
	// (without the UUID): saved first, so a machine whose creation was
	// interrupted can still be found - by name, and its tag checked -
	// and removed.
	Plan(spec MachineSpec) MachineRef
	// CreateMachine creates the machine's disk from its base image and
	// its NoCloud volume, then defines and starts it, tagged as owned.
	CreateMachine(ctx context.Context, spec MachineSpec) (*MachineRef, error)
	MachineStatus(ctx context.Context, ref MachineRef) (*MachineStatus, error)
	Power(ctx context.Context, ref MachineRef, action PowerAction) error
	// Reconfigure changes a stopped machine's vCPUs, memory and network
	// interfaces - nics is the whole new set: an interface whose MAC
	// isn't in it is removed, one that's new is added, one whose network
	// changed is moved - from its next start.
	Reconfigure(ctx context.Context, ref MachineRef, vcpus, memoryMiB int, nics []NIC) error
	// Console copies the machine's serial console output to w until ctx
	// ends or the console closes. Read-only.
	Console(ctx context.Context, ref MachineRef, w io.Writer) error
	// DestroyMachine stops and removes the machine and the volumes
	// CreateMachine made for it. A machine already gone isn't an error.
	DestroyMachine(ctx context.Context, ref MachineRef) error
	Close() error
}

// HostInfo is a hypervisor host's state, as shown on its card.
type HostInfo struct {
	Hostname          string `json:"hostname"`
	HypervisorVersion string `json:"hypervisor_version"` // e.g. "QEMU 9.2.0"
	LibraryVersion    string `json:"library_version"`    // e.g. "libvirt 11.3.0"
	CPUModel          string `json:"cpu_model"`
	CPUs              int    `json:"cpus"`
	CPUMHz            int    `json:"cpu_mhz"`
	// Cumulative CPU time over all CPUs, nanoseconds: the busy share
	// between two samples is the CPU usage (computed by the caller).
	CPUBusyNs   uint64 `json:"-"`
	CPUTotalNs  uint64 `json:"-"`
	MemoryTotal uint64 `json:"memory_total"` // bytes
	MemoryFree  uint64 `json:"memory_free"`  // bytes
	// Storage is the pool new machines' disks go to.
	Storage StorageInfo `json:"storage"`
	// Networks are the ones machines may be attached to, as configured.
	Networks []NetworkInfo `json:"networks"`
	// RunningMachines counts the running virtual machines the
	// Controller's account may see - every one on the host, or only its
	// own under an access policy (docs/hypervisors.md); -1 when the
	// hypervisor won't say.
	RunningMachines int `json:"running_machines"`
}

type StorageInfo struct {
	Name      string `json:"name"`
	Active    bool   `json:"active"`
	Capacity  uint64 `json:"capacity"`
	Allocated uint64 `json:"allocated"`
	Available uint64 `json:"available"`
	Error     string `json:"error,omitempty"`
}

type NetworkInfo struct {
	Name   string `json:"name"`
	Active bool   `json:"active"`
	Error  string `json:"error,omitempty"`
}

// Image is a Janus disk image, cached on the hypervisor once per
// schematic and version and copied for each machine.
type Image struct {
	Name string // volume name, unique per schematic+version
	// For UploadImage: the file's size in bytes, and the disk size the
	// qcow2 file describes.
	Size        int64
	VirtualSize uint64
}

// MachineSpec is everything CreateMachine needs.
type MachineSpec struct {
	// MachineID is the Controller's own ID for the machine, recorded in
	// the ownership tag.
	MachineID string
	// Name is the virtual machine's name on the hypervisor - already
	// carrying the configured prefix.
	Name      string
	VCPUs     int
	MemoryMiB int
	Image     Image
	// CIData is the NoCloud volume's content (internal/cidata).
	CIData []byte
	NICs   []NIC
}

// NIC attaches the machine to one of the hypervisor's networks.
type NIC struct {
	Network string `json:"network"`
	MAC     string `json:"mac"`
}

// MachineRef is how the Controller finds a machine it created again:
// what CreateMachine returned, persisted with the machine.
type MachineRef struct {
	MachineID string `json:"machine_id"`
	// UUID is empty while the machine is being created (Plan): it's then
	// found by Name, its ownership tag checked all the same.
	UUID string `json:"uuid,omitempty"`
	Name string `json:"name"`
	// Volumes CreateMachine created, removed with the machine.
	Volumes []string `json:"volumes"`
}

// PowerState is a machine's state as the hypervisor sees it.
type PowerState string

const (
	PowerRunning  PowerState = "running"
	PowerOff      PowerState = "off"
	PowerPaused   PowerState = "paused"
	PowerCrashed  PowerState = "crashed"
	PowerStarting PowerState = "starting"
	PowerStopping PowerState = "stopping"
	PowerUnknown  PowerState = "unknown"
)

type MachineStatus struct {
	Power     PowerState `json:"power"`
	VCPUs     int        `json:"vcpus"`
	MemoryMiB int        `json:"memory_mib"`
	// CPUTimeNs is the machine's cumulative CPU time.
	CPUTimeNs uint64 `json:"cpu_time_ns"`
}

// PowerAction is what Power does - the hypervisor-level actions, for
// when the node itself doesn't answer. A clean reboot or shutdown goes
// through the node's own API.
type PowerAction string

const (
	PowerStart    PowerAction = "start"
	PowerForceOff PowerAction = "force-off"
	PowerReset    PowerAction = "reset"
)

// Valid reports whether a is one of the actions Power knows.
func (a PowerAction) Valid() bool {
	switch a {
	case PowerStart, PowerForceOff, PowerReset:
		return true
	}
	return false
}

// Timestamps used by Store.
var now = func() time.Time { return time.Now().UTC() }
