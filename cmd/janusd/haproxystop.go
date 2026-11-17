package main

import (
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/swenske/Janus/internal/haproxy"
	"github.com/swenske/Janus/internal/sockdiag"
)

// clientsWatch is haproxy.Manager.StopWatch: the ports HAProxy pid
// listens on - its frontends, not its peers' - read just before it
// soft-stops, and then whether a client's connection to one of them
// still holds the stop (holds). Its connections to servers (an idle
// pool, health checks) and its peers' don't: a request in flight has
// its client's side. Nor does a client's keep-alive connection idle
// since before the stop: `option idle-close-on-response` (the
// bootstrap's defaults) keeps those open through a soft-stop until their
// next request - for a reload's sake - which a HAProxy stopped for good
// may never get: every reboot with one would have waited Stop's timeout.
// streams are the clients of the requests HAProxy serves just before
// the stop ("show sess", haproxyStreams); nil when they couldn't be
// read. Any doubt - streams unknown, its descriptors or the sockets
// unreadable - keeps it waiting for every client, as Stop did.
func clientsWatch(proc string, pid, peersPort int, streams map[netip.AddrPort]bool) func() bool {
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
	stopped := time.Now() // SIGUSR1 follows at once
	return func() bool {
		inodes := socketInodes(proc, pid)
		if inodes == nil {
			return true
		}
		since := time.Since(stopped)
		left := false
		if !eachSocketInfo(sockdiag.States(sockdiag.SynRecv, sockdiag.Established, sockdiag.CloseWait), func(s sockdiag.Socket) {
			if inodes[s.Inode] && listening[s.Src.Port()] && holds(s, streams, since) {
				left = true
			}
		}) {
			return true
		}
		return left
	}
}

// quiet: data a client's connection received this recently is a request
// arriving - being served.
const quiet = 500 * time.Millisecond

// holds reports whether a client's connection to HAProxy holds a stop
// for good that began since ago, from the requests HAProxy served at the
// stop (streams, by client address) and the connection's tcp_info:
//
//   - a request served at the stop: HAProxy answers it - with
//     "Connection: close", soft-stopped - and closes the connection;
//   - data not acknowledged yet: an answer still leaving;
//   - data received in the last quiet: a request arriving;
//   - data received since about the stop (a request on a connection
//     idle until then - HAProxy answers it and closes): held until its
//     answer has left, the last data sent after the last received;
//   - otherwise idle since before the stop, waiting for a next request:
//     it doesn't hold.
//
// streams nil (unknown) or no tcp_info (a SYN_RECV request socket, an
// older kernel): held, as before.
func holds(s sockdiag.Socket, streams map[netip.AddrPort]bool, since time.Duration) bool {
	switch {
	case streams == nil, !s.HasBytes, s.Unacked > 0, s.LastDataRecv < quiet:
		return true
	case streams[canonical(s.Dst)]:
		return true
	case s.LastDataRecv < since+quiet:
		// The times are "how long ago": received after sent = more recent.
		return s.LastDataRecv < s.LastDataSent
	}
	return false
}

// canonical is a as "show sess" writes it: an IPv4 client of a v4v6
// socket as IPv4.
func canonical(a netip.AddrPort) netip.AddrPort {
	return netip.AddrPortFrom(a.Addr().Unmap(), a.Port())
}

// sessionClient reads a "show sess" source - 192.0.2.1:54321,
// 2001:db8::1:54321 (no brackets: the port is after the last colon) - as
// canonical.
func sessionClient(src string) (netip.AddrPort, bool) {
	i := strings.LastIndexByte(src, ':')
	if i < 0 {
		return netip.AddrPort{}, false
	}
	addr, err := netip.ParseAddr(strings.Trim(src[:i], "[]"))
	if err != nil {
		return netip.AddrPort{}, false
	}
	port, err := strconv.ParseUint(src[i+1:], 10, 16)
	if err != nil {
		return netip.AddrPort{}, false
	}
	return canonical(netip.AddrPortFrom(addr, uint16(port))), true
}

// haproxyStreams are the clients of the requests HAProxy serves now
// (h's "show sess", its own runtime API's aside): nil when they can't
// all be read - clientsWatch then waits for every client.
func haproxyStreams(list func(haproxy.SessionFilter, int) ([]haproxy.Session, bool, error)) map[netip.AddrPort]bool {
	sessions, truncated, err := list(haproxy.SessionFilter{}, 1<<16)
	if err != nil || truncated {
		return nil
	}
	out := map[netip.AddrPort]bool{}
	for _, s := range sessions {
		if s.Proto != "tcpv4" && s.Proto != "tcpv6" {
			continue
		}
		c, ok := sessionClient(s.Source)
		if !ok {
			return nil
		}
		out[c] = true
	}
	return out
}

// eachSocket calls fn on the TCP sockets in states, both families: false
// when they can't be read.
func eachSocket(states uint32, fn func(sockdiag.Socket)) bool {
	return eachFamily(sockdiag.TCP, states, fn)
}

// eachSocketInfo is eachSocket with each socket's tcp_info.
func eachSocketInfo(states uint32, fn func(sockdiag.Socket)) bool {
	return eachFamily(sockdiag.TCPWithBytes, states, fn)
}

func eachFamily(dump func(uint8, uint32, func(sockdiag.Socket)) error, states uint32, fn func(sockdiag.Socket)) bool {
	for _, family := range []uint8{unix.AF_INET, unix.AF_INET6} {
		if err := dump(family, states, fn); err != nil {
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
