// Package diskseed writes node self-registration config
// (controller_address/controller_ca_cert - see internal/api/
// install.go's InstallRequest for the original fields) onto an
// ALREADY-BUILT disk's existing STATE partition, without touching
// partitioning or the rootfs at all.
//
// internal/api/install.go's writeControllerConfig already does this
// same write, but only as one step of LifecycleService.Install's full
// from-scratch build (partition table + both A/B rootfs slots + ESP) -
// which needs a live janusd process to serve the RPC at all. That's
// fine for provisioning one node at a time, but doesn't scale to
// standing up dozens of nodes from one shared, pre-built generic image
// (image/kvm-proxmox/assemble.sh's own qcow2, or its raw disk before
// conversion): every one of them would need its own native-janusd-plus-
// gRPC round trip just to change two small files.
//
// This package is the standalone, no-server-needed alternative:
// SeedController opens a disk that's already fully assembled - STATE
// already exists (blank) - and writes only controller/address and
// controller/ca.crt into it, mirroring the exact convention
// rootfs/init/main.go's mountState and cmd/janusd's
// selfRegisterIfConfigured already expect on a running node. Built for
// cmd/janusctl's "image seed-controller" subcommand (and later a
// companion-site equivalent, see docs/provisioning-a-node.md) - the
// Talos Image Factory precedent for this is its own "embedded machine
// configuration" option, which bakes config directly into a generated
// image rather than delivering it separately at boot.
//
// go-diskfs has no qcow2 support - this operates on a raw disk image
// only. A qcow2 (e.g. downloaded from a future companion site, or
// produced locally by `make proxmox-image`) needs `qemu-img convert -O
// raw`/`-O qcow2` round-tripped around a SeedController call - see
// docs/provisioning-a-node.md for the exact commands.
package diskseed

import (
	"fmt"
	"os"

	diskfs "github.com/diskfs/go-diskfs"
	diskpkg "github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/filesystem"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/netconfig"
)

// SeedController writes address/caCertPEM onto diskPath's existing
// STATE partition, found by its conventional GPT name (the same signal
// internal/api/install.go's refuseIfAlreadyInstalled already uses to
// recognize a Janus disk) rather than a hardcoded partition number -
// self-documenting, and doesn't need to assume the exact same six-
// partition layout internal/diskimage.Compute produces stays the only
// one ever built. Refuses a disk with no STATE partition at all (not a
// Janus image, or not built yet) and an empty address (nothing to
// write, matching writeControllerConfig's own "blank address writes
// nothing" behavior - but here that's a caller mistake worth surfacing
// rather than a silent no-op, since this function's entire purpose is
// writing this one thing).
// fleetRootPEM, optional, is the Controller's fleet root: the node then
// checks the Controller through its fleet first (InstallRequest's
// controller_fleet_root_cert).
func SeedController(diskPath, address string, caCertPEM, fleetRootPEM []byte) error {
	if address == "" {
		return fmt.Errorf("controller address is required")
	}
	if len(caCertPEM) == 0 {
		return fmt.Errorf("controller CA certificate is required - the node has to already know which CA to trust before it ever dials the Controller")
	}

	d, err := diskfs.Open(diskPath, diskfs.WithOpenMode(diskfs.ReadWrite))
	if err != nil {
		return fmt.Errorf("open %s: %w", diskPath, err)
	}

	partIndex, err := findStatePartition(d)
	if err != nil {
		return fmt.Errorf("%s: %w", diskPath, err)
	}

	fs, err := d.GetFilesystem(partIndex)
	if err != nil {
		return fmt.Errorf("open STATE filesystem: %w", err)
	}

	// Refuses an already-seeded disk outright, rather than overwriting -
	// same "refuse rather than silently redo" philosophy
	// refuseIfAlreadyInstalled already applies to a whole disk. Found to
	// be the right call the hard way, not by inspection: overwriting an
	// existing file here was tried first, and go-diskfs's ext4 driver
	// turned out to have two separate real bugs around it (OpenFile
	// ignoring O_TRUNC, and Remove-then-recreate silently losing the new
	// file entirely on a later read) - see this function's own git
	// history/PR for the exact symptoms. Re-seeding an already-seeded
	// image is a rare enough case that "start from a fresh image
	// instead" is a perfectly reasonable answer, and sidesteps both
	// bugs entirely rather than working around them.
	if _, err := fs.OpenFile("controller/address", os.O_RDONLY); err == nil {
		return fmt.Errorf("%s is already seeded with a controller config - re-run against a fresh, unseeded image instead of patching this one in place", diskPath)
	}

	// No leading slash: go-diskfs's ext4 driver runs Mkdir's argument
	// through Go's io/fs.ValidPath, which rejects one outright - the
	// same real bug install.go's writeControllerConfig already found
	// and worked around.
	if err := fs.Mkdir("controller"); err != nil {
		return fmt.Errorf("mkdir STATE controller/: %w", err)
	}
	if err := writeFSFile(fs, "controller/address", []byte(address)); err != nil {
		return err
	}
	if err := writeFSFile(fs, "controller/ca.crt", caCertPEM); err != nil {
		return err
	}
	if len(fleetRootPEM) > 0 {
		if err := writeFSFile(fs, "controller/fleet-root.crt", fleetRootPEM); err != nil {
			return err
		}
	}

	return nil
}

// SeedNetwork writes a network configuration onto an already-built
// disk's STATE partition (network/config.json - see
// internal/netconfig), offline, the same way SeedController writes a
// Controller: so a generic image can boot straight into, say, a static
// address on a network without DHCP. Refuses an invalid configuration,
// and a disk already seeded with one (same go-diskfs reasons as
// SeedController).
func SeedNetwork(diskPath string, cfg *janusv1alpha1.NetworkConfig) error {
	if err := netconfig.Validate(cfg); err != nil {
		return err
	}
	data, err := netconfig.Marshal(cfg)
	if err != nil {
		return err
	}
	d, err := diskfs.Open(diskPath, diskfs.WithOpenMode(diskfs.ReadWrite))
	if err != nil {
		return fmt.Errorf("open %s: %w", diskPath, err)
	}
	partIndex, err := findStatePartition(d)
	if err != nil {
		return fmt.Errorf("%s: %w", diskPath, err)
	}
	fs, err := d.GetFilesystem(partIndex)
	if err != nil {
		return fmt.Errorf("open STATE filesystem: %w", err)
	}
	path := "network/" + netconfig.FileName
	if _, err := fs.OpenFile(path, os.O_RDONLY); err == nil {
		return fmt.Errorf("%s is already seeded with a network config - re-run against a fresh, unseeded image instead of patching this one in place", diskPath)
	}
	if err := fs.Mkdir("network"); err != nil {
		return fmt.Errorf("mkdir STATE network/: %w", err)
	}
	return writeFSFile(fs, path, data)
}

func findStatePartition(d *diskpkg.Disk) (int, error) {
	if d.Table == nil {
		return 0, fmt.Errorf("no partition table found - not a Janus image")
	}
	for _, p := range d.Table.GetPartitions() {
		if p.Label() == "STATE" {
			return p.GetIndex(), nil
		}
	}
	return 0, fmt.Errorf("no STATE partition found - not a Janus image")
}

// writeFSFile mirrors internal/api/install.go's own helper of the same
// name exactly - always writes onto a path that SeedController has
// already confirmed doesn't exist yet (see its own "already seeded"
// check above), so it never needs to overwrite/truncate an existing
// file.
func writeFSFile(fs filesystem.FileSystem, path string, data []byte) error {
	f, err := fs.OpenFile(path, os.O_CREATE|os.O_RDWR)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
