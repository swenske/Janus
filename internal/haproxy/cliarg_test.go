package haproxy

import (
	"errors"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeStatsSocket is a stats socket that records every command it
// receives and answers the way HAProxy does for the commands used here.
type fakeStatsSocket struct {
	mu       sync.Mutex
	commands []string
}

func (f *fakeStatsSocket) received() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.commands...)
}

func startFakeStatsSocket(t *testing.T) (*Manager, *fakeStatsSocket) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "admin.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	f := &fakeStatsSocket{}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(conn)
		}
	}()
	return NewManager("/nonexistent/haproxy", "/nonexistent/haproxy.cfg", "/nonexistent/pid", path), f
}

func (f *fakeStatsSocket) serve(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	var buf []byte
	chunk := make([]byte, 4096)
	// A command ends at its newline; a "<<" payload command at the
	// empty line closing its payload.
	complete := func() bool {
		s := string(buf)
		if strings.Contains(s, " <<\n") {
			return strings.HasSuffix(s, "\n\n")
		}
		return strings.HasSuffix(s, "\n")
	}
	for !complete() {
		n, err := conn.Read(chunk)
		buf = append(buf, chunk[:n]...)
		if err != nil {
			break
		}
	}
	cmd := string(buf)
	f.mu.Lock()
	f.commands = append(f.commands, cmd)
	f.mu.Unlock()
	switch {
	case strings.HasPrefix(cmd, "commit ssl cert"), strings.HasPrefix(cmd, "add ssl crt-list"):
		_, _ = conn.Write([]byte("Success!\n"))
	case strings.HasPrefix(cmd, "set ssl cert "):
		// HAProxy 3.4's own reply.
		name, _, _ := strings.Cut(strings.TrimPrefix(cmd, "set ssl cert "), " ")
		_, _ = conn.Write([]byte("Transaction created for certificate " + name + "!\n\n"))
	case strings.HasPrefix(cmd, "del ssl cert"):
		_, _ = conn.Write([]byte("Certificate deleted!\n"))
	case strings.HasPrefix(cmd, "show map "):
		_, _ = conn.Write([]byte("0x55d0 key1 value one\n\n"))
	default:
		_, _ = conn.Write([]byte("\n"))
	}
}

// TestStatsSocketUnescapableRefused: what no escape can make safe - a
// newline or any other control character, whitespace in a name or key
// (printed back unescaped, see cliToken), an empty name - is refused
// before any connection to the socket, MapGet (open to os:reader)
// included.
func TestStatsSocketUnescapableRefused(t *testing.T) {
	m, f := startFakeStatsSocket(t)
	cases := map[string]func() error{
		"MapGet newline":          func() error { _, err := m.MapGet("x\nset server be/s1 state maint"); return err },
		"MapGet carriage return":  func() error { _, err := m.MapGet("x\rset server be/s1 state maint"); return err },
		"MapGet space":            func() error { _, err := m.MapGet("x y"); return err },
		"MapGet empty":            func() error { _, err := m.MapGet(""); return err },
		"MapUpdate key newline":   func() error { return m.MapUpdate("m", "k\nclear map m", "v", false) },
		"MapUpdate key space":     func() error { return m.MapUpdate("m", "k 2", "v", false) },
		"MapUpdate value newline": func() error { return m.MapUpdate("m", "k", "v\nclear map m", false) },
		"MapUpdate value tab":     func() error { return m.MapUpdate("m", "k", "v\tw", false) },
		"MapUpdate delete key":    func() error { return m.MapUpdate("m", "k\nclear map m", "", true) },
		"ACLUpdate acl newline":   func() error { return m.ACLUpdate("#0\nclear acl #0", "10.0.0.1", false) },
		"ACLUpdate value newline": func() error { return m.ACLUpdate("#0", "10.0.0.1\nclear acl #0", false) },
		"SetServerState backend":  func() error { return m.SetServerState("be\nshutdown sessions server be/s1", "s1", "ready") },
		"SetServerState server":   func() error { return m.SetServerState("be", "s 1", "ready") },
		"SetServerState state":    func() error { return m.SetServerState("be", "s1", "") },
		"CertificateUpload name":  func() error { return m.CertificateUpload("a\ndel ssl cert b", []byte("PEM"), "", nil) },
		"CertificateUpload list":  func() error { return m.CertificateUpload("a", []byte("PEM"), "l x", nil) },
		"CertificateUpload sni":   func() error { return m.CertificateUpload("a", []byte("PEM"), "l", []string{"ok.example", "b\nx"}) },
		"CertificateUpload binary": func() error {
			return m.CertificateUpload("a", []byte("-----BEGIN-----\nAA\x00AA\n-----END-----\n"), "", nil)
		},
		"CertificateUpload empty": func() error { return m.CertificateUpload("a", []byte("\n \n\n"), "", nil) },
		"CertificateDelete name":  func() error { return m.CertificateDelete("a\nclear map m", "") },
		"CertificateDelete list":  func() error { return m.CertificateDelete("a", "l\nclear map m") },
	}
	for name, call := range cases {
		if err := call(); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s: err = %v, want ErrInvalidArgument", name, err)
		}
	}
	// Give any stray connection a moment to be recorded before checking.
	time.Sleep(50 * time.Millisecond)
	if got := f.received(); len(got) != 0 {
		t.Fatalf("the stats socket received %q - nothing should reach it", got)
	}
}

// TestStatsSocketEscaping: ";" (the command separator), spaces in values
// and backslashes reach HAProxy escaped, so one call is always exactly
// one command - an attempt to chain one through MapGet (open to
// os:reader) becomes a literal, unknown map name.
func TestStatsSocketEscaping(t *testing.T) {
	m, f := startFakeStatsSocket(t)
	_, _ = m.MapGet("m;clear")
	if err := m.MapUpdate("m", `k;x\y`, "a value; clear map m", false); err != nil {
		t.Fatalf("MapUpdate: %v", err)
	}
	if err := m.ACLUpdate("#0", "1.2.3.4;clear acl #0", true); err != nil {
		t.Fatalf("ACLUpdate: %v", err)
	}
	if err := m.SetServerState("be;x", "s1", "ready"); err != nil {
		t.Fatalf("SetServerState: %v", err)
	}
	want := []string{
		`show map m\;clear` + "\n",
		`del map m k\;x\\y` + "\n",
		`add map m k\;x\\y a\ value\;\ clear\ map\ m` + "\n",
		`del acl #0 1.2.3.4\;clear\ acl\ #0` + "\n",
		`set server be\;x/s1 state ready` + "\n",
	}
	got := f.received()
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("commands sent:\n%q\nwant:\n%q", got, want)
	}
}

// TestStatsSocketValidArguments: ordinary values reach HAProxy as
// before, and a map value with spaces is escaped instead of being cut at
// the first space (confirmed against this project's HAProxy build:
// `add map M k value\ with\ spaces` stores "value with spaces").
func TestStatsSocketValidArguments(t *testing.T) {
	m, f := startFakeStatsSocket(t)

	entries, err := m.MapGet("/etc/haproxy/maps/hosts.map")
	if err != nil {
		t.Fatalf("MapGet: %v", err)
	}
	if entries["key1"] != "value one" {
		t.Errorf("MapGet entries = %v", entries)
	}
	if err := m.MapUpdate("/etc/haproxy/maps/hosts.map", "example.com", "backend one", false); err != nil {
		t.Fatalf("MapUpdate: %v", err)
	}
	if err := m.SetServerState("be", "s1", "drain"); err != nil {
		t.Fatalf("SetServerState: %v", err)
	}
	// No trailing newline: the payload must still be closed by an empty
	// line (it used to leave HAProxy waiting until the socket deadline).
	if err := m.CertificateUpload("site.pem", []byte("-----BEGIN CERTIFICATE-----\r\nAAAA\r\n-----END CERTIFICATE-----"), "/etc/haproxy/crt.list", []string{"www.example.com", "!*.example.org"}); err != nil {
		t.Fatalf("CertificateUpload: %v", err)
	}

	// A blank line inside the bundle (here between cert and key, as
	// `cat cert.pem key.pem` often leaves) would end the payload early:
	// it's dropped, so the text after it - even a would-be command -
	// stays payload.
	if err := m.CertificateUpload("two.pem", []byte("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n\nclear map m\n\n-----BEGIN PRIVATE KEY-----\nBBBB\n-----END PRIVATE KEY-----\n"), "", nil); err != nil {
		t.Fatalf("CertificateUpload with blank lines: %v", err)
	}

	want := []string{
		"show map /etc/haproxy/maps/hosts.map\n",
		"del map /etc/haproxy/maps/hosts.map example.com\n",
		`add map /etc/haproxy/maps/hosts.map example.com backend\ one` + "\n",
		"set server be/s1 state drain\n",
		"new ssl cert site.pem\n",
		"set ssl cert site.pem <<\n-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n\n",
		"commit ssl cert site.pem\n",
		"add ssl crt-list /etc/haproxy/crt.list site.pem www.example.com !*.example.org\n",
		"new ssl cert two.pem\n",
		"set ssl cert two.pem <<\n-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\nclear map m\n-----BEGIN PRIVATE KEY-----\nBBBB\n-----END PRIVATE KEY-----\n\n",
		"commit ssl cert two.pem\n",
	}
	got := f.received()
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("commands sent:\n%q\nwant:\n%q", got, want)
	}
}
