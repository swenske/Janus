package probe

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

// run runs r and collects its events.
func run(t *testing.T, r Request) []Event {
	t.Helper()
	var events []Event
	if err := Run(context.Background(), r, func(e Event) error { events = append(events, e); return nil }); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(events) == 0 || events[len(events)-1].Stage != "done" {
		t.Fatalf("events don't end with done: %+v", events)
	}
	return events
}

func stage(events []Event, name string) *Event {
	for i := range events {
		if events[i].Stage == name {
			return &events[i]
		}
	}
	return nil
}

func TestCheck(t *testing.T) {
	orig := localAddress
	localAddress = func(a netip.Addr) bool { return a == netip.MustParseAddr("10.0.0.5") }
	t.Cleanup(func() { localAddress = orig })
	for _, r := range []Request{
		{Kind: "nmap", Target: "x"},
		{Kind: KindDNS, Target: ""},
		{Kind: KindDNS, Target: "a b"},
		{Kind: KindDNS, Target: "x", RecordType: "AXFR"},
		{Kind: KindDNS, Target: "x", Server: "ns:99999"},
		{Kind: KindTCP, Target: "example.com"},
		{Kind: KindHTTP, Target: "ftp://example.com/"},
		{Kind: KindHTTP, Target: "https://example.com/", Method: "POST"},
		{Kind: KindHTTP, Target: "https://example.com/", Headers: make([][2]string, MaxHeaders+1)},
		{Kind: KindPing, Target: "10.0.0.1", Count: MaxCount + 1},
		{Kind: KindPing, Target: "10.0.0.1", Interval: 10 * time.Millisecond},
		{Kind: KindPing, Target: "10.0.0.1", Size: MaxPayload + 1},
		{Kind: KindTraceroute, Target: "10.0.0.1", MaxHops: MaxHops + 1},
		{Kind: KindPing, Target: "10.0.0.1", Source: netip.MustParseAddr("192.0.2.1")},
		{Kind: KindPing, Target: "10.0.0.1", ConnectTo: "10.0.0.2"},
		{Kind: KindTLS, Target: "x:443", Timeout: time.Minute},
	} {
		if err := r.check(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: %v, want ErrInvalid", r, err)
		}
	}
	ok := Request{Kind: KindPing, Target: "10.0.0.1", Source: netip.MustParseAddr("10.0.0.5")}
	if err := ok.check(); err != nil || ok.Count != 4 || ok.Interval != time.Second || ok.Size != 56 || ok.Timeout != 2*time.Second {
		t.Errorf("defaults: %+v, %v", ok, err)
	}
	for in, want := range map[string]string{"h": "h:443", "h:8443": "h:8443", "[::1]:8443": "::1:8443", "::1": "::1:443", "[::1]": "::1:443"} {
		host, port, err := hostPort(in, 443)
		if got := fmt.Sprintf("%s:%d", host, port); err != nil || got != want {
			t.Errorf("hostPort(%q) = %s, %v", in, got, err)
		}
	}
}

// dnsServer answers probe.test's A, AAAA and TXT records.
func dnsServer(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			var p dnsmessage.Parser
			h, err := p.Start(buf[:n])
			if err != nil {
				continue
			}
			q, err := p.Question()
			if err != nil {
				continue
			}
			b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: h.ID, Response: true, Authoritative: true})
			_ = b.StartQuestions()
			_ = b.Question(q)
			_ = b.StartAnswers()
			rh := dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: 60}
			if q.Name.String() == "probe.test." {
				switch q.Type {
				case dnsmessage.TypeA:
					_ = b.AResource(rh, dnsmessage.AResource{A: [4]byte{127, 0, 0, 1}})
				case dnsmessage.TypeAAAA:
					_ = b.AAAAResource(rh, dnsmessage.AAAAResource{AAAA: [16]byte{15: 1}})
				case dnsmessage.TypeTXT:
					_ = b.TXTResource(rh, dnsmessage.TXTResource{TXT: []string{"v=probe"}})
				}
			}
			out, _ := b.Finish()
			_, _ = pc.WriteTo(out, addr)
		}
	}()
	return pc.LocalAddr().String()
}

func TestDNS(t *testing.T) {
	server := dnsServer(t)
	ev := run(t, Request{Kind: KindDNS, Target: "probe.test", Server: server})
	d := stage(ev, "dns")
	if d == nil || !d.OK || !slices.Equal(slices.Sorted(slices.Values(d.DNS.Records)), []string{"127.0.0.1", "::1"}) || d.DNS.Server != server || !ev[len(ev)-1].OK {
		t.Errorf("A+AAAA: %+v %+v", d, d.DNS)
	}
	ev = run(t, Request{Kind: KindDNS, Target: "probe.test", Server: server, RecordType: "txt"})
	if d := stage(ev, "dns"); d == nil || strings.Join(d.DNS.Records, ",") != "v=probe" || d.DNS.Type != "TXT" {
		t.Errorf("TXT: %+v", d)
	}
	ev = run(t, Request{Kind: KindDNS, Target: "nothing.test", Server: server})
	if d := stage(ev, "dns"); d == nil || d.OK || d.Message == "" || ev[len(ev)-1].OK {
		t.Errorf("no such name: %+v", ev)
	}
}

func TestTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	ev := run(t, Request{Kind: KindTCP, Target: ln.Addr().String()})
	if c := stage(ev, "connect"); c == nil || !c.OK || c.Connect.Remote != ln.Addr().String() || !strings.HasPrefix(c.Connect.Local, "127.0.0.1:") || stage(ev, "dns") != nil {
		t.Errorf("connect: %+v", ev)
	}
	addr := ln.Addr().String()
	ln.Close()
	ev = run(t, Request{Kind: KindTCP, Target: addr, Timeout: time.Second})
	if c := stage(ev, "connect"); c == nil || c.OK || !strings.Contains(c.Message, "refused") || ev[len(ev)-1].OK {
		t.Errorf("refused: %+v", ev)
	}
}

// testCert is a CA and a certificate it signed for probe.test - not
// httptest's, shared by every test of a process.
func testCert(t *testing.T) (tls.Certificate, []byte) {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "probe test CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "probe.test"}, DNSNames: []string{"probe.test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, caCert, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der, caDER}, PrivateKey: key}, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
}

func TestTLS(t *testing.T) {
	cert, caPEM := testCert(t)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"h2"}})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _ = c.(*tls.Conn).Handshake(); c.Close() }()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())

	ev := run(t, Request{Kind: KindTLS, Target: "probe.test:" + port, ConnectTo: "127.0.0.1", CAPEM: caPEM, ALPN: []string{"h2"}})
	tl := stage(ev, "tls")
	if tl == nil || !tl.OK || !tl.TLS.Verified || tl.TLS.ServerName != "probe.test" || tl.TLS.ALPN != "h2" || tl.TLS.Version != "TLS 1.3" || len(tl.TLS.Chain) != 2 || tl.TLS.Chain[0].DNSNames[0] != "probe.test" {
		t.Errorf("trusted: %+v", tl)
	}
	// The system's roots don't know the test CA; another name doesn't
	// match: shown, not refused.
	ev = run(t, Request{Kind: KindTLS, Target: "127.0.0.1:" + port, SNI: "other.test"})
	if tl := stage(ev, "tls"); tl == nil || !tl.OK || tl.TLS.Verified || tl.TLS.VerifyError == "" || !ev[len(ev)-1].OK {
		t.Errorf("untrusted: %+v", tl)
	}
}

func TestHTTP(t *testing.T) {
	cert, caPEM := testCert(t)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Host", r.Host)
		w.Header().Set("X-Probe", r.Header.Get("X-Probe"))
		switch r.URL.Path {
		case "/big":
			_, _ = w.Write(make([]byte, BodyRead+10))
		case "/moved":
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		default:
			_, _ = w.Write([]byte("hello from " + r.Proto))
		}
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())

	ev := run(t, Request{Kind: KindHTTP, Target: "https://probe.test:" + port + "/", ConnectTo: "127.0.0.1", CAPEM: caPEM, Headers: [][2]string{{"X-Probe", "1"}}})
	h := stage(ev, "http")
	if h == nil || !h.OK || h.HTTP.Status != 200 || h.HTTP.Proto != "HTTP/2.0" || h.HTTP.BodyPreview != "hello from HTTP/2.0" || !stage(ev, "tls").TLS.Verified {
		t.Fatalf("https: %+v", h)
	}
	headers := map[string]string{}
	for _, x := range h.HTTP.Headers {
		headers[x[0]] = x[1]
	}
	if headers["X-Host"] != "probe.test:"+port || headers["X-Probe"] != "1" {
		t.Errorf("headers back: %v", headers)
	}

	ev = run(t, Request{Kind: KindHTTP, Target: "https://probe.test:" + port + "/big", ConnectTo: "127.0.0.1", CAPEM: caPEM})
	if h := stage(ev, "http"); h == nil || h.HTTP.BodyBytes != BodyRead || !h.HTTP.BodyTruncated || len(h.HTTP.BodyPreview) != BodyPreview {
		t.Errorf("big body: %+v", h.HTTP)
	}
	ev = run(t, Request{Kind: KindHTTP, Target: "https://probe.test:" + port + "/moved", ConnectTo: "127.0.0.1", CAPEM: caPEM, Method: "HEAD"})
	if h := stage(ev, "http"); h == nil || h.HTTP.Status != 302 {
		t.Errorf("a redirect is answered, not followed: %+v", h)
	}

	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("plain " + r.Host)) }))
	defer plain.Close()
	ev = run(t, Request{Kind: KindHTTP, Target: plain.URL + "/", Headers: [][2]string{{"Host", "virtual.test"}}})
	if h := stage(ev, "http"); h == nil || h.HTTP.BodyPreview != "plain virtual.test" || h.HTTP.Proto != "HTTP/1.1" || stage(ev, "tls") != nil {
		t.Errorf("http: %+v", ev)
	}
}

// TestQuotedEcho: our echo request found in what an ICMP error quotes.
func TestQuotedEcho(t *testing.T) {
	echo, _ := (&icmp.Message{Type: ipv4.ICMPTypeEcho, Body: &icmp.Echo{ID: 0x1234, Seq: 7, Data: []byte("x")}}).Marshal(nil)
	ip := make([]byte, 20)
	ip[0], ip[9] = 0x45, 1
	if id, seq, ok := quotedEcho(append(ip, echo...), false); !ok || id != 0x1234 || seq != 7 {
		t.Errorf("IPv4: %x %d %v", id, seq, ok)
	}
	ip6 := make([]byte, 40)
	ip6[6] = 58
	e6 := make([]byte, 8)
	e6[0] = 128
	binary.BigEndian.PutUint16(e6[4:], 0xbeef)
	binary.BigEndian.PutUint16(e6[6:], 9)
	if id, seq, ok := quotedEcho(append(ip6, e6...), true); !ok || id != 0xbeef || seq != 9 {
		t.Errorf("IPv6: %x %d %v", id, seq, ok)
	}
	if _, _, ok := quotedEcho(ip, false); ok {
		t.Error("a truncated quote read")
	}

	// A router's "time exceeded" about it, and a "fragmentation needed"
	// with the next hop's MTU.
	s := &icmpSock{id: 0x1234}
	te, _ := (&icmp.Message{Type: ipv4.ICMPTypeTimeExceeded, Body: &icmp.TimeExceeded{Data: append(append([]byte{}, ip...), echo...)}}).Marshal(nil)
	if a, ok := s.parse(te); !ok || a.kind != "time-exceeded" || a.seq != 7 {
		t.Errorf("time exceeded: %+v %v", a, ok)
	}
	fn, _ := (&icmp.Message{Type: ipv4.ICMPTypeDestinationUnreachable, Code: 4, Body: &icmp.DstUnreach{Data: append(append([]byte{}, ip...), echo...)}}).Marshal(nil)
	binary.BigEndian.PutUint16(fn[6:], 1400)
	if a, ok := s.parse(fn); !ok || a.kind != "too-big" || a.mtu != 1400 {
		t.Errorf("fragmentation needed: %+v %v", a, ok)
	}
	other := &icmpSock{id: 0x9999}
	if _, ok := other.parse(te); ok {
		t.Error("another prober's error taken")
	}
}

// TestPingLoopback needs a raw socket: root (CI's containers), skipped
// otherwise.
func TestPingLoopback(t *testing.T) {
	if s, err := openICMP(netip.MustParseAddr("127.0.0.1"), netip.Addr{}, false); err != nil {
		t.Skipf("no raw ICMP socket here: %v", err)
	} else {
		s.close()
	}
	ev := run(t, Request{Kind: KindPing, Target: "127.0.0.1", Count: 2, Interval: MinInterval})
	done := ev[len(ev)-1]
	if !done.OK || done.Summary.Sent != 2 || done.Summary.Received != 2 {
		t.Errorf("ping: %+v", ev)
	}
	ev = run(t, Request{Kind: KindTraceroute, Target: "127.0.0.1"})
	if h := stage(ev, "hop"); h == nil || !h.Hop.Reached || h.Hop.TTL != 1 {
		t.Errorf("traceroute: %+v", ev)
	}
}
