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
	// NewSynRecv: a connection being set up (a request socket, reported as
	// SynRecv).
	NewSynRecv = 12
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

// All is every socket /proc/net/tcp lists: connections, those being set
// up, listeners (not ^0: newer kernels have states it doesn't show).
var All = AllButListen | States(Listen, NewSynRecv)

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
	// BytesAcked and BytesReceived (TCPWithBytes): what it sent that the
	// peer acknowledged and what it received since it opened - tcp_info's;
	// HasBytes says the kernel gave them (not a TIME_WAIT or request
	// socket's).
	BytesAcked, BytesReceived uint64
	HasBytes                  bool
}

const (
	headerLen = 16 // struct nlmsghdr
	reqLen    = 56 // struct inet_diag_req_v2
	msgLen    = 72 // struct inet_diag_msg

	inetDiagInfo = 2      // INET_DIAG_INFO: a struct tcp_info after the message
	nlaTypeMask  = 0x3fff // an attribute's type without NLA_F_NESTED, NLA_F_NET_BYTEORDER
	// struct tcp_info's tcpi_bytes_acked and tcpi_bytes_received (u64,
	// since Linux 4.1 - include/uapi/linux/tcp.h): an older kernel's is
	// shorter.
	tcpiBytesAcked    = 120
	tcpiBytesReceived = 128
	tcpiBytesEnd      = 136
)

// TCP calls fn with each TCP socket of family (unix.AF_INET or
// unix.AF_INET6) in states.
func TCP(family uint8, states uint32, fn func(Socket)) error {
	return dump(family, states, 0, fn)
}

// TCPWithBytes is TCP with each connection's byte counters (tcp_info's:
// the kernel takes each socket's lock to read them, as ss -i does).
func TCPWithBytes(family uint8, states uint32, fn func(Socket)) error {
	return dump(family, states, 1<<(inetDiagInfo-1), fn)
}

func dump(family uint8, states uint32, ext uint8, fn func(Socket)) error {
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
	req[headerLen+2] = ext
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
				s := socket(payload)
				attributes(payload[msgLen:], &s)
				fn(s)
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

// attributes reads the netlink attributes after a struct inet_diag_msg:
// the byte counters from INET_DIAG_INFO's struct tcp_info, the rest
// skipped.
func attributes(b []byte, s *Socket) {
	for len(b) >= 4 {
		size := int(binary.NativeEndian.Uint16(b[0:]))
		if size < 4 || size > len(b) {
			return
		}
		value := b[4:size]
		if binary.NativeEndian.Uint16(b[2:])&nlaTypeMask == inetDiagInfo && len(value) >= tcpiBytesEnd {
			s.BytesAcked = binary.NativeEndian.Uint64(value[tcpiBytesAcked:])
			s.BytesReceived = binary.NativeEndian.Uint64(value[tcpiBytesReceived:])
			s.HasBytes = true
		}
		b = b[min((size+3)&^3, len(b)):] // RTA_ALIGN
	}
}
