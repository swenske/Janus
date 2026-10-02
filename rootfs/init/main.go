// Command init is Janus's PID 1. It mounts proc/sysfs/devtmpfs,
// sets up an ephemeral tmpfs writable layer (see mountEphemeral) and the
// persistent STATE partition (see mountState), prints a fixed success
// marker that hack/qemu-run.sh greps for, then:
//   - if /sbin/janusd is present in the initramfs, starts it under a
//     Supervisor (see supervisor.go) that restarts it - with a growing,
//     capped backoff - every time it exits, forever, on an ordinary
//     boot. There's no give-up threshold: janusd is the only way to
//     reach the node at all (see docs/architecture.md's "no shell"
//     design), so a node that stops retrying after N crashes would be
//     permanently unmanageable with no fallback - unlike systemd's
//     default, which can still fall back to SSH. The *one* exception is
//     a boot with a pending LifecycleService.Upgrade(wait_for_health=true)
//     confirmation marker still outstanding (see checkBootCommit and
//     internal/bootcommit): there, restarting forever would just leave
//     an unconfirmed, unhealthy slot unreachable forever too, so
//     Supervisor is bounded and gives up in favor of reverting to the
//     previously-known-good slot instead - a different action from
//     "giving up" on reaching the node at all.
//   - otherwise, powers off cleanly after a short delay - the Phase 1
//     boot-proof shape, so `make qemu-boot-test` (init alone, no
//     janusd packaged in) keeps working unchanged.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/swenske/Janus/internal/bootcommit"
	"github.com/swenske/Janus/internal/bootrevert"
	"github.com/swenske/Janus/internal/bootslot"
	"github.com/swenske/Janus/internal/consoledrain"
	"github.com/swenske/Janus/internal/netconfig"
	"github.com/swenske/Janus/internal/nocloud"
)

const daemonPath = "/sbin/janusd"

func mount(source, target, fstype string) {
	mountData(source, target, fstype, "")
}

// mountData is mount() with an explicit mount-options string (the
// syscall's own "data" argument) - only mountState needs this, to force
// a fixed SELinux context via the "context=" mount option (below).
func mountData(source, target, fstype, data string) {
	if err := os.MkdirAll(target, 0o755); err != nil {
		fmt.Printf("init: mkdir %s: %v\n", target, err)
		return
	}
	if err := syscall.Mount(source, target, fstype, 0, data); err != nil {
		fmt.Printf("init: mount %s on %s: %v\n", fstype, target, err)
	}
}

// mountEphemeral gives the otherwise fully read-only (Phase 3: dm-verity
// verified) root a writable layer, entirely tmpfs-backed - nothing here
// survives a reboot (see mountState for the one directory that does).
// /run and /tmp are mounted empty - nothing pre-existing there needs to
// survive the overmount (janusd's /run/janus, HAProxy's stats
// socket/pid file, Manager.Validate's tmpfile). /etc needs its own
// handling since it isn't empty on a freshly-booted node: rootfs/base/
// etc/haproxy/haproxy.cfg (the bootstrap default config) lives on the
// squashfs and would otherwise vanish under a plain tmpfs mount, so its
// bytes are read *before* the overmount and rewritten into the new
// tmpfs - this is what makes both PKI bootstrap (/etc/janus/pki,
// see mountState) and a live ApplyConfig RPC (which writes to this same
// path) actually work on a dm-verity-booted node; before this,
// janusd crash-looped forever on "read-only file system" trying to
// create either. /var is left alone (still squashfs-backed): the only
// thing under it is /var/empty, HAProxy's chroot jail, which must keep
// the exact immutable mode-0000 baked into the image by rootfs/
// assemble.sh, not a fresh writable one recreated here.
func mountEphemeral() {
	mount("tmpfs", "/run", "tmpfs")
	mount("tmpfs", "/tmp", "tmpfs")

	type seedFile struct {
		path string
		data []byte
		mode os.FileMode
	}
	var seeds []seedFile
	for _, path := range etcSeedFiles {
		data, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) || !etcOptionalFiles[path] {
				fmt.Printf("init: read %s before /etc overlay: %v\n", path, err)
			}
			continue
		}
		mode := os.FileMode(0o644)
		if info, err := os.Stat(path); err == nil {
			mode = info.Mode().Perm()
		}
		seeds = append(seeds, seedFile{path, data, mode})
	}

	mount("tmpfs", "/etc", "tmpfs")

	for _, f := range seeds {
		if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
			fmt.Printf("init: mkdir %s: %v\n", filepath.Dir(f.path), err)
			continue
		}
		if err := os.WriteFile(f.path, f.data, f.mode); err != nil {
			fmt.Printf("init: write %s: %v\n", f.path, err)
		}
	}
}

// etcSeedFiles are the squashfs-backed files under /etc that must stay
// visible after mountEphemeral's tmpfs overmount. The CA bundle was
// silently hidden by that overmount on every real boot until a real
// https:// Upgrade on a deployed node failed with "certificate signed by
// unknown authority" - every earlier URL-fetch test used plain http://.
var etcSeedFiles = []string{
	"/etc/haproxy/haproxy.cfg",
	"/etc/ssl/certs/ca-certificates.crt",
	"/etc/services", // from the nftables extension: service names in rulesets
}

// etcOptionalFiles are seed files an image may not have.
var etcOptionalFiles = map[string]bool{"/etc/services": true}

// mountState mounts the pre-formatted, persistent STATE partition (see
// rootfs/state-image.sh) once at /mnt/state, then bind-mounts its pki/
// and haproxy/ subdirectories over /etc/janus/pki and /etc/haproxy
// respectively - the two things mountEphemeral's tmpfs overlay can't be
// allowed to wipe every boot: PKI (or a fresh CA/admin cert generates
// every boot) and applied config (or a live ApplyConfig RPC - which
// just writes straight to /etc/haproxy/haproxy.cfg, see
// internal/haproxy.Manager.Apply - is lost the moment the node
// restarts). Unlike the root device, this one is writable, not
// dm-verity-protected, since it's meant to be written to and
// losing/corrupting it only costs state, not the system's integrity.
//
// Where to find it depends on which of two shapes actually booted -
// resolveStateDevice tells them apart by parsing /proc/cmdline's own
// dm-mod.create= parameter (see internal/bootslot, shared with
// internal/api/lifecycle.go's LifecycleService.Rollback), not a
// separate flag or convention of its own:
//   - the real, single GPT disk (image/disk/assemble.sh, booted by
//     hack/qemu-ab-boot-test.sh): root's data device is a partition
//     (e.g. /dev/vda2), and STATE is always partition 6 on that same
//     disk, by image/disk/assemble.sh's own fixed layout.
//   - the older separate-virtio-blk-drives harness
//     (hack/qemu-verity-boot-test.sh, hack/qemu-state-persist-test.sh),
//     which deliberately keeps working unchanged since it still covers
//     things the single-disk test doesn't (dm-verity tamper detection,
//     STATE persistence in isolation): root's data device is a whole
//     disk (e.g. /dev/vda, no partition table at all), and STATE is a
//     separate, fixed /dev/vdc drive.
//
// Not present at all in Phase 1/2's initramfs boots, or a verity boot
// without the drive mountState resolves to actually attached - mount()
// fails harmlessly there (see its own doc comment), leaving both
// directories on the ephemeral tmpfs instead, same fallback behavior
// as before this existed.
func mountState() {
	// Has to live inside the already-writable tmpfs /etc (mountEphemeral
	// runs first, see main()), not some fresh top-level path like
	// /mnt/state: root itself is still the dm-verity-verified, read-only
	// squashfs, so os.MkdirAll on a path that doesn't already exist
	// there fails outright - caught by a real boot regenerating a new
	// CA every time despite this function running, because every step
	// past that failed MkdirAll silently no-op'd (mount()/bindMount()
	// only log and return on error, never abort the boot).
	const stateRoot = "/etc/.state"
	// Phase 4 cont'd (SELinux): "context=" forces every file on this
	// mount to a single fixed SELinux context, ignoring any per-inode
	// xattr entirely - the same role genfscon plays for a
	// non-xattr-capable filesystem like the ESP's FAT, used here
	// instead of relying on selinux/policy.conf's own `fs_use_xattr
	// ext4 ... state_t` statement's fallback behavior. A real boot
	// caught why that fallback doesn't do what it sounds like it
	// should: an inode with no security.selinux xattr set (every inode
	// on this partition - rootfs/state-image.sh's mkfs.ext4 never sets
	// one) doesn't fall back to the fs_use_xattr statement's own
	// context at all, it falls back to the "file" initial SID
	// (SECINITSID_FILE, mapped to squashfs_t in this policy) instead -
	// which only happened to look like it worked for the read-only
	// rootfs because that mapping coincidentally already IS squashfs_t
	// there. On STATE it meant every write to a freshly-formatted
	// partition's root directory - including the very first
	// os.MkdirAll("/etc/.state/pki") - was denied under enforcing=1 as
	// squashfs_t, not state_t, "avc: denied { write } ... tcontext=
	// ...squashfs_t ... permissive=0". "context=" sidesteps the whole
	// xattr/fallback question rather than working around it.
	mountData(resolveStateDevice(), stateRoot, "ext4", "context=system_u:object_r:state_t")

	// pki/: created with 0700 directly (matching
	// internal/pki.LoadOrBootstrap's own intent) rather than mounting
	// the STATE device straight at /etc/janus/pki and chmod-ing
	// afterward - a bind mount's target shows the *source* directory's
	// mode, so controlling it here is enough, no separate chmod needed.
	pkiDir := filepath.Join(stateRoot, "pki")
	if err := os.MkdirAll(pkiDir, 0o700); err != nil {
		fmt.Printf("init: mkdir %s: %v\n", pkiDir, err)
	} else {
		bindMount(pkiDir, "/etc/janus/pki")
	}

	// haproxy/: needs the same first-boot seeding problem mountEphemeral
	// already solved for /etc itself - a blank STATE partition's
	// haproxy/ subdirectory has no haproxy.cfg yet, and bind-mounting it
	// over /etc/haproxy as-is would hide the bootstrap default
	// mountEphemeral just put there, leaving HAProxy nothing to start
	// from. So the bootstrap bytes are copied in *only if this is still
	// empty* - once a real ApplyConfig has written something there, a
	// later boot must never overwrite it back to the bootstrap default.
	haproxyDir := filepath.Join(stateRoot, "haproxy")
	seedPersistentHaproxyCfg(haproxyDir)
	bindMount(haproxyDir, "/etc/haproxy")

	// boot/: internal/bootcommit's pending-confirmation marker for a
	// wait_for_health LifecycleService.Upgrade - needs no first-boot
	// seeding trick like haproxy/ does, since bootcommit.Read treats a
	// missing marker file as simply "nothing pending", not an error.
	bootDir := filepath.Join(stateRoot, "boot")
	if err := os.MkdirAll(bootDir, 0o755); err != nil {
		fmt.Printf("init: mkdir %s: %v\n", bootDir, err)
	} else {
		bindMount(bootDir, bootcommit.Dir)
	}

	// controller/: Point 2 suite tranche 5 - node self-registration
	// config (address/ca.crt), written by LifecycleService.Install onto
	// STATE at provisioning time (internal/api/install.go's
	// writeControllerConfig), or left entirely absent for the
	// overwhelming majority of nodes with no Controller at all. Needs
	// no first-boot seeding trick like haproxy/ does - either Install
	// already wrote content here or it didn't, and either way there's
	// nothing this boot needs to create beyond the directory itself.
	// Consumed by cmd/janusd's own internal/selfregister, not by
	// rootfs/init - bind-mounted here (a literal path, not an import of
	// that package, matching how "/etc/janus/pki"/"/etc/haproxy" above
	// are also literals) purely so STATE-backed content survives the
	// same tmpfs /etc overmount everything else under /etc/janus does.
	ctrlDir := filepath.Join(stateRoot, "controller")
	if err := os.MkdirAll(ctrlDir, 0o755); err != nil {
		fmt.Printf("init: mkdir %s: %v\n", ctrlDir, err)
	} else {
		bindMount(ctrlDir, "/etc/janus/controller")
	}

	// network/: the node's confirmed network configuration
	// (internal/netconfig.Dir - a literal here too), written by janusd on
	// NetworkConfigConfirm, or at provisioning time (Install, `janusctl
	// image seed`, NoCloud). Absent: the defaults (kernel boot DHCP).
	netDir := filepath.Join(stateRoot, "network")
	if err := os.MkdirAll(netDir, 0o755); err != nil {
		fmt.Printf("init: mkdir %s: %v\n", netDir, err)
	} else {
		bindMount(netDir, "/etc/janus/network")
	}

	// config/: janusd's other persistent settings - the exporter's
	// (internal/exporter.Dir), the optional modules' configurations.
	// Absent files mean the defaults.
	cfgDir := filepath.Join(stateRoot, "config")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		fmt.Printf("init: mkdir %s: %v\n", cfgDir, err)
	} else {
		bindMount(cfgDir, "/etc/janus/config")
	}
}

func seedPersistentHaproxyCfg(dir string) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Printf("init: mkdir %s: %v\n", dir, err)
		return
	}

	const cfgName = "haproxy.cfg"
	dst := filepath.Join(dir, cfgName)
	if _, err := os.Stat(dst); err == nil {
		return // already has a persisted config (bootstrap or applied) - never overwrite it
	}

	// Still the mountEphemeral-seeded bootstrap default at this point -
	// mountState always runs after mountEphemeral (see main()).
	src := "/etc/haproxy/" + cfgName
	cfgBytes, err := os.ReadFile(src)
	if err != nil {
		fmt.Printf("init: read %s to seed persistent state: %v\n", src, err)
		return
	}
	mode := os.FileMode(0o644)
	if info, err := os.Stat(src); err == nil {
		mode = info.Mode().Perm()
	}
	if err := os.WriteFile(dst, cfgBytes, mode); err != nil {
		fmt.Printf("init: write %s: %v\n", dst, err)
	}
}

// seedFromNoCloud is the external, delivered-at-boot complement to
// internal/diskseed's embedded-at-image-generation-time approach (see
// internal/nocloud's own package doc for the full reasoning and the
// Talos precedent behind both) - a locally-attached volume labeled
// "cidata"/"CIDATA" can carry a Controller (controller_address/
// controller_ca_cert) and a network configuration for a shared, generic
// image that was never seeded at all. Runs after mountState so
// /etc/janus/controller and /etc/janus/network are already the real,
// writable, bind-mounted STATE directories - plain file writes here,
// unlike internal/diskseed's own go-diskfs dance, since this is a
// genuinely mounted filesystem now, not a raw disk file being patched
// cold. Before janusd starts, so the network configuration applies from
// this very boot.
//
// Never overwrites what's already provisioned (by Install, diskseed, an
// earlier NoCloud boot, or - for the network - a confirmed
// NetworkConfigApply): each part is only written if absent, the same
// "don't clobber what's already there" philosophy the other paths apply
// on the writing side. Scans whole-disk devices
// (internal/nocloud.ScanBlockDevices), excluding this node's own boot
// disk (internal/bootslot.WholeDisk off /proc/cmdline, same resolution
// resolveStateDevice already trusts) - non-fatal at every step, same
// tolerant pattern as everything else in this file: the overwhelming
// majority of boots have no such volume attached at all, and that's not
// an error.
func seedFromNoCloud() {
	const addressPath = "/etc/janus/controller/address"
	_, err := os.Stat(addressPath)
	haveController := err == nil
	haveNetwork := netconfig.Exists()
	if haveController && haveNetwork {
		return
	}

	var bootDisk string
	if cmdline, err := os.ReadFile("/proc/cmdline"); err == nil {
		if dataDev, ok := bootslot.DataDevice(string(cmdline)); ok {
			bootDisk, _ = bootslot.WholeDisk(dataDev)
		}
	}

	candidates, err := nocloud.ScanBlockDevices(bootDisk)
	if err != nil {
		fmt.Printf("init: nocloud: scan block devices: %v\n", err)
		return
	}
	volume, err := nocloud.FindVolume(candidates)
	if err != nil {
		if err != nocloud.ErrNotFound {
			fmt.Printf("init: nocloud: find volume: %v\n", err)
		}
		return
	}
	cfg, err := nocloud.Read(volume)
	if err != nil {
		fmt.Printf("init: nocloud: read %s: %v\n", volume, err)
		return
	}

	if !haveNetwork && cfg.Network != nil {
		if err := netconfig.Save(cfg.Network); err != nil {
			fmt.Printf("init: nocloud: save the network configuration: %v\n", err)
		} else {
			fmt.Printf("init: nocloud: seeded network config from %s\n", volume)
		}
	}

	if haveController || cfg.ControllerAddress == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(addressPath), 0o755); err != nil {
		fmt.Printf("init: nocloud: mkdir %s: %v\n", filepath.Dir(addressPath), err)
		return
	}
	if err := os.WriteFile(addressPath, []byte(cfg.ControllerAddress), 0o644); err != nil {
		fmt.Printf("init: nocloud: write %s: %v\n", addressPath, err)
		return
	}
	if err := os.WriteFile("/etc/janus/controller/ca.crt", cfg.ControllerCACert, 0o644); err != nil {
		fmt.Printf("init: nocloud: write /etc/janus/controller/ca.crt: %v\n", err)
		return
	}
	fmt.Printf("init: nocloud: seeded controller config from %s\n", volume)
}

// resolveStateDevice figures out where the STATE partition/drive
// actually is for *this* boot - see mountState's doc comment for the
// two shapes it distinguishes between. Never fails outright: any
// read/parse problem just falls back to the older separate-drive
// convention, same as if this function didn't exist at all.
func resolveStateDevice() string {
	const fallback = "/dev/vdc" // hack/qemu-verity-boot-test.sh / hack/qemu-state-persist-test.sh's separate-drives harness
	cmdline, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		fmt.Printf("init: read /proc/cmdline: %v\n", err)
		return fallback
	}
	dataDev, ok := bootslot.DataDevice(string(cmdline))
	if !ok {
		return fallback
	}
	device, ok := bootslot.StateDevice(dataDev)
	if !ok {
		return fallback
	}
	return device
}

// mountReleaseBundle mounts image/iso/assemble.sh's embedded release
// bundle - present only on the Bare-metal Machine installer/
// maintenance-mode ISO, always partition 1 on that image's own fixed
// layout (1:ISO9660 release bundle, 2:ESP, 3:DATA, 4:HASH - a
// different convention from image/disk/assemble.sh's, see that
// script's own header) - read-only, at a fixed path janusd's own
// LifecycleService.Install can be pointed at directly as BUNDLE_DIR.
// Same tolerant pattern as mountState: any other image shape either
// has no partition 1 at all, or (image/disk's own ESP, a FAT
// filesystem) fails the "iso9660" mount outright - both non-fatal,
// falling back to simply not having this path populated, exactly as
// if this function didn't exist.
func mountReleaseBundle() {
	cmdline, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		fmt.Printf("init: release bundle: read /proc/cmdline: %v\n", err)
		return
	}
	dataDev, ok := bootslot.DataDevice(string(cmdline))
	if !ok {
		return
	}
	disk, ok := bootslot.Disk(dataDev)
	if !ok {
		return
	}
	const target = "/etc/janus/release"
	if err := os.MkdirAll(target, 0o755); err != nil {
		fmt.Printf("init: mkdir %s: %v\n", target, err)
		return
	}
	// Unlike mount()/mountData()'s other callers, iso9660 refuses an
	// implicit read-write mount outright (EACCES) - it never supports
	// writing at all, so MS_RDONLY has to be passed explicitly rather
	// than left to the filesystem driver to assume.
	if err := syscall.Mount(disk+"1", target, "iso9660", syscall.MS_RDONLY, ""); err != nil {
		fmt.Printf("init: mount iso9660 on %s: %v\n", target, err)
	}
}

func bindMount(src, dst string) {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		fmt.Printf("init: mkdir %s: %v\n", dst, err)
		return
	}
	if err := syscall.Mount(src, dst, "", syscall.MS_BIND, ""); err != nil {
		fmt.Printf("init: bind mount %s on %s: %v\n", src, dst, err)
	}
}

// hardenSysctls applies the runtime half of Phase 4's kernel hardening
// pass - the half that can't be baked into kernel/configs/
// janus_defconfig at build time (a tunable *value*, not a feature
// being compiled in or out at all) and has to be written to /proc/sys
// at boot instead, since there's no sysctl(8)/procps binary, and no
// /etc/sysctl.d for one to read anyway, on this rootfs. Runs right
// after mount("proc", ...) - nothing else here needs anything more than
// that.
//
// Each entry is independent and non-fatal on its own: a kernel built
// without some feature (CONFIG_SECURITY_YAMA, say) simply won't have
// the matching /proc/sys node at all, and that one write logs and moves
// on rather than aborting the boot - the same tolerant pattern mount()
// itself already uses for a missing STATE drive. Logged either way
// (success or failure) specifically so a real boot's console output is
// enough to verify every value actually took effect, not just that
// this function ran without panicking.
func hardenSysctls() {
	sysctls := map[string]string{
		// Kernel self-protection: hide the ring buffer and kernel
		// pointers from anything without CAP_SYSLOG/CAP_SYSLOG-adjacent
		// privilege - defense in depth against a compromised janusd
		// or haproxy (uid 1000, no such capability) trying to defeat
		// KASLR via an info leak.
		"/proc/sys/kernel/dmesg_restrict": "1",
		"/proc/sys/kernel/kptr_restrict":  "2",
		// Yama (kernel/configs/janus_defconfig's own CONFIG_SECURITY_YAMA):
		// 2 ("admin-only") means only a process with CAP_SYS_PTRACE can
		// ptrace another - janusd (root) still can, but the
		// unprivileged haproxy worker (chroot + uid 1000, no such
		// capability) can no longer ptrace anything at all, including
		// itself/siblings.
		"/proc/sys/kernel/yama/ptrace_scope": "2",
		// Anti-spoofing / anti-redirect network hardening - meaningful
		// for a network-facing reverse proxy specifically, not just a
		// generic checklist item: reject packets whose reverse path
		// doesn't match the interface they arrived on, never honor ICMP
		// redirects (a classic MITM vector) or source-routed packets,
		// and never originate ICMP redirects either.
		"/proc/sys/net/ipv4/conf/all/rp_filter":                "1",
		"/proc/sys/net/ipv4/conf/default/rp_filter":            "1",
		"/proc/sys/net/ipv4/conf/all/accept_redirects":         "0",
		"/proc/sys/net/ipv4/conf/default/accept_redirects":     "0",
		"/proc/sys/net/ipv4/conf/all/send_redirects":           "0",
		"/proc/sys/net/ipv4/conf/default/send_redirects":       "0",
		"/proc/sys/net/ipv4/conf/all/accept_source_route":      "0",
		"/proc/sys/net/ipv4/conf/default/accept_source_route":  "0",
		"/proc/sys/net/ipv4/icmp_echo_ignore_broadcasts":       "1",
		"/proc/sys/net/ipv4/icmp_ignore_bogus_error_responses": "1",
		// SYN flood protection - not a generic checklist item here
		// either: this node's entire purpose is accepting inbound
		// connections from the internet as a reverse proxy/load
		// balancer, exactly the exposure tcp_syncookies protects.
		"/proc/sys/net/ipv4/tcp_syncookies": "1",
		// Not hardening, but a network sysctl every node needs: when an
		// interface's primary IPv4 address is removed (janusd moving it
		// to another one in the same subnet), promote a secondary rather
		// than delete them all with it - the kernel's default, which a
		// real reconfiguration hit (internal/netmgr).
		"/proc/sys/net/ipv4/conf/all/promote_secondaries":     "1",
		"/proc/sys/net/ipv4/conf/default/promote_secondaries": "1",
		// VFS-level protections against following an attacker-created
		// hardlink/symlink in a world-writable sticky directory - no
		// such directory actually exists on this rootfs today, but this
		// is cheap, harmless, and forward-looking (STATE, /tmp).
		"/proc/sys/fs/protected_hardlinks": "1",
		"/proc/sys/fs/protected_symlinks":  "1",
	}

	// Sorted, not map iteration order, so console output (and this
	// function's own tests) are deterministic.
	paths := make([]string, 0, len(sysctls))
	for p := range sysctls {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	for _, path := range paths {
		value := sysctls[path]
		if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
			fmt.Printf("init: sysctl %s=%s: %v\n", path, value, err)
			continue
		}
		fmt.Printf("init: sysctl %s=%s\n", path, value)
	}
}

// fileLimit is the open-file limit janusd and what it starts may raise
// their own to. HAProxy sizes its maxconn from it (about half) unless its
// configuration says otherwise, and refuses to start when a "ulimit-n" or
// "maxconn" asks more - while `haproxy -c` accepts them. The kernel gives
// PID 1 a hard limit of 4096: about 2000 connections at most. 524288 is
// the hard limit systemd gives every service, what HAProxy gets on a
// mainstream distribution. The soft limit stays 1024: a program raises
// its own when it needs more, as Go and HAProxy do by themselves.
const fileLimit = 524288

func raiseFileLimit() {
	l := syscall.Rlimit{Cur: 1024, Max: fileLimit}
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &l); err != nil {
		fmt.Printf("init: open files limit %d: %v\n", fileLimit, err)
		return
	}
	fmt.Printf("init: open files limit %d\n", fileLimit)
}

// selinuxPolicyPath is where rootfs/assemble.sh bundles the binary
// policy selinux/classes.conf + selinux/policy.conf compile into (see
// the Makefile's selinux-policy target) - a fixed path on the
// squashfs-backed rootfs, not something init discovers dynamically.
const selinuxPolicyPath = "/etc/selinux/janus.policy"

// loadSELinuxPolicy mounts selinuxfs and writes the compiled binary
// policy straight to /sys/fs/selinux/load - the entire "loading" story
// on a rootfs with no libselinux/load_policy/policy-store userspace at
// all. Must run after mount("sysfs", ...) (selinuxfs lives under /sys)
// but before mountEphemeral overmounts /etc with an empty tmpfs -
// selinuxPolicyPath needs to still be readable off the original
// squashfs-backed /etc at this point, not the fresh empty one.
//
// Missing entirely (an initramfs-only boot - qemu-boot-test/
// qemu-network-test/qemu-hardening-test package no rootfs/assemble.sh
// squashfs at all, so there's no policy file to find) is non-fatal, the
// same tolerant pattern mount() and mountState() already use for a
// missing STATE drive: log and move on. The kernel stays functionally
// as if SELinux were absent (kernel/configs/janus_defconfig's own
// SECURITY_SELINUX_DEVELOP=y keeps it permissive with no policy loaded
// regardless), so those tests' own boots are unaffected either way.
//
// Loading itself never fails on a first load specifically because none
// has happened yet this boot - the kernel's own selinux_disabled bypass
// unconditionally allows the very first security_load_policy() call
// before any policy exists to check permissions against - so this
// either succeeds or the policy file itself is malformed (a real bug in
// how selinux/policy.conf was authored or compiled, not a permission
// problem), logged either way for the same external-verifiability
// reason hardenSysctls logs each write.
func loadSELinuxPolicy() {
	mount("selinuxfs", "/sys/fs/selinux", "selinuxfs")

	policy, err := os.ReadFile(selinuxPolicyPath)
	if err != nil {
		fmt.Printf("init: selinux: no policy at %s: %v\n", selinuxPolicyPath, err)
		return
	}
	if err := os.WriteFile("/sys/fs/selinux/load", policy, 0); err != nil {
		fmt.Printf("init: selinux: load policy: %v\n", err)
		return
	}
	fmt.Printf("init: selinux: loaded policy from %s (%d bytes)\n", selinuxPolicyPath, len(policy))
}

func main() {
	mount("proc", "/proc", "proc")
	mount("sysfs", "/sys", "sysfs")
	loadSELinuxPolicy()
	mount("devtmpfs", "/dev", "devtmpfs")
	mirrorConsole()
	linkVirtioPorts()
	hardenSysctls()
	raiseFileLimit()
	mountEphemeral()
	// /etc/resolv.conf, like the rest of the network, is janusd's
	// (internal/netmgr): the kernel's boot DHCP resolvers by default.
	mountState()
	mountReleaseBundle()
	seedFromNoCloud()
	pendingMarker := checkBootCommit()

	release, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		release = []byte("unknown")
	}

	fmt.Println("Janus init")
	fmt.Printf("kernel: %s", release) // osrelease already ends in \n

	if _, err := os.Stat(daemonPath); err == nil {
		startDaemon(pendingMarker)
		return
	}

	fmt.Println("JANUS_INIT_BOOT_OK")
	// Give the console a moment to flush before the machine goes away.
	time.Sleep(2 * time.Second)
	powerOff()
}

// defaultGiveUpAfter is how long Supervisor keeps restarting a
// crash-looping janusd, on a boot with a pending boot-commit marker
// (see checkBootCommit), before giving up on this slot ever coming up
// at all and reverting - overridden per-boot by the marker's own
// HealthTimeoutSeconds (UpgradeRequest.health_timeout_seconds) when
// set. Irrelevant on an ordinary boot (no marker): Supervisor keeps its
// unconditional "restart forever" policy there, see its own doc
// comment.
const defaultGiveUpAfter = 60 * time.Second

func startDaemon(pendingMarker *bootcommit.Marker) {
	if err := os.MkdirAll("/run/janus", 0o755); err != nil {
		fmt.Printf("init: mkdir /run/janus: %v\n", err)
	}

	fmt.Println("JANUS_INIT_BOOT_OK")

	sv := &Supervisor{
		Path: daemonPath,
		// -manage-host: janusd owns this node's network, hostname and
		// clock (off by default, for janusd run anywhere else).
		Args:       []string{"-addr", ":9505", "-manage-host"},
		Stdout:     os.Stdout,
		Stderr:     os.Stderr,
		MinBackoff: 1 * time.Second,
		MaxBackoff: 30 * time.Second,
		// StableAfter only matters for Supervisor's own crash-backoff
		// reset here - real health confirmation for a pending marker
		// happens inside janusd itself now (cmd/janusd/main.go,
		// against HAProxy's actual stats socket via
		// internal/bootcommit.Confirm), not by inference from how long
		// the janusd *process* merely stayed alive.
		StableAfter: 60 * time.Second,
	}

	// A pending marker means this boot is provisional (see
	// checkBootCommit) - bound how long Supervisor keeps restarting a
	// janusd that's crash-looping so fast, or so often, that it
	// never gets a real chance to run its own HAProxy-level health
	// check at all - the one thing janusd's own confirmation logic
	// can't catch, since it requires janusd to actually be running.
	// Without this, such a slot would sit unreachable forever:
	// checkBootCommit's own cross-boot TriesLeft check only ever gets a
	// chance to act on a boot *after* this one, which requires the
	// machine to actually reboot again first.
	if pendingMarker != nil {
		giveUpAfter := defaultGiveUpAfter
		if pendingMarker.HealthTimeoutSeconds > 0 {
			giveUpAfter = time.Duration(pendingMarker.HealthTimeoutSeconds) * time.Second
		}
		sv.GiveUpAfter = giveUpAfter
		sv.OnGiveUp = func() { giveUpBootCommit(pendingMarker) }
	}

	done := make(chan struct{})
	handlePowerSignals(sv, done)
	sv.Run()
	close(done)
	// Run only returns when a power action or a boot-commit revert is
	// under way; PID 1 must never exit.
	select {}
}

// giveUpBootCommit is Supervisor's OnGiveUp hook, wired up only when
// this boot has a pending marker: janusd has been restarted for
// GiveUpAfter without staying up at all, so it never even got a chance
// to run its own HAProxy-level health check (see cmd/janusd/
// main.go). Re-reads the marker rather than trusting the one it closed
// over, since it could have been cleared by something else in the
// meantime - in which case this is a deliberate no-op.
func giveUpBootCommit(marker *bootcommit.Marker) {
	cur, err := bootcommit.Read()
	if err != nil || cur == nil || cur.Slot != marker.Slot {
		return
	}
	fmt.Printf("init: janusd never stayed up for slot %s - giving up and reverting to slot %s\n", cur.Slot, cur.RevertTo)
	revertAndReboot(cur)
}

// checkBootCommit runs after mountState (needs bootcommit.Dir already
// bind-mounted) and before startDaemon - see internal/bootcommit's own
// package doc for the full mechanism. Three outcomes:
//
//   - no marker, or a marker for some slot other than the one this boot
//     is actually running from (e.g. a Rollback happened in between,
//     making it stale): nothing to do - the marker, if any, is cleared
//     and this returns nil.
//   - a marker for *this* slot with tries remaining: this is the one
//     confirmation attempt LifecycleService.Upgrade granted it. The
//     marker's TriesLeft is decremented and saved *before* handing off
//     to startDaemon - so that if this very boot never confirms
//     (crashes, hangs, gets power-cycled) and the machine comes back up
//     on this same slot again, the *next* call here finds tries already
//     exhausted. Returns the (decremented) marker so startDaemon can
//     honor its HealthTimeoutSeconds.
//   - a marker for this slot with no tries left: a previous boot into
//     this same slot already used its one attempt without ever
//     confirming - whatever's wrong with it isn't fixing itself, so
//     there's no point starting janusd for a third time. Reverts
//     the ESP back to RevertTo (espswitch.Activate - the exact
//     mechanism LifecycleService.Rollback uses, just triggered by init
//     instead of a gRPC call), clears the marker, and reboots
//     immediately - main() never reaches startDaemon at all this boot.
func checkBootCommit() *bootcommit.Marker {
	marker, err := bootcommit.Read()
	if err != nil {
		fmt.Printf("init: read boot-commit marker: %v\n", err)
		return nil
	}
	if marker == nil {
		return nil
	}

	cmdline, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		fmt.Printf("init: read /proc/cmdline for boot-commit check: %v\n", err)
		return nil
	}
	dataDev, ok := bootslot.DataDevice(string(cmdline))
	if !ok {
		return nil
	}
	currentSlot, ok := bootslot.ActiveSlot(dataDev)
	if !ok {
		return nil
	}

	if marker.Slot != currentSlot {
		fmt.Printf("init: boot-commit marker is for slot %s, but this boot is slot %s - stale, clearing\n", marker.Slot, currentSlot)
		if err := bootcommit.Clear(); err != nil {
			fmt.Printf("init: clear stale boot-commit marker: %v\n", err)
		}
		return nil
	}

	if marker.TriesLeft > 0 {
		marker.TriesLeft--
		if err := bootcommit.Write(marker); err != nil {
			fmt.Printf("init: write boot-commit marker: %v\n", err)
		}
		fmt.Printf("init: boot-commit pending for slot %s - awaiting confirmation\n", currentSlot)
		return marker
	}

	fmt.Printf("init: boot-commit for slot %s never confirmed after its one attempt - reverting to slot %s\n", currentSlot, marker.RevertTo)
	revertAndReboot(marker)
	// revertAndReboot only returns on failure (its success path reboots
	// or blocks forever) - fall through to a normal boot of this
	// (apparently still not fully healthy) slot rather than leaving the
	// node completely unreachable.
	return nil
}

// revertAndReboot switches the ESP back to marker.RevertTo and clears
// the marker (internal/bootrevert.To - the same helper
// cmd/janusd's own HAProxy-level revert path uses), then reboots -
// the success path never returns, since PID 1 must not fall through to
// starting janusd on a slot that just proved itself unhealthy.
// Called from two places: checkBootCommit (a *later* boot finding
// tries already exhausted) and giveUpBootCommit (this *same* boot,
// once Supervisor's GiveUpAfter elapses without janusd ever staying
// up) - the two paths that can conclude a slot isn't coming up at all,
// one crossing a reboot to find out and one not needing to.
func revertAndReboot(marker *bootcommit.Marker) {
	if err := bootrevert.To(marker); err != nil {
		fmt.Printf("init: revert to slot %s failed: %v\n", marker.RevertTo, err)
		return
	}
	syscall.Sync()

	fmt.Println("init: rebooting to complete the revert")
	consoledrain.Wait(os.Stdout, 2*time.Second)
	if err := syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART); err != nil {
		fmt.Printf("init: reboot: %v\n", err)
		return
	}
	// Block here (like powerOff does) until the reboot above actually
	// takes effect, rather than returning and letting a caller fall
	// through to something that assumes this boot is still going.
	select {}
}

func powerOff() {
	consoledrain.Wait(os.Stdout, 2*time.Second)
	if err := syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF); err != nil {
		fmt.Printf("init: reboot(POWER_OFF): %v\n", err)
	}
	// PID 1 must never return - if power-off somehow didn't take effect,
	// hang here instead of letting main() exit (which panics the kernel).
	select {}
}
