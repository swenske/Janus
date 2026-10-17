package logship

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// maxUDP is the most of a message a datagram carries (RFC 5426 says
// receivers should take 2048 bytes).
const maxUDP = 2048

// maxSyslog cuts a message over TCP and TLS.
const maxSyslog = 16 << 10

// syslogSink sends to a syslog server, keeping its connection.
type syslogSink struct {
	cfg SyslogTarget
	tls *tls.Config // with transport tls
	env *env

	conn net.Conn
	w    *bufio.Writer
	peer peer
}

func newSyslogSink(t Target, e *env) (*syslogSink, error) {
	s := &syslogSink{cfg: *t.Syslog, env: e}
	if s.cfg.Transport == "tls" {
		host, _, _ := net.SplitHostPort(s.cfg.Address)
		cfg, err := t.TLS.Config(host)
		if err != nil {
			return nil, err
		}
		s.tls = cfg
	}
	return s, nil
}

func (s *syslogSink) connect(ctx context.Context) error {
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	var conn net.Conn
	var err error
	switch s.cfg.Transport {
	case "udp":
		conn, err = d.DialContext(ctx, "udp", s.cfg.Address)
	case "tcp":
		conn, err = d.DialContext(ctx, "tcp", s.cfg.Address)
	default:
		td := &tls.Dialer{NetDialer: d, Config: s.tls}
		conn, err = td.DialContext(ctx, "tcp", s.cfg.Address)
	}
	if err != nil {
		return err
	}
	s.conn, s.w = conn, bufio.NewWriterSize(conn, 64<<10)
	s.peer.set(conn)
	return nil
}

// stale says whether the server closed a stream connection since the
// last batch: a syslog server never writes, so anything a read gets -
// EOF most of all - means it's gone. Without it, the batch after a
// server restart would go into a socket nobody reads, and be lost.
// (Syslog has no acknowledgement: a server going away during a batch
// still loses that one. Elasticsearch confirms each document.)
func (s *syslogSink) stale() bool {
	if s.cfg.Transport == "udp" {
		return false
	}
	_ = s.conn.SetReadDeadline(time.Now().Add(time.Millisecond))
	var b [1]byte
	_, err := s.conn.Read(b[:])
	_ = s.conn.SetReadDeadline(time.Time{})
	var ne net.Error
	timedOut := errors.As(err, &ne) && ne.Timeout()
	return !timedOut // nothing to read: still there
}

func (s *syslogSink) close() {
	if s.conn != nil {
		s.conn.Close()
	}
	s.conn, s.w = nil, nil
}

// Send writes recs. With TCP or TLS, an error leaves the connection
// closed: the next Send dials again, and sends the batch whole again.
func (s *syslogSink) Send(ctx context.Context, recs []Record) (int, error) {
	if s.conn != nil && s.stale() {
		s.close()
	}
	if s.conn == nil {
		if err := s.connect(ctx); err != nil {
			return 0, err
		}
	}
	n, err := s.write(recs)
	if err != nil {
		s.close()
		return 0, err
	}
	return n, nil
}

// write sends recs and says how many bytes went: the messages with
// their framing.
func (s *syslogSink) write(recs []Record) (int, error) {
	_ = s.conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
	host := s.env.meta.hostname()
	var buf []byte
	n := 0
	for _, r := range recs {
		buf = s.encode(buf[:0], r, host)
		if s.cfg.Transport == "udp" {
			w, err := s.conn.Write(buf[:min(len(buf), maxUDP)])
			if err != nil {
				return 0, err
			}
			n += w
			continue
		}
		if len(buf) > maxSyslog {
			buf = buf[:maxSyslog]
		}
		if s.cfg.Framing == "lf" {
			buf = append(buf, '\n')
		} else {
			w, err := s.w.WriteString(strconv.Itoa(len(buf)) + " ")
			if err != nil {
				return 0, err
			}
			n += w
		}
		w, err := s.w.Write(buf)
		if err != nil {
			return 0, err
		}
		n += w
	}
	if s.w != nil {
		if err := s.w.Flush(); err != nil {
			return 0, err
		}
	}
	return n, nil
}

func (s *syslogSink) Peer() (local, remote string) { return s.peer.get() }

func (s *syslogSink) Close() { s.close() }

// facility is a record's syslog facility: the kernel's kern, the
// audit's authpriv, the configured one otherwise.
func (s *syslogSink) facility(r Record) int {
	switch r.Source {
	case SourceKernel:
		return 0
	case SourceAudit:
		return 10
	}
	return facilities[s.cfg.Facility]
}

// encode appends r as a syslog message (no framing).
func (s *syslogSink) encode(dst []byte, r Record, host string) []byte {
	pri := s.facility(r)*8 + clampSeverity(r.Severity)
	msg := strings.ReplaceAll(r.Message, "\n", " ")
	if s.cfg.Format == "rfc3164" {
		// <PRI>Mmm dd hh:mm:ss HOSTNAME TAG: MSG
		dst = fmt.Appendf(dst, "<%d>%s %s %s: %s", pri, r.Time.Format(time.Stamp), field(host, 255), tag(r.Service), msg)
		return dst
	}
	// <PRI>1 TIMESTAMP HOSTNAME APP-NAME PROCID MSGID SD MSG
	return fmt.Appendf(dst, "<%d>1 %s %s %s - %s - %s", pri, r.Time.UTC().Format("2006-01-02T15:04:05.000000Z"),
		field(host, 255), field(r.Service, 48), field(r.Source, 32), msg)
}

func clampSeverity(sev int) int {
	if sev < SevEmergency || sev > SevDebug {
		return SevInfo
	}
	return sev
}

// field is s as a RFC 5424 header field: printable US-ASCII without
// spaces, n bytes at most, "-" when empty.
func field(s string, n int) string {
	b := make([]byte, 0, min(len(s), n))
	for i := 0; i < len(s) && len(b) < n; i++ {
		if c := s[i]; c > 32 && c < 127 {
			b = append(b, c)
		}
	}
	if len(b) == 0 {
		return "-"
	}
	return string(b)
}

// tag is s as a RFC 3164 tag: letters, digits, '-', '_', '.', 32 at most.
func tag(s string) string {
	b := make([]byte, 0, min(len(s), 32))
	for i := 0; i < len(s) && len(b) < 32; i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' {
			b = append(b, c)
		}
	}
	if len(b) == 0 {
		return "janus"
	}
	return string(b)
}

// sink is a target's way to its server.
type sink interface {
	// Send sends recs and says how many bytes it handed the transport.
	Send(ctx context.Context, recs []Record) (int, error)
	// Peer is its latest connection's local and remote addresses
	// (host:port), kept once it's closed - empty before the first.
	Peer() (local, remote string)
	Close()
}

// peer is a sink's latest connection's addresses: where its traffic goes
// (the node's status says through which interface). Set by the sink's
// goroutine, read by the status's.
type peer struct {
	mu            sync.Mutex
	local, remote string
}

func (p *peer) set(c net.Conn) {
	p.mu.Lock()
	p.local, p.remote = c.LocalAddr().String(), c.RemoteAddr().String()
	p.mu.Unlock()
}

func (p *peer) get() (local, remote string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.local, p.remote
}
