// Package hwsupport says whether an image's kernel drives the machine a
// node runs on: the drivers its boot disk and network interfaces are
// bound to now, read from sysfs, against the drivers the image says its
// kernel builds in (image.json's kernel.drivers, internal/schematic).
//
// An update whose kernel lacks the boot disk's driver never reaches init:
// the kernel waits for its root filesystem forever (dm-mod.waitfor), so
// nothing on the node can roll it back - only someone at its console. One
// that lacks a network interface's driver leaves the node unreachable
// there. Upgrade and Install refuse both, before the node boots it.
package hwsupport

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Use is a driver a device the node needs is bound to.
type Use struct {
	// Driver is the sysfs driver's name: "virtio_blk", "nvme", "e1000e".
	Driver string
	// Device is what needs it: "boot disk vda", "eth0".
	Device string
}

func (u Use) String() string { return u.Driver + " (" + u.Device + ")" }

// InUse returns the drivers on the sysfs paths of disk (a device name or
// path: "vda", "/dev/nvme0n1") and of every network interface backed by a
// device - not lo, bridges, VLANs or other virtual interfaces - from the
// device up to the bus: a disk on virtio-scsi needs sd, virtio_scsi and
// virtio-pci. sysfs is "/sys", or a tree laid out like it. disk "" leaves
// the disk out.
func InUse(sysfs, disk string) ([]Use, error) {
	var out []Use
	if disk != "" {
		name := filepath.Base(disk)
		drivers, err := pathDrivers(sysfs, filepath.Join(sysfs, "block", name))
		if err != nil {
			return nil, fmt.Errorf("disk %s: %w", name, err)
		}
		if len(drivers) == 0 {
			return nil, fmt.Errorf("disk %s: no driver found in %s", name, filepath.Join(sysfs, "block", name))
		}
		for _, d := range drivers {
			out = append(out, Use{Driver: d, Device: "boot disk " + name})
		}
	}
	nets, err := os.ReadDir(filepath.Join(sysfs, "class", "net"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, n := range nets {
		dev := filepath.Join(sysfs, "class", "net", n.Name(), "device")
		if _, err := os.Stat(dev); err != nil {
			continue // virtual: no device behind it
		}
		drivers, err := pathDrivers(sysfs, dev)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", n.Name(), err)
		}
		for _, d := range drivers {
			out = append(out, Use{Driver: d, Device: n.Name()})
		}
	}
	return out, nil
}

// pathDrivers walks from link's target up to sysfs's devices/ root and
// returns the drivers bound along the way, nearest first, once each.
func pathDrivers(sysfs, link string) ([]string, error) {
	root, err := filepath.EvalSymlinks(filepath.Join(sysfs, "devices"))
	if err != nil {
		return nil, err
	}
	dir, err := filepath.EvalSymlinks(link)
	if err != nil {
		return nil, err
	}
	var out []string
	for dir != root && strings.HasPrefix(dir, root+string(filepath.Separator)) {
		if target, err := os.Readlink(filepath.Join(dir, "driver")); err == nil {
			if d := filepath.Base(target); !slices.Contains(out, d) {
				out = append(out, d)
			}
		}
		dir = filepath.Dir(dir)
	}
	return out, nil
}

// Missing returns the uses whose driver isn't among drivers. No drivers:
// the image doesn't say (an older image, or a kernel for any machine) -
// nothing is missing.
func Missing(drivers []string, uses []Use) []Use {
	if len(drivers) == 0 {
		return nil
	}
	var out []Use
	for _, u := range uses {
		if !slices.Contains(drivers, u.Driver) {
			out = append(out, u)
		}
	}
	return out
}

// Describe lists uses as "nvme (boot disk nvme0n1), igb (eth0)".
func Describe(uses []Use) string {
	s := make([]string, len(uses))
	for i, u := range uses {
		s[i] = u.String()
	}
	return strings.Join(s, ", ")
}

// Summary lists uses by device, in their order: "boot disk vda:
// virtio_blk, virtio-pci; eth0: virtio_net, virtio-pci".
func Summary(uses []Use) string {
	var parts []string
	prev := ""
	for _, u := range uses {
		if len(parts) == 0 || u.Device != prev {
			parts = append(parts, u.Device+": "+u.Driver)
		} else {
			parts[len(parts)-1] += ", " + u.Driver
		}
		prev = u.Device
	}
	if len(parts) == 0 {
		return "no disk or network interface with a driver"
	}
	return strings.Join(parts, "; ")
}
