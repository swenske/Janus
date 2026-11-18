package main

import (
	"net"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/swenske/Janus/internal/sockdiag"
)

// This process stands for HAProxy: a client's connection to one of its
// listeners holds a stop; its own connection to a server, and one to its
// peers' listener, don't.
func TestClientsWatch(t *testing.T) {
	listen := func() net.Listener {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { l.Close() })
		return l
	}
	frontend, peers := listen(), listen()
	peersPort := peers.Addr().(*net.TCPAddr).Port
	watch := clientsWatch("/proc", os.Getpid(), peersPort, map[netip.AddrPort]bool{})
	if watch == nil {
		t.Fatal("no watch over this process")
	}
	if watch() {
		t.Error("listeners alone are no client")
	}
	// A server elsewhere (another process would do; a listener opened
	// after the watch began isn't one of its ports).
	server := listen()
	toServer, err := net.Dial("tcp", server.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer toServer.Close()
	fromServer, _ := server.Accept()
	defer fromServer.Close()
	toPeers, err := net.Dial("tcp", peers.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer toPeers.Close()
	fromPeers, _ := peers.Accept()
	defer fromPeers.Close()
	if watch() {
		t.Error("a server's connection or a peer's held the stop")
	}
	client, err := net.Dial("tcp", frontend.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	accepted, _ := frontend.Accept()
	if !watch() {
		t.Error("a client's connection not seen")
	}
	client.Close()
	accepted.Close()
	if watch() {
		t.Error("a closed client's connection still seen")
	}
	if clientsWatch("/proc", -1, peersPort, nil) != nil {
		t.Error("unreadable descriptors watched")
	}
}

// TestHolds: a client's connection to HAProxy at a stop for good begun
// since ago - a request served then or since, or idle.
func TestHolds(t *testing.T) {
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
	client := netip.MustParseAddrPort("192.0.2.7:40000")
	served := map[netip.AddrPort]bool{client: true}
	none := map[netip.AddrPort]bool{}
	idle := sockdiag.Socket{HasBytes: true, Dst: client, BytesReceived: 80, LastDataRecv: ms(9000), LastDataSent: ms(8990)}
	with := func(f func(*sockdiag.Socket)) sockdiag.Socket { s := idle; f(&s); return s }
	for _, c := range []struct {
		name    string
		s       sockdiag.Socket
		streams map[netip.AddrPort]bool
		since   time.Duration
		want    bool
	}{
		{"idle since before the stop", idle, none, ms(2000), false},
		{"idle, an IPv4 client of a v4v6 socket", with(func(s *sockdiag.Socket) { s.Dst = netip.MustParseAddrPort("[::ffff:192.0.2.7]:40000") }), none, ms(2000), false},
		{"streams unknown", idle, nil, ms(2000), true},
		{"no tcp_info", sockdiag.Socket{Dst: client}, none, ms(2000), true},
		{"a request served at the stop", idle, served, ms(2000), true},
		{"served at the stop, a v4v6 socket", with(func(s *sockdiag.Socket) { s.Dst = netip.MustParseAddrPort("[::ffff:192.0.2.7]:40000") }), served, ms(2000), true},
		{"an answer not acknowledged yet", with(func(s *sockdiag.Socket) { s.Unacked = 2 }), none, ms(2000), true},
		{"a request arriving", with(func(s *sockdiag.Socket) { s.LastDataRecv = ms(100) }), none, ms(2000), true},
		{"a request since the stop, not answered", with(func(s *sockdiag.Socket) { s.LastDataRecv, s.LastDataSent = ms(1500), ms(9000) }), none, ms(2000), true},
		{"a request since the stop, answered", with(func(s *sockdiag.Socket) { s.LastDataRecv, s.LastDataSent = ms(1500), ms(1400) }), none, ms(2000), false},
		{"a request just before the stop, not answered", with(func(s *sockdiag.Socket) { s.LastDataRecv, s.LastDataSent = ms(2300), ms(9000) }), none, ms(2000), true},
		{"connected before the stop, nothing asked", with(func(s *sockdiag.Socket) { s.BytesReceived, s.LastDataRecv, s.LastDataSent = 0, ms(3000), ms(3000) }), none, ms(2000), false},
	} {
		if got := holds(c.s, c.streams, c.since); got != c.want {
			t.Errorf("%s: holds %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSessionClient(t *testing.T) {
	for src, want := range map[string]string{
		"192.0.2.1:54321":        "192.0.2.1:54321",
		"2001:db8::1:443":        "[2001:db8::1]:443",
		"[2001:db8::1]:443":      "[2001:db8::1]:443",
		"::ffff:192.0.2.1:54321": "192.0.2.1:54321",
		"unix:2":                 "",
		"192.0.2.1":              "",
		"192.0.2.1:http":         "",
	} {
		got, ok := sessionClient(src)
		if want == "" {
			if ok {
				t.Errorf("%s: read as %s", src, got)
			}
			continue
		}
		if !ok || got != netip.MustParseAddrPort(want) {
			t.Errorf("%s: %s %v, want %s", src, got, ok, want)
		}
	}
}

// TestClientsWatchIdleKeepAlive: over the real kernel, a keep-alive
// connection idle when the stop begins doesn't hold it; a request on it
// since does, until answered; one served at the stop does.
func TestClientsWatchIdleKeepAlive(t *testing.T) {
	frontend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer frontend.Close()
	client, err := net.Dial("tcp", frontend.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	accepted, err := frontend.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer accepted.Close()
	request := func() {
		t.Helper()
		if _, err := client.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
			t.Fatal(err)
		}
		if _, err := accepted.Read(make([]byte, 4096)); err != nil {
			t.Fatal(err)
		}
	}
	answer := func() {
		t.Helper()
		if _, err := accepted.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")); err != nil {
			t.Fatal(err)
		}
		if _, err := client.Read(make([]byte, 4096)); err != nil {
			t.Fatal(err)
		}
	}
	settle := func() { time.Sleep(quiet + 300*time.Millisecond) }
	request()
	answer()
	settle() // idle keep-alive when the stop begins

	if clientsWatch("/proc", os.Getpid(), 0, nil)() != true {
		t.Error("streams unknown: the idle connection didn't hold the stop")
	}
	me := canonical(netip.MustParseAddrPort(client.LocalAddr().String()))
	if clientsWatch("/proc", os.Getpid(), 0, map[netip.AddrPort]bool{me: true})() != true {
		t.Error("a request served at the stop didn't hold it")
	}
	watch := clientsWatch("/proc", os.Getpid(), 0, map[netip.AddrPort]bool{})
	if watch() {
		t.Error("a keep-alive connection idle at the stop held it")
	}
	request() // a request since the stop
	if !watch() {
		t.Error("a request arriving didn't hold the stop")
	}
	settle()
	if !watch() {
		t.Error("a request not answered yet didn't hold the stop")
	}
	answer()
	settle()
	if watch() {
		t.Error("an answered connection held the stop")
	}
}
