package logship

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swenske/Janus/internal/logvol"
	"github.com/swenske/Janus/internal/tlstrust"
)

func TestClassify(t *testing.T) {
	for _, c := range []struct {
		service, line, source string
		sev                   int
		msg                   string
	}{
		{"janusd", "2026/10/15 17:39:09 api: SystemService/Reboot: admin (os:admin)", SourceJanusd, SevNotice, "api: SystemService/Reboot: admin (os:admin)"},
		{"janusd", "2026/10/15 17:39:06 netmgr: hostname lb1", SourceJanusd, SevInfo, "netmgr: hostname lb1"},
		{"haproxy", "[WARNING]  (85) : Server app/app1 is DOWN", SourceHAProxy, SevWarning, ""},
		{"haproxy", "[ALERT]    (85) : config : parsing", SourceHAProxy, SevAlert, ""},
		{"haproxy", `10.0.2.2:34824 [15/Oct/2026:17:11:37.211] fe be/s1 0/0/0/1/1 200 2 - - ---- 1/1/0/0/0 0/0 "GET / HTTP/1.1"`, SourceAccess, SevInfo, ""},
		{"haproxy", "<131>Oct 15 17:11:37 haproxy[85]: 10.0.2.2 ...", SourceAccess, SevError, ""},
		{"consul", "2026-10-15T17:00:00.000Z [ERROR] agent: something", SourceExtensions, SevError, ""},
		{"bird", "2026-10-15 17:00:00.000 <WARN> BGP peer_a: something", SourceExtensions, SevWarning, ""},
		{"prometheus-node-exporter", `time=... level=INFO source=node_exporter.go msg="Listening"`, SourceExtensions, SevInfo, ""},
		{"keepalived", "VRRP_Instance(VI_1) Entering MASTER STATE", SourceExtensions, SevInfo, ""},
		{"kernel", "[    0.000000] Linux version", SourceKernel, SevInfo, ""},
	} {
		src, sev, msg := Classify(c.service, c.line)
		want := c.msg
		if want == "" {
			want = c.line
		}
		if src != c.source || sev != c.sev || msg != want {
			t.Errorf("Classify(%s, %.30q) = %s %d %.40q; want %s %d", c.service, c.line, src, sev, msg, c.source, c.sev)
		}
	}
}

func TestConfigValidate(t *testing.T) {
	good := Config{Targets: []Target{
		{Name: "synology", Syslog: &SyslogTarget{Address: "nas.example"}},
		{Name: "tls.1", Sources: []string{"audit", "access"}, Syslog: &SyslogTarget{Address: "[2001:db8::1]", Transport: "tls", Format: "rfc3164"}},
	}, Labels: map[string]string{"env": "prod"}, AccessLogsOnDisk: "always"}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	s0, s1 := good.Targets[0].Syslog, good.Targets[1].Syslog
	if s0.Address != "nas.example:514" || s0.Transport != "udp" || s0.Format != "rfc5424" || s0.Framing != "octet-counting" || s0.Facility != "local0" {
		t.Errorf("defaults: %+v", s0)
	}
	if s1.Address != "[2001:db8::1]:6514" || s1.Framing != "lf" {
		t.Errorf("tls defaults: %+v", s1)
	}
	for _, bad := range []Config{
		{Targets: []Target{{Name: "Bad Name", Syslog: &SyslogTarget{Address: "x"}}}},
		{Targets: []Target{{Name: "a", Syslog: &SyslogTarget{Address: "x"}}, {Name: "a", Syslog: &SyslogTarget{Address: "y"}}}},
		{Targets: []Target{{Name: "a"}}},
		{Targets: []Target{{Name: "a", Syslog: &SyslogTarget{Address: "x"}, Elasticsearch: &ElasticsearchTarget{URL: "https://e"}}}},
		{Targets: []Target{{Name: "a", Sources: []string{"nope"}, Syslog: &SyslogTarget{Address: "x"}}}},
		{Targets: []Target{{Name: "a", Syslog: &SyslogTarget{Address: "x:99999"}}}},
		{Targets: []Target{{Name: "a", Syslog: &SyslogTarget{Address: "x", Transport: "sctp"}}}},
		{Targets: []Target{{Name: "a", Syslog: &SyslogTarget{Address: "x", Facility: "kern"}}}},
		{Targets: []Target{{Name: "a", Syslog: &SyslogTarget{Address: "x"}, TLS: tlstrust.Trust{ServerName: "y"}}}},
		{Targets: []Target{{Name: "a", Elasticsearch: &ElasticsearchTarget{URL: "ftp://e"}}}},
		{Targets: []Target{{Name: "a", Elasticsearch: &ElasticsearchTarget{URL: "https://u:p@e"}}}},
		{Targets: []Target{{Name: "a", Elasticsearch: &ElasticsearchTarget{URL: "https://e", APIKey: "k", Username: "u", Password: "p"}}}},
		{Targets: []Target{{Name: "a", Elasticsearch: &ElasticsearchTarget{URL: "https://e", Namespace: "Not-OK"}}}},
		{AccessLogsOnDisk: "sometimes"},
		{Labels: map[string]string{"Bad-Key": "v"}},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("accepted: %+v", bad)
		}
	}
}

func TestSecretsStayOnTheNode(t *testing.T) {
	saved := Config{Targets: []Target{
		{Name: "es", Elasticsearch: &ElasticsearchTarget{URL: "https://e", APIKey: "secret-key"}},
		{Name: "es2", Elasticsearch: &ElasticsearchTarget{URL: "https://f", Username: "u", Password: "secret-pw"}},
		{Name: "tls", Syslog: &SyslogTarget{Address: "x", Transport: "tls"}, TLS: tlstrust.Trust{ClientCertPEM: "CERT", ClientKeyPEM: "secret-client-key"}},
	}}
	shown := saved.Redacted()
	text := fmt.Sprintf("%+v %+v %+v", *shown.Targets[0].Elasticsearch, *shown.Targets[1].Elasticsearch, shown.Targets[2].TLS)
	if strings.Contains(text, "secret") {
		t.Fatalf("Redacted shows a secret: %s", text)
	}
	if saved.Targets[0].Elasticsearch.APIKey != "secret-key" {
		t.Fatal("Redacted changed the original")
	}
	if !saved.Targets[0].HasSecret()["api_key"] || !saved.Targets[2].HasSecret()["client_key"] {
		t.Fatal("HasSecret")
	}
	// Read back, changed, set again: the secrets stay - but not for a
	// target pointed elsewhere.
	shown.Targets[0].Sources = []string{"audit"}
	shown.Targets[1].Elasticsearch.URL = "https://elsewhere"
	shown.MergeSecrets(saved)
	if shown.Targets[0].Elasticsearch.APIKey != "secret-key" || shown.Targets[2].TLS.ClientKeyPEM != "secret-client-key" {
		t.Fatalf("secrets lost: %+v", shown)
	}
	if shown.Targets[1].Elasticsearch.Password != "" {
		t.Fatal("a password went to another URL")
	}
}

func TestSyslogEncoding(t *testing.T) {
	r := Record{Time: time.Date(2026, 10, 15, 17, 39, 9, 123456789, time.UTC), Source: SourceJanusd, Service: "janusd", Severity: SevNotice, Message: "api: SystemService/Reboot: admin\nsecond line"}
	s := &syslogSink{cfg: SyslogTarget{Format: "rfc5424", Facility: "local0"}}
	if got, want := string(s.encode(nil, r, "lb 1")), "<133>1 2026-10-15T17:39:09.123456Z lb1 janusd - janusd - api: SystemService/Reboot: admin second line"; got != want {
		t.Errorf("rfc5424:\n got %q\nwant %q", got, want)
	}
	k := Record{Time: r.Time, Source: SourceKernel, Service: "kernel", Severity: SevWarning, Message: "m"}
	if got := string(s.encode(nil, k, "")); !strings.HasPrefix(got, "<4>1 ") || !strings.Contains(got, " - kernel - kernel - m") {
		t.Errorf("kernel: %q", got)
	}
	a := Record{Time: r.Time, Source: SourceAudit, Service: "janusd", Severity: SevNotice, Message: "m"}
	if got := string(s.encode(nil, a, "h")); !strings.HasPrefix(got, "<85>1 ") {
		t.Errorf("audit (authpriv): %q", got)
	}
	s.cfg.Format = "rfc3164"
	if got, want := string(s.encode(nil, Record{Time: r.Time, Service: "prometheus-node-exporter", Severity: -1, Message: "x"}, "lb1")), "<134>Oct 15 17:39:09 lb1 prometheus-node-exporter: x"; got != want {
		t.Errorf("rfc3164: %q, want %q", got, want)
	}
}

// sink is a test syslog server: what it received, message by message.
type testServer struct {
	t       *testing.T
	ln      net.Listener
	pc      net.PacketConn
	framing string
	mu      sync.Mutex
	msgs    []string
	conns   []net.Conn
}

func newServer(t *testing.T, transport, framing string, tlsCfg *tls.Config) *testServer {
	srv := &testServer{t: t, framing: framing}
	switch transport {
	case "udp":
		pc, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		srv.pc = pc
		go func() {
			buf := make([]byte, 65536)
			for {
				n, _, err := pc.ReadFrom(buf)
				if err != nil {
					return
				}
				srv.add(string(buf[:n]))
			}
		}()
	default:
		var ln net.Listener
		var err error
		if tlsCfg != nil {
			ln, err = tls.Listen("tcp", "127.0.0.1:0", tlsCfg)
		} else {
			ln, err = net.Listen("tcp", "127.0.0.1:0")
		}
		if err != nil {
			t.Fatal(err)
		}
		srv.ln = ln
		go srv.accept()
	}
	t.Cleanup(srv.close)
	return srv
}

func (srv *testServer) addr() string {
	if srv.pc != nil {
		return srv.pc.LocalAddr().String()
	}
	return srv.ln.Addr().String()
}

func (srv *testServer) accept() {
	for {
		c, err := srv.ln.Accept()
		if err != nil {
			return
		}
		srv.mu.Lock()
		srv.conns = append(srv.conns, c)
		srv.mu.Unlock()
		go func() {
			r := bufio.NewReader(c)
			for {
				var msg string
				if srv.framing == "lf" {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					msg = strings.TrimSuffix(line, "\n")
				} else {
					l, err := r.ReadString(' ')
					if err != nil {
						return
					}
					n, err := strconv.Atoi(strings.TrimSpace(l))
					if err != nil {
						srv.t.Errorf("bad octet count %q", l)
						return
					}
					b := make([]byte, n)
					if _, err := io.ReadFull(r, b); err != nil {
						return
					}
					msg = string(b)
				}
				srv.add(msg)
			}
		}()
	}
}

func (srv *testServer) add(m string) {
	srv.mu.Lock()
	srv.msgs = append(srv.msgs, m)
	srv.mu.Unlock()
}

func (srv *testServer) close() {
	if srv.pc != nil {
		srv.pc.Close()
	}
	if srv.ln != nil {
		srv.ln.Close()
	}
	srv.mu.Lock()
	for _, c := range srv.conns {
		c.Close()
	}
	srv.mu.Unlock()
}

func (srv *testServer) got() []string {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	return append([]string(nil), srv.msgs...)
}

// waitFor waits until the server holds n messages containing want.
func (srv *testServer) waitFor(t *testing.T, want string, n int) []string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var hits []string
		for _, m := range srv.got() {
			if strings.Contains(m, want) {
				hits = append(hits, m)
			}
		}
		if len(hits) >= n {
			return hits
		}
		if time.Now().After(deadline) {
			t.Fatalf("the server got %d messages with %q, want %d; it has: %q", len(hits), want, n, srv.got())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSyslogTransports(t *testing.T) {
	ca, caKey := testCA(t)
	leaf := testLeaf(t, ca, caKey, "logs.example")
	for _, c := range []struct {
		name               string
		transport, framing string
		format             string
	}{
		{"udp", "udp", "", "rfc5424"},
		{"tcp octet-counting", "tcp", "octet-counting", "rfc5424"},
		{"tcp lf 3164", "tcp", "lf", "rfc3164"},
		{"tls", "tls", "octet-counting", "rfc5424"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var tlsCfg *tls.Config
			var trust tlstrust.Trust
			if c.transport == "tls" {
				tlsCfg = &tls.Config{Certificates: []tls.Certificate{leaf}}
				trust = tlstrust.Trust{CAPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Raw})), ServerName: "logs.example"}
			}
			srv := newServer(t, c.transport, cmpOr(c.framing, "octet-counting"), tlsCfg)
			cfg := Config{Targets: []Target{{Name: "t", Syslog: &SyslogTarget{Address: srv.addr(), Transport: c.transport, Format: c.format, Framing: c.framing}, TLS: trust}}}
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}
			sh := New(Options{Meta: Meta{Hostname: func() string { return "node1" }}})
			defer sh.Close()
			sh.Apply(cfg)
			for i := range 3 {
				sh.Offer(Record{Time: time.Now(), Source: SourceJanusd, Service: "janusd", Severity: SevInfo, Message: fmt.Sprintf("hello %d", i)})
			}
			got := srv.waitFor(t, "hello", 3)
			if !strings.Contains(got[0], "node1") || !strings.HasSuffix(got[2], "hello 2") {
				t.Fatalf("got %q", got)
			}
			if st := sh.Status(); len(st) != 1 || st[0].State != "ok" || st[0].Sent != 3 {
				t.Fatalf("status = %+v", st)
			}
			// The bytes are the messages with their framing; the
			// addresses, the connection's.
			want := 0
			for _, m := range got {
				switch {
				case c.transport == "udp":
					want += len(m)
				case c.framing == "lf":
					want += len(m) + 1
				default:
					want += len(strconv.Itoa(len(m))) + 1 + len(m)
				}
			}
			st := sh.Status()[0]
			if st.BytesSent != uint64(want) || st.Remote != srv.addr() || !strings.HasPrefix(st.Local, "127.0.0.1:") {
				t.Fatalf("bytes %d (want %d), local %q, remote %q (want %q)", st.BytesSent, want, st.Local, st.Remote, srv.addr())
			}
		})
	}
}

// A server down, then back: what the volume holds goes once it's back,
// from where the target was - and from where it was after a restart of
// the shipper (janusd), no more.
func TestVolumeCursorAcrossOutagesAndRestarts(t *testing.T) {
	dir := t.TempDir()
	volPath := filepath.Join(dir, "logs.img")
	if err := os.WriteFile(volPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(volPath, 8<<20); err != nil {
		t.Fatal(err)
	}
	vol, err := logvol.Open(volPath, logvol.Options{BootID: [16]byte{1}})
	if err != nil {
		t.Fatal(err)
	}
	defer vol.Close()

	srv := newServer(t, "tcp", "octet-counting", nil)
	addr := srv.addr()
	cfg := Config{Targets: []Target{{Name: "t", Sources: []string{SourceJanusd}, Syslog: &SyslogTarget{Address: addr, Transport: "tcp"}}}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	sh := New(Options{Volume: vol})
	sh.Apply(cfg)
	line := vol.Line("janusd")
	line("2026/10/15 17:00:00 before the outage")
	line("2026/10/15 17:00:00 a kernel line is another source") // still janusd's: taken
	vol.Append(logvol.Record{Service: "kernel", Severity: 6, Message: "[ 1.0] not taken"})
	if err := vol.Flush(); err != nil {
		t.Fatal(err)
	}
	srv.waitFor(t, "before the outage", 1)

	// The server goes away; lines keep coming.
	srv.close()
	for i := range 5 {
		line(fmt.Sprintf("2026/10/15 17:00:01 during the outage %d", i))
	}
	if err := vol.Flush(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)

	// Back on the same address.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Skipf("the port went elsewhere meanwhile: %v", err)
	}
	srv2 := &testServer{t: t, ln: ln, framing: "octet-counting"}
	go srv2.accept()
	t.Cleanup(srv2.close)
	got := srv2.waitFor(t, "during the outage", 5)
	if !strings.HasSuffix(got[4], "during the outage 4") {
		t.Fatalf("order: %q", got)
	}
	for _, m := range srv.got() {
		if strings.Contains(m, "not taken") {
			t.Fatalf("a source the target doesn't take was sent: %q", m)
		}
	}
	sh.Close() // saves the cursor on the volume

	// janusd restarts: from where it was.
	line("2026/10/15 17:00:02 after the restart")
	if err := vol.Flush(); err != nil {
		t.Fatal(err)
	}
	sh = New(Options{Volume: vol})
	defer sh.Close()
	sh.Apply(cfg)
	srv2.waitFor(t, "after the restart", 1)
	time.Sleep(300 * time.Millisecond)
	if n := len(srv2.waitFor(t, "during the outage", 5)); n != 5 {
		t.Fatalf("after a restart, the outage's lines were sent %d times", n)
	}
}

// A target down longer than the volume holds: the lines the volume
// wrote over before the target read them are counted lost, the rest
// waiting (queued) until sent - sent and lost add up to what was written.
func TestVolumeOverwrittenCountsLost(t *testing.T) {
	volPath := filepath.Join(t.TempDir(), "logs.img")
	if err := os.WriteFile(volPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(volPath, 4<<20); err != nil {
		t.Fatal(err)
	}
	vol, err := logvol.Open(volPath, logvol.Options{BootID: [16]byte{2}})
	if err != nil {
		t.Fatal(err)
	}
	defer vol.Close()
	srv := newServer(t, "tcp", "octet-counting", nil)
	addr := srv.addr()
	srv.close()
	cfg := Config{Targets: []Target{{Name: "t", Sources: []string{SourceJanusd}, Syslog: &SyslogTarget{Address: addr, Transport: "tcp"}}}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	sh := New(Options{Volume: vol})
	defer sh.Close()
	sh.Apply(cfg)

	// 8 MiB of lines through a 4 MiB volume, the server away.
	line, pad := vol.Line("janusd"), strings.Repeat("x", 1000)
	const total = 8000
	for i := range total {
		line(fmt.Sprintf("2026/10/15 17:00:00 line %d %s", i, pad))
		if i%500 == 499 {
			if err := vol.Flush(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if st := sh.Status()[0]; st.QueuedRecords < total/4 {
		t.Fatalf("waiting %d, want the lines the volume still holds", st.QueuedRecords)
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Skipf("the port went elsewhere meanwhile: %v", err)
	}
	srv2 := &testServer{t: t, ln: ln, framing: "octet-counting"}
	go srv2.accept()
	t.Cleanup(srv2.close)
	var st TargetStatus
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if st = sh.Status()[0]; st.Sent+st.Dropped >= total && st.QueuedRecords == 0 {
			break
		}
	}
	if st.Sent+st.Dropped != total || st.Dropped == 0 || st.Sent < total/4 {
		t.Fatalf("sent %d + lost %d, want %d with some lost; waiting %d", st.Sent, st.Dropped, total, st.QueuedRecords)
	}
	if got := len(srv2.waitFor(t, "line ", int(st.Sent))); uint64(got) != st.Sent {
		t.Fatalf("the server got %d lines, %d sent", got, st.Sent)
	}
}

// What's offered before the first configuration - janusd starting, on a
// node without a LOGS partition: the kernel's boot, its first lines -
// goes to the targets it brings, in order, before what follows; a
// second configuration doesn't send it again.
func TestOfferedBeforeTheFirstApply(t *testing.T) {
	srv := newServer(t, "tcp", "octet-counting", nil)
	cfg := Config{Targets: []Target{{Name: "t", Sources: []string{SourceKernel, SourceJanusd}, Syslog: &SyslogTarget{Address: srv.addr(), Transport: "tcp"}}}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	sh := New(Options{Meta: Meta{Hostname: func() string { return "lb1" }}})
	defer sh.Close()
	for i := range 300 {
		sh.Offer(Record{Time: time.Now(), Source: SourceKernel, Service: "kernel", Severity: SevInfo, Message: fmt.Sprintf("[ 0.%06d] boot line %d", i, i)})
	}
	sh.Offer(Record{Time: time.Now(), Source: SourceJanusd, Service: "janusd", Severity: SevInfo, Message: "logs: no LOGS partition"})
	sh.Offer(Record{Time: time.Now(), Source: SourceAccess, Service: "haproxy", Severity: SevInfo, Message: "not taken"})
	sh.Apply(cfg)
	sh.Offer(Record{Time: time.Now(), Source: SourceJanusd, Service: "janusd", Severity: SevInfo, Message: "after the configuration"})
	got := srv.waitFor(t, "", 302)
	if !strings.HasSuffix(got[0], "boot line 0") || !strings.HasSuffix(got[299], "boot line 299") ||
		!strings.HasSuffix(got[300], "no LOGS partition") || !strings.HasSuffix(got[301], "after the configuration") {
		t.Fatalf("order: %q ... %q", got[0], got[298:])
	}
	if !strings.Contains(got[0], " lb1 ") {
		t.Errorf("hostname: %q", got[0])
	}
	sh.Apply(Config{Targets: []Target{cfg.Targets[0], {Name: "u", Syslog: &SyslogTarget{Address: srv.addr(), Transport: "tcp"}}}})
	time.Sleep(500 * time.Millisecond)
	for _, m := range srv.got() {
		if strings.Contains(m, "not taken") {
			t.Fatalf("a source no target takes was sent: %q", m)
		}
	}
	if n := len(srv.got()); n != 302 {
		t.Errorf("%d messages after a second configuration, want 302 (nothing sent again)", n)
	}
}

// TestDrain: what was offered is sent before Drain returns; a target
// that can't send isn't waited for.
func TestDrain(t *testing.T) {
	srv := newServer(t, "tcp", "octet-counting", nil)
	down := newServer(t, "tcp", "octet-counting", nil)
	downAddr := down.addr()
	down.close() // nothing listens there any more
	cfg := Config{Targets: []Target{
		{Name: "up", Syslog: &SyslogTarget{Address: srv.addr(), Transport: "tcp"}},
		{Name: "down", Syslog: &SyslogTarget{Address: downAddr, Transport: "tcp"}},
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	sh := New(Options{Meta: Meta{Hostname: func() string { return "lb1" }}})
	defer sh.Close()
	sh.Apply(cfg)
	for i := range 200 {
		sh.Offer(Record{Time: time.Now(), Source: SourceJanusd, Service: "janusd", Severity: SevInfo, Message: fmt.Sprintf("line %d", i)})
	}
	start := time.Now()
	sh.Drain(5 * time.Second)
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("Drain took %s: it waited for the failing target", took)
	}
	for _, st := range sh.Status() {
		if st.Name == "up" && st.QueuedRecords != 0 {
			t.Errorf("Drain returned with %d records still queued for the working target", st.QueuedRecords)
		}
	}
	srv.waitFor(t, "line 199", 1)
}

func TestFeedsAndTheirCursorFile(t *testing.T) {
	srv := newServer(t, "tcp", "octet-counting", nil)
	var mu sync.Mutex
	var items []FeedItem
	changed := make(chan struct{})
	feed := Feed{
		Name: SourceEvents,
		Since: func(id uint64) ([]FeedItem, uint64) {
			mu.Lock()
			defer mu.Unlock()
			var out []FeedItem
			for _, it := range items {
				if it.ID > id {
					out = append(out, it)
				}
			}
			return out, id
		},
		Latest: func() uint64 {
			mu.Lock()
			defer mu.Unlock()
			if len(items) == 0 {
				return 0
			}
			return items[len(items)-1].ID
		},
		Changed: func() <-chan struct{} { mu.Lock(); defer mu.Unlock(); return changed },
	}
	publish := func(id uint64, msg string) {
		mu.Lock()
		items = append(items, FeedItem{ID: id, Record: Record{Time: time.Now(), Source: SourceEvents, Service: "janusd", Message: msg}})
		close(changed)
		changed = make(chan struct{})
		mu.Unlock()
	}
	publish(1, "older than the target")
	cursorFile := filepath.Join(t.TempDir(), "cursors.json")
	cfg := Config{Targets: []Target{{Name: "t", Sources: []string{SourceEvents}, Syslog: &SyslogTarget{Address: srv.addr(), Transport: "tcp"}}}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	sh := New(Options{Feeds: []Feed{feed}, CursorFile: cursorFile})
	sh.Apply(cfg)
	publish(2, "event two")
	publish(3, "event three")
	srv.waitFor(t, "event three", 1)
	sh.Close()
	b, err := os.ReadFile(cursorFile)
	if err != nil || !strings.Contains(string(b), `"t":3`) {
		t.Fatalf("cursor file: %s %v", b, err)
	}
	for _, m := range srv.got() {
		if strings.Contains(m, "older than the target") {
			t.Fatal("a new target sent what was there before it")
		}
	}
	publish(4, "event four")
	sh = New(Options{Feeds: []Feed{feed}, CursorFile: cursorFile})
	defer sh.Close()
	sh.Apply(cfg)
	srv.waitFor(t, "event four", 1)
	if n := len(srv.waitFor(t, "event three", 1)); n != 1 {
		t.Fatalf("event three sent %d times", n)
	}
}

func TestAFullQueueDropsTheOldest(t *testing.T) {
	q := &queue{}
	for i := range queueRecords + 5 {
		q.push(Record{Message: strconv.Itoa(i)})
	}
	recs, end := q.peek(2, 1<<20)
	if q.droppedCount() != 5 || recs[0].Message != "5" || end != 7 {
		t.Fatalf("dropped %d, first %q, end %d", q.droppedCount(), recs[0].Message, end)
	}
	// Dropped meanwhile: pop doesn't remove what it never gave.
	q.push(Record{Message: "x"})
	q.pop(end)
	if r, _ := q.peek(1, 1<<20); r[0].Message != "7" {
		t.Fatalf("after pop: %q", r[0].Message)
	}
}

func TestTestSendsOneRecord(t *testing.T) {
	srv := newServer(t, "tcp", "octet-counting", nil)
	sh := New(Options{Meta: Meta{Agent: "janusd"}})
	defer sh.Close()
	tg := Target{Name: "probe", Syslog: &SyslogTarget{Address: srv.addr(), Transport: "tcp"}}
	if err := sh.Test(context.Background(), tg); err != nil {
		t.Fatal(err)
	}
	srv.waitFor(t, "log forwarding test from", 1)
	tg.Syslog.Address = "127.0.0.1:1" // nothing there
	if err := sh.Test(context.Background(), tg); err == nil {
		t.Fatal("a test to nowhere succeeded")
	}
}

func testCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Logs CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return c, key
}

func testLeaf(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, name string) tls.Certificate {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
