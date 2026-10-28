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
	// Reconfigure changes a stopped machine's hardware - its size, what
	// it may grow to while it runs (hw.MaxVCPUs, hw.MaxMemoryMiB), and
	// its network interfaces: nics is the whole new set - an interface
	// whose MAC isn't in it is removed, one that's new is added, one
	// whose network changed is moved - from its next start. It also
	// sets the settings MachineStatus reports as Drift: s's (the guest
	// agent), and what CreateMachine gives every machine today (the CPU
	// model, where an older Controller left the hypervisor's default).
	Reconfigure(ctx context.Context, ref MachineRef, hw Hardware, nics []NIC, s Settings) error
	// Resize changes a running scalable machine's vCPUs and memory, within
	// the maxima it was started with, without stopping it (hot-plug: the
	// Controller's auto-scaling). It returns once the hypervisor took the
	// request; the node onlines what it gets.
	Resize(ctx context.Context, ref MachineRef, vcpus, memoryMiB int) error
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
	Hostname string `json:"hostname"`
	// HostID is the physical host's identity, for anti-affinity: two
	// hypervisor records reaching the same host share it (libvirt: the
	// host's UUID; Proxmox: the node's name). Empty: unknown.
	HostID            string `json:"host_id,omitempty"`
	HypervisorVersion string `json:"hypervisor_version"` // e.g. "QEMU 9.2.0"
	LibraryVersion    string `json:"library_version"`    // e.g. "libvirt 11.3.0"
	CPUModel          string `json:"cpu_model"`
	CPUs              int    `json:"cpus"`
	CPUMHz            int    `json:"cpu_mhz"`
	// Cumulative CPU time over all CPUs, nanoseconds: the busy share
	// between two samples is the CPU usage (computed by the caller).
	CPUBusyNs  uint64 `json:"-"`
	CPUTotalNs uint64 `json:"-"`
	// CPUUsage, when the hypervisor gives it (Proxmox), is the share of
	// CPU busy right now, 0 to 1 - instead of the cumulative times.
	CPUUsage    *float64 `json:"-"`
	MemoryTotal uint64   `json:"memory_total"` // bytes
	MemoryFree  uint64   `json:"memory_free"`  // bytes
	// Storage is the pool new machines' disks go to.
	Storage StorageInfo `json:"storage"`
	// ImageStorage, when the hypervisor keeps the base images apart
	// (Proxmox), is where they go - its Error says why it can't take them.
	ImageStorage *StorageInfo `json:"image_storage,omitempty"`
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

// Hardware is a machine's size - vCPUs and memory - and what it may grow
// to while it runs.
type Hardware struct {
	VCPUs     int
	MemoryMiB int
	// MaxVCPUs and MaxMemoryMiB, above VCPUs and MemoryMiB, make the
	// machine scalable: started with room for them, Resize grows it to
	// them and back without a restart. Zero or equal: fixed.
	MaxVCPUs     int
	MaxMemoryMiB int
}

// ScalableCPUs and ScalableMemory report whether the machine may grow
// its vCPUs, its memory, while it runs.
func (h Hardware) ScalableCPUs() bool   { return h.MaxVCPUs > h.VCPUs }
func (h Hardware) ScalableMemory() bool { return h.MaxMemoryMiB > h.MemoryMiB }

// Scalable reports whether either may grow.
func (h Hardware) Scalable() bool { return h.ScalableCPUs() || h.ScalableMemory() }

// Settings are what a machine's image asks of its hypervisor beyond its
// size and interfaces.
type Settings struct {
	// GuestAgent: the image runs the QEMU guest agent (the
	// qemu-guest-agent extension). A hypervisor with an option for it
	// turns it on (Proxmox VE's "Use QEMU Guest Agent"): on without an
	// agent in the guest, its shutdowns and backups wait for one and time
	// out. libvirt's channel is always there, harmless without one.
	GuestAgent bool
}

// GuestAgentExtension is the extension that runs the QEMU guest agent.
const GuestAgentExtension = "qemu-guest-agent"

// MachineSpec is everything CreateMachine needs.
type MachineSpec struct {
	// MachineID is the Controller's own ID for the machine, recorded in
	// the ownership tag.
	MachineID string
	// Name is the virtual machine's name on the hypervisor - already
	// carrying the configured prefix.
	Name string
	// Hardware: the size it starts with, and its maxima.
	Hardware
	Image Image
	// CIData is the NoCloud volume's content (internal/cidata).
	CIData []byte
	NICs   []NIC
	Settings
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
	Power PowerState `json:"power"`
	// VCPUs and MemoryMiB are what the machine has now - what it was
	// resized to, for a scalable one.
	VCPUs     int `json:"vcpus"`
	MemoryMiB int `json:"memory_mib"`
	// MaxVCPUs and MaxMemoryMiB are the most it can have without a
	// restart - VCPUs and MemoryMiB for a fixed machine. MaxMemoryMiB is
	// 0 when the hypervisor sets no ceiling of its own (Proxmox: its
	// memory modules go as far as the CPU addresses).
	MaxVCPUs     int `json:"max_vcpus"`
	MaxMemoryMiB int `json:"max_memory_mib"`
	// MemoryStepMiB is what Resize changes the memory by - 0 when it
	// can't change while the machine runs.
	MemoryStepMiB int `json:"memory_step_mib,omitempty"`
	// CPUTimeNs is the machine's cumulative CPU time.
	CPUTimeNs uint64 `json:"cpu_time_ns"`
	// HostID and HostName are the host the machine runs on now - a
	// Proxmox HA migration moves it without asking (HostInfo.HostID).
	HostID   string `json:"host_id,omitempty"`
	HostName string `json:"host_name,omitempty"`
	// Drift: the settings CreateMachine gives every machine today that
	// this one hasn't got, whatever its image - one an older Controller
	// created, or changed on the hypervisor (Proxmox: a CPU model without
	// AES-NI, #372). Reconfigure sets them.
	Drift []Drift `json:"drift,omitempty"`
	// GuestAgent, for a hypervisor with an option for the guest agent
	// (Proxmox VE), is whether the machine runs with it - its
	// virtio-serial port there; nil when there's no such option (libvirt:
	// the channel is always there). The Controller compares it with the
	// image's Settings.GuestAgent.
	GuestAgent *bool `json:"guest_agent,omitempty"`
}

// Drift is one of a machine's hypervisor settings that isn't what the
// Controller gives it today: what it has, what it would get.
type Drift struct {
	// Setting names it: "cpu", "guest-agent".
	Setting string `json:"setting"`
	Have    string `json:"have"`
	Want    string `json:"want"`
}

// String is the drift as a machine's history says it.
func (d Drift) String() string {
	return d.Setting + " " + d.Have + " → " + d.Want
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

// PrepStep is one step of preparing a host for the Controller: what to
// run there as root, and why.
type PrepStep struct {
	Title  string `json:"title"`
	About  string `json:"about"`
	Script string `json:"script"`
}
