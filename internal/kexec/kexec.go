// Package kexec loads a Unified Kernel Image's kernel into memory for a
// reboot that skips the firmware (reboot(LINUX_REBOOT_CMD_KEXEC)): the
// running kernel jumps straight into the new one - seconds, where a
// server's POST takes a minute.
//
// The kernel and its command line come out of the UKI's own PE
// sections (.linux, .cmdline, .initrd when there is one - what
// systemd-stub would hand the firmware-booted kernel), so what kexec
// boots is what the firmware would have booted: the same signed
// cmdline, the same verity root hash in it. Whether that UKI may be
// trusted is the caller's decision (internal/releasetrust, Secure
// Boot), before Load: the kernel verifies nothing here (no
// CONFIG_KEXEC_SIG - it could only check a signature on the bare
// bzImage, and the command line that pins the root filesystem isn't
// part of that).
package kexec

import (
	"bytes"
	"debug/pe"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/sys/unix"
)

// Parts are what a UKI hands the kernel: the bzImage, its command line,
// an initrd when the UKI has one (Janus's don't).
type Parts struct {
	Kernel  []byte
	Cmdline string
	Initrd  []byte
}

// Split reads a UKI's PE sections.
func Split(uki []byte) (*Parts, error) {
	f, err := pe.NewFile(bytes.NewReader(uki))
	if err != nil {
		return nil, fmt.Errorf("not a PE image: %w", err)
	}
	defer f.Close()
	section := func(name string) ([]byte, error) {
		sec := f.Section(name)
		if sec == nil {
			return nil, nil
		}
		data, err := sec.Data()
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		// The file holds the section padded to its alignment; the
		// image's own size is VirtualSize.
		if n := int(sec.VirtualSize); n > 0 && n < len(data) {
			data = data[:n]
		}
		return data, nil
	}
	kernel, err := section(".linux")
	if err != nil {
		return nil, err
	}
	if len(kernel) == 0 {
		return nil, errors.New("no .linux section: not a UKI")
	}
	cmdline, err := section(".cmdline")
	if err != nil {
		return nil, err
	}
	initrd, err := section(".initrd")
	if err != nil {
		return nil, err
	}
	return &Parts{Kernel: kernel, Cmdline: strings.TrimSpace(strings.TrimRight(string(cmdline), "\x00")), Initrd: initrd}, nil
}

// Load stages uki's kernel for the next reboot(LINUX_REBOOT_CMD_KEXEC)
// (kexec_file_load). ErrUnsupported when this kernel has no
// kexec_file_load.
func Load(uki []byte) error {
	parts, err := Split(uki)
	if err != nil {
		return err
	}
	kernelFd, err := memfd(".linux", parts.Kernel)
	if err != nil {
		return err
	}
	defer unix.Close(kernelFd)
	initrd := parts.Initrd
	if len(initrd) == 0 {
		initrd = emptyInitramfs()
	}
	initrdFd, err := memfd(".initrd", initrd)
	if err != nil {
		return err
	}
	defer unix.Close(initrdFd)
	if err := unix.KexecFileLoad(kernelFd, initrdFd, parts.Cmdline, 0); err != nil {
		if errors.Is(err, unix.ENOSYS) {
			return fmt.Errorf("%w: this kernel has no kexec_file_load", ErrUnsupported)
		}
		return fmt.Errorf("kexec_file_load: %w", err)
	}
	return nil
}

// emptyInitramfs is a cpio archive (newc) holding nothing but its
// trailer: the initrd handed to kexec when the UKI has none. Never no
// initrd at all: a kernel booted by the firmware through systemd-stub
// took a small initrd from an EFI configuration table (INITRD=... in
// its log), and a kernel kexec'd from it finds that same table again -
// pointing into memory the first kernel has long reused (EFI loader
// data is ordinary RAM to x86), so it reads a ramdisk of a garbage
// size and panics ("Cannot find place for new RAMDISK"). The kernel
// only consults that table when the boot parameters name no ramdisk;
// an empty one makes them name this, which unpacks to nothing.
func emptyInitramfs() []byte {
	const name = "TRAILER!!!\x00"
	hdr := fmt.Sprintf("070701%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x",
		0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, len(name), 0)
	entry := []byte(hdr + name)
	for len(entry)%4 != 0 {
		entry = append(entry, 0)
	}
	// Padded to the archive's block size, as cpio writes it.
	return append(entry, make([]byte, 512-len(entry)%512)...)
}

// ErrUnsupported: the running kernel can't kexec.
var ErrUnsupported = errors.New("kexec unsupported")

// memfd holds data in an anonymous memory file, what kexec_file_load
// reads: nothing is written to disk.
func memfd(name string, data []byte) (int, error) {
	fd, err := unix.MemfdCreate("kexec"+name, unix.MFD_CLOEXEC)
	if err != nil {
		return -1, fmt.Errorf("memfd_create: %w", err)
	}
	for off := 0; off < len(data); {
		n, err := unix.Pwrite(fd, data[off:], int64(off))
		if err != nil {
			unix.Close(fd)
			return -1, fmt.Errorf("write %s: %w", name, err)
		}
		off += n
	}
	return fd, nil
}
