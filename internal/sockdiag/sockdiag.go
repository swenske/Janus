// Package sockdiag lists TCP sockets through the kernel's sock_diag
// netlink interface - what ss uses: filtered by state in the kernel, read
// as a stream, where /proc/net/tcp costs a formatted line per socket of
// the whole system.
package sockdiag

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"syscall"

	"golang.org/x/sys/unix"
)

// TCP states, the kernel's (include/net/tcp_states.h).
const (
	Established = 1
	SynSent     = 2
	SynRecv     = 3
	FinWait1    = 4
	FinWait2    = 5
	TimeWait    = 6
	Close       = 7
	CloseWait   = 8
	LastAck     = 9
	Listen      = 10
	Closing     = 11
)

// States is the set of states a dump asks for.
func States(states ...uint8) uint32 {
	var set uint32
	for _, s := range states {
		set |= 1 << s
	}
	return set
}

// AllButListen is every state a connection can be in.
var AllButListen = States(Established, SynSent, SynRecv, FinWait1, FinWait2, TimeWait, Close, CloseWait, LastAck, Closing)

// Socket is one TCP socket.
type Socket struct {
	State    uint8
	Src, Dst netip.AddrPort
	// RQueue and WQueue: a listening socket's connections waiting to be
	// accepted, and its backlog - what it asked listen() for, at most
	// net.core.somaxconn.
	RQueue, WQueue uint32
	UID            uint32
	Inode          uint32
}

const (
	headerLen = 16 // struct nlmsghdr
	reqLen    = 56 // struct inet_diag_req_v2
	msgLen    = 72 // struct inet_diag_msg
)

// TCP calls fn with each TCP socket of family (unix.AF_INET or
// unix.AF_INET6) in states.
func TCP(family uint8, states uint32, fn func(Socket)) error {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_SOCK_DIAG)
	if err != nil {
		return fmt.Errorf("sock_diag socket: %w", err)
	}
	defer unix.Close(fd)

	req := make([]byte, headerLen+reqLen)
	binary.NativeEndian.PutUint32(req[0:], uint32(len(req)))
	binary.NativeEndian.PutUint16(req[4:], unix.SOCK_DIAG_BY_FAMILY)
	binary.NativeEndian.PutUint16(req[6:], unix.NLM_F_REQUEST|unix.NLM_F_DUMP)
	binary.NativeEndian.PutUint32(req[8:], 1) // sequence
	req[headerLen] = family
	req[headerLen+1] = unix.IPPROTO_TCP
	binary.NativeEndian.PutUint32(req[headerLen+4:], states)
	if err := unix.Sendto(fd, req, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return fmt.Errorf("sock_diag request: %w", err)
	}

	buf := make([]byte, 64<<10)
	for {
		n, _, err := unix.Recvfrom(fd, buf, 0)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return fmt.Errorf("sock_diag read: %w", err)
		}
		done, err := parse(buf[:n], fn)
		if err != nil || done {
			return err
		}
	}
}

// parse reads one buffer of netlink messages; done at the dump's end.
func parse(b []byte, fn func(Socket)) (done bool, err error) {
	for len(b) >= headerLen {
		size := int(binary.NativeEndian.Uint32(b[0:]))
		if size < headerLen || size > len(b) {
			return false, fmt.Errorf("sock_diag: a message of %d bytes in %d", size, len(b))
		}
		payload := b[headerLen:size]
		switch binary.NativeEndian.Uint16(b[4:]) {
		case unix.NLMSG_DONE:
			return true, nil
		case unix.NLMSG_ERROR:
			if len(payload) < 4 {
				return false, errors.New("sock_diag: a short error message")
			}
			if code := int32(binary.NativeEndian.Uint32(payload)); code != 0 {
				return false, fmt.Errorf("sock_diag: %w", syscall.Errno(-code))
			}
		default:
			if len(payload) >= msgLen {
				fn(socket(payload))
			}
		}
		b = b[min((size+3)&^3, len(b)):] // NLMSG_ALIGN
	}
	return false, nil
}

// socket reads a struct inet_diag_msg.
func socket(m []byte) Socket {
	addr := func(b []byte) netip.Addr {
		if m[0] == unix.AF_INET {
			return netip.AddrFrom4([4]byte(b[:4]))
		}
		return netip.AddrFrom16([16]byte(b[:16]))
	}
	return Socket{
		State:  m[1],
		Src:    netip.AddrPortFrom(addr(m[8:24]), binary.BigEndian.Uint16(m[4:])),
		Dst:    netip.AddrPortFrom(addr(m[24:40]), binary.BigEndian.Uint16(m[6:])),
		RQueue: binary.NativeEndian.Uint32(m[56:]),
		WQueue: binary.NativeEndian.Uint32(m[60:]),
		UID:    binary.NativeEndian.Uint32(m[64:]),
		Inode:  binary.NativeEndian.Uint32(m[68:]),
	}
}
