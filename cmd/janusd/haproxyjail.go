package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

// haproxyJailArg is the hidden first argument that turns a janusd
// re-exec into the HAProxy jail: it sets up the mount namespace, then
// exec's the real haproxy. See runHAProxyJail.
const haproxyJailArg = "__haproxy-jail"

// jailHiddenPaths are the STATE-backed paths detached from HAProxy's
// mount namespace before it starts (security review 2026-11-05 #1,
// JANUS-2026-008). HAProxy parses its configuration as root, before the
// chroot and the drop to uid 1000, and several directives open a file at
// parse time and then serve or expose its bytes (`http-request return
// ... file`, pattern files relisted through the runtime API, ...). Left
// reachable, STATE's secrets - the node CA key under pki/ (which signs
// os:admin certificates, so an operator who may ApplyConfig could become
// admin), the Controller registration token, the ACME account key and
// DNS credentials and the log-forwarding credentials under config/ -
// could be named by a configuration an operator applies. HAProxy reads
// its own materials (haproxy.cfg, the error pages and CAs under files/,
// the ACME and runtime certificates, the cluster peers certificate) only
// through /etc/haproxy, a mount kept in place, so detaching these costs
// it nothing.
//
// Each of the /etc/janus/* paths is an independent bind mount of a STATE
// subdirectory (rootfs/init), so each must be detached in its own right;
// /etc/.state, the STATE filesystem itself, is detached last because it
// is the path the same secrets also reach (/etc/.state/pki/...). A lazy
// detach keeps the STATE superblock alive as long as /etc/haproxy pins
// it.
var jailHiddenPaths = []string{
	"/etc/janus/pki",
	"/etc/janus/config",
	"/etc/janus/controller",
	"/etc/janus/network",
	"/etc/.state",
}

// haproxyJailSpawn returns a Manager.Spawn that runs HAProxy - the real
// process and every `haproxy -c` - as a janusd re-exec in a private
// mount namespace (jailHiddenPaths detached). self is this janusd
// binary's path.
func haproxyJailSpawn(self string) func(name string, arg ...string) *exec.Cmd {
	return func(name string, arg ...string) *exec.Cmd {
		cmd := exec.Command(self, append([]string{haproxyJailArg, name}, arg...)...)
		// A private mount namespace: the detaches below never reach
		// janusd's own mounts or the host's.
		cmd.SysProcAttr = &syscall.SysProcAttr{Unshareflags: syscall.CLONE_NEWNS}
		return cmd
	}
}

// runHAProxyJail is the hidden re-exec (haproxyJailArg): in the private
// mount namespace it was cloned into, it makes the mounts private, hides
// jailHiddenPaths, and exec's args[0] (the haproxy binary) with args[1:].
// It never returns - it exec's or exits. A failure to hide a path is
// fatal: HAProxy must never run with STATE's secrets reachable.
func runHAProxyJail(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "haproxy jail: no command")
		os.Exit(2)
	}
	// Don't propagate the detaches back to janusd or the host, whatever
	// the parent mounts' propagation.
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		jailFatal("make mounts private", err)
	}
	for _, p := range jailHiddenPaths {
		switch err := unix.Unmount(p, unix.MNT_DETACH); {
		case err == nil, errors.Is(err, unix.EINVAL), errors.Is(err, unix.ENOENT):
			// Detached, or not a mount here / absent (a host, an image
			// without that directory): nothing reachable through it.
		default:
			// EPERM/EACCES (a SELinux denial) above all: stop here rather
			// than run HAProxy with the path still reachable.
			jailFatal("hide "+p, err)
		}
	}
	bin := args[0]
	if err := syscall.Exec(bin, args, os.Environ()); err != nil {
		jailFatal("exec "+bin, err)
	}
}

func jailFatal(what string, err error) {
	fmt.Fprintf(os.Stderr, "haproxy jail: %s: %v\n", what, err)
	os.Exit(1)
}
