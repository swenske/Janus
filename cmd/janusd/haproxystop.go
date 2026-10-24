package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/swenske/Janus/internal/sockdiag"
)

// clientsWatch is haproxy.Manager.StopWatch: the ports HAProxy pid
// listens on - its frontends, not its peers' - read just before it
// soft-stops, and then whether a client's connection to one of them is
// still open. Its connections to servers (an idle pool, health checks)
// and its peers' don't hold a stop: a request in flight has its client's
// side. Any doubt - its descriptors or the sockets unreadable - keeps it
// waiting, as Stop did.
func clientsWatch(proc string, pid, peersPort int) func() bool {
	inodes := socketInodes(proc, pid)
	if inodes == nil {
		return nil
	}
	listening := map[uint16]bool{}
	if !eachSocket(sockdiag.States(sockdiag.Listen), func(s sockdiag.Socket) {
		if inodes[s.Inode] && int(s.Src.Port()) != peersPort {
			listening[s.Src.Port()] = true
		}
	}) {
		return nil
	}
	return func() bool {
		inodes := socketInodes(proc, pid)
		if inodes == nil {
			return true
		}
		left := false
		if !eachSocket(sockdiag.States(sockdiag.SynRecv, sockdiag.Established, sockdiag.CloseWait), func(s sockdiag.Socket) {
			if inodes[s.Inode] && listening[s.Src.Port()] {
				left = true
			}
		}) {
			return true
		}
		return left
	}
}

// eachSocket calls fn on the TCP sockets in states, both families: false
// when they can't be read.
func eachSocket(states uint32, fn func(sockdiag.Socket)) bool {
	for _, family := range []uint8{unix.AF_INET, unix.AF_INET6} {
		if err := sockdiag.TCP(family, states, fn); err != nil {
			return false
		}
	}
	return true
}

// socketInodes are the inodes of the sockets pid holds; nil when its
// descriptors can't be read.
func socketInodes(proc string, pid int) map[uint32]bool {
	dir := filepath.Join(proc, strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := map[uint32]bool{}
	for _, e := range entries {
		link, err := os.Readlink(filepath.Join(dir, e.Name()))
		if err != nil || !strings.HasPrefix(link, "socket:[") {
			continue
		}
		if n, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]"), 10, 32); err == nil {
			out[uint32(n)] = true
		}
	}
	return out
}
