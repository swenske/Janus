package sockdiag

import (
	"encoding/binary"
	"net"
	"net/netip"
	"testing"

	"golang.org/x/sys/unix"
)

// listen opens a TCP listener on 127.0.0.1 with backlog, the way
// net.Listen can't (it asks for net.core.somaxconn).
func listen(t *testing.T, backlog int) (fd int, port uint16, inode uint64) {
	t.Helper()
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unix.Close(fd) })
	if err := unix.Bind(fd, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	if err := unix.Listen(fd, backlog); err != nil {
		t.Fatal(err)
	}
	sa, err := unix.Getsockname(fd)
	if err != nil {
		t.Fatal(err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		t.Fatal(err)
	}
	return fd, uint16(sa.(*unix.SockaddrInet4).Port), st.Ino
}

// TestListenersFromTheKernel dumps this test's own listener from the
// real kernel: its backlog, its queue of connections not accepted yet,
// its inode - and finds the connections made to it among the others.
func TestListenersFromTheKernel(t *testing.T) {
	_, port, inode := listen(t, 7)
	for range 2 {
		c, err := net.Dial("tcp", netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), port).String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
	}

	var found *Socket
	if err := TCP(unix.AF_INET, States(Listen), func(s Socket) {
		if s.Src.Port() == port {
			found = &s
		}
		if s.State != Listen {
			t.Errorf("asked for listeners, got state %d", s.State)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if found == nil {
		t.Fatalf("listener on port %d not found", port)
	}
	if found.WQueue != 7 || found.RQueue != 2 || uint64(found.Inode) != inode || found.Src.Addr() != netip.MustParseAddr("127.0.0.1") {
		t.Errorf("listener %+v, want backlog 7, 2 waiting, inode %d", *found, inode)
	}

	clients := 0
	if err := TCP(unix.AF_INET, AllButListen, func(s Socket) {
		if s.Dst.Port() == port && s.State == Established {
			clients++
		}
	}); err != nil {
		t.Fatal(err)
	}
	if clients != 2 {
		t.Errorf("%d client sockets to port %d, want 2", clients, port)
	}
}

// TestParse: messages split across reads, an error, the end.
func TestParse(t *testing.T) {
	msg := func(typ uint16, payload []byte) []byte {
		b := make([]byte, headerLen, headerLen+len(payload)+3)
		binary.NativeEndian.PutUint32(b, uint32(headerLen+len(payload)))
		binary.NativeEndian.PutUint16(b[4:], typ)
		b = append(b, payload...)
		for len(b)%4 != 0 {
			b = append(b, 0)
		}
		return b
	}
	diag := make([]byte, msgLen)
	diag[0], diag[1] = unix.AF_INET6, TimeWait
	binary.BigEndian.PutUint16(diag[4:], 40000)
	binary.BigEndian.PutUint16(diag[6:], 443)
	copy(diag[24:40], netip.MustParseAddr("2001:db8::1").AsSlice())
	binary.NativeEndian.PutUint32(diag[68:], 99)

	var got []Socket
	done, err := parse(append(msg(unix.SOCK_DIAG_BY_FAMILY, diag), msg(unix.SOCK_DIAG_BY_FAMILY, diag)...), func(s Socket) { got = append(got, s) })
	if err != nil || done || len(got) != 2 {
		t.Fatalf("two sockets: %v, %v, %d", done, err, len(got))
	}
	if s := got[0]; s.State != TimeWait || s.Src.Port() != 40000 || s.Dst != netip.MustParseAddrPort("[2001:db8::1]:443") || s.Inode != 99 {
		t.Errorf("socket %+v", s)
	}
	if done, err := parse(msg(unix.NLMSG_DONE, make([]byte, 4)), nil); !done || err != nil {
		t.Errorf("the end: %v, %v", done, err)
	}
	errMsg := make([]byte, 4+headerLen)
	code := -int32(unix.EACCES)
	binary.NativeEndian.PutUint32(errMsg, uint32(code))
	if _, err := parse(msg(unix.NLMSG_ERROR, errMsg), nil); err == nil || err.Error() != "sock_diag: permission denied" {
		t.Errorf("an error: %v", err)
	}
}
