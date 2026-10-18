// Package probe tests the network from the node: what it resolves a
// name to, whether it reaches an address and a port, what a TLS server
// presents, what an HTTP server answers, whether a host answers ICMP
// echo, and the routers on the way - each step timed, from the node's
// own routes, DNS and addresses (a VRRP VIP, a BGP anycast address as
// the source). SystemService.Probe streams its events: no shell, no
// ping or curl binary on the node.
//
// A probe is bounded: what it may send (a few echo requests, one HTTP
// request without its body), how long it may take, how much it reads.
// It is no scanner: one target, one port.
package probe

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The kinds of probe.
const (
	KindDNS        = "dns"
	KindTCP        = "tcp"
	KindTLS        = "tls"
	KindHTTP       = "http"
	KindPing       = "ping"
	KindTraceroute = "traceroute"
)

// The limits a request is held to.
const (
	MaxDuration    = 60 * time.Second // a whole probe
	MaxCount       = 20               // echo requests
	MinInterval    = 200 * time.Millisecond
	MaxHops        = 30
	MaxPayload     = 9216 // a jumbo frame's
	MaxTimeout     = 10 * time.Second
	MaxHeaders     = 16
	BodyRead       = 1 << 20 // bytes of an HTTP body read, counted
	BodyPreview    = 1024    // of which shown
	maxHeadersBack = 64      // response headers returned
)

// ErrInvalid marks a request refused before anything is sent.
var ErrInvalid = errors.New("invalid probe")

// Request is one probe.
type Request struct {
	Kind string
	// Target: a name (dns), host:port (tcp, tls - port 443 by default),
	// a URL (http), a host (ping, traceroute).
	Target string
	// Source is the node's address to send from: one of its interfaces'
	// at the time - unset, the route's.
	Source netip.Addr

	RecordType string // dns: A, AAAA, CNAME, SRV, TXT, MX, NS, PTR; both families when empty
	Server     string // dns: the server to ask, host[:port]; the node's resolv.conf when empty

	// ConnectTo is where tls and http connect instead of resolving the
	// target's host, host[:port] - the target still gives the SNI and
	// the Host header (curl's --resolve).
	ConnectTo string
	SNI       string   // tls, http: the target's host when empty
	ALPN      []string // tls, http
	CAPEM     []byte   // tls, http: verify against these instead of the system's

	Method  string      // http: GET, HEAD or OPTIONS
	Headers [][2]string // http: name, value

	Count        int           // ping: echo requests, 4 by default
	Interval     time.Duration // ping: between them, 1 s by default
	Size         int           // ping, traceroute: payload bytes, 56 by default
	DontFragment bool          // ping, traceroute: the DF bit - an MTU test
	MaxHops      int           // traceroute: 30 by default

	// Timeout is each step's (a connection, a reply, a hop): 5 s, 2 s
	// for ping and 1 s for traceroute by default.
	Timeout time.Duration
}

// Event is what a probe reports, as it goes.
type Event struct {
	Stage     string  // dns, connect, tls, http, reply, hop, done
	OK        bool    // the step succeeded
	ElapsedMS float64 // the step took
	Message   string  // what happened: an error's text

	DNS     *DNSResult
	Connect *ConnectResult
	TLS     *TLSResult
	HTTP    *HTTPResult
	Reply   *Reply
	Hop     *Hop
	Summary *Summary
}

// DNSResult is a lookup's answer.
type DNSResult struct {
	Name, Type, Server string
	Records            []string
}

// ConnectResult is a TCP connection's two ends.
type ConnectResult struct {
	Local, Remote string
}

// TLSResult is what a TLS server presented.
type TLSResult struct {
	ServerName, Version, Cipher, ALPN string
	Verified                          bool
	VerifyError                       string
	Chain                             []Cert
	OCSP                              string // stapled response: good, revoked, unknown, unreadable; none: ""
}

// Cert is one certificate of a chain.
type Cert struct {
	Subject, Issuer     string
	DNSNames            []string
	IPs                 []string
	NotBefore, NotAfter time.Time
	SHA256              string
}

// HTTPResult is an HTTP server's answer.
type HTTPResult struct {
	Status        int
	StatusText    string
	Proto         string
	Headers       [][2]string
	BodyBytes     int64
	BodyTruncated bool // more than BodyRead
	BodyPreview   string
	TTFBMS        float64 // from the request sent to the first byte back
}

// Reply is one echo request's outcome.
type Reply struct {
	Seq   int
	From  string
	TTL   int
	Bytes int
	RTTMS float64
	Kind  string // reply, timeout, unreachable, time-exceeded, too-big
	MTU   int    // too-big: the MTU a router or the node said
}

// Hop is one step of a traceroute.
type Hop struct {
	TTL     int
	Address string
	Name    string
	RTTsMS  []float64 // one per probe; -1: no answer
	Reached bool      // the target answered
}

// Summary closes a probe.
type Summary struct {
	Sent, Received      int
	MinMS, AvgMS, MaxMS float64
	Reached             bool
	Hops                int
}

// Run runs r, handing each event to emit, the last a "done". An error is
// a request refused (ErrInvalid) or emit's - the client gone; what the
// network did is in the events.
func Run(ctx context.Context, r Request, emit func(Event) error) error {
	if err := r.check(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, MaxDuration)
	defer cancel()
	p := &prober{r: r, emit: emit}
	var err error
	switch r.Kind {
	case KindDNS:
		err = p.dns(ctx)
	case KindTCP, KindTLS, KindHTTP:
		err = p.connection(ctx)
	case KindPing:
		err = p.ping(ctx)
	case KindTraceroute:
		err = p.traceroute(ctx)
	}
	return err
}

type prober struct {
	r    Request
	emit func(Event) error
}

func (r *Request) check() error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
	}
	if r.Target == "" || len(r.Target) > 2048 || strings.ContainsAny(r.Target, " \t\r\n") {
		return bad("a target is required, without spaces")
	}
	if r.Source.IsValid() && !localAddress(r.Source) {
		return bad("%s isn't one of this node's addresses", r.Source)
	}
	if r.Timeout < 0 || r.Timeout > MaxTimeout {
		return bad("timeout: %s at most", MaxTimeout)
	}
	switch r.Kind {
	case KindDNS:
		if r.RecordType != "" && !recordTypes[strings.ToUpper(r.RecordType)] {
			return bad("record type %q: A, AAAA, CNAME, SRV, TXT, MX, NS or PTR", r.RecordType)
		}
		r.RecordType = strings.ToUpper(r.RecordType)
		if r.Server != "" {
			if _, _, err := hostPort(r.Server, 53); err != nil {
				return bad("server: %v", err)
			}
		}
	case KindTCP:
		if _, port, err := net.SplitHostPort(r.Target); err != nil || port == "" {
			return bad("target: host:port")
		}
	case KindTLS:
		if _, _, err := hostPort(r.Target, 443); err != nil {
			return bad("target: %v", err)
		}
	case KindHTTP:
		u, err := url.Parse(r.Target)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return bad("target: an http:// or https:// URL")
		}
		switch r.Method {
		case "":
			r.Method = "GET"
		case "GET", "HEAD", "OPTIONS":
		default:
			return bad("method %q: GET, HEAD or OPTIONS - a probe sends no body", r.Method)
		}
		if len(r.Headers) > MaxHeaders {
			return bad("%d headers, %d at most", len(r.Headers), MaxHeaders)
		}
	case KindPing, KindTraceroute:
		if r.Count == 0 {
			r.Count = 4
		}
		if r.Count < 1 || r.Count > MaxCount {
			return bad("count: 1 to %d", MaxCount)
		}
		if r.Interval == 0 {
			r.Interval = time.Second
		}
		if r.Interval < MinInterval {
			return bad("interval: %s at least", MinInterval)
		}
		if r.Size == 0 {
			r.Size = 56
		}
		if r.Size < 0 || r.Size > MaxPayload {
			return bad("size: up to %d bytes", MaxPayload)
		}
		if r.MaxHops == 0 {
			r.MaxHops = MaxHops
		}
		if r.MaxHops < 1 || r.MaxHops > MaxHops {
			return bad("max hops: 1 to %d", MaxHops)
		}
	default:
		return bad("kind %q: dns, tcp, tls, http, ping or traceroute", r.Kind)
	}
	if r.ConnectTo != "" {
		if r.Kind != KindTLS && r.Kind != KindHTTP {
			return bad("connect_to is for tls and http")
		}
		if _, _, err := hostPort(r.ConnectTo, 0); err != nil {
			return bad("connect_to: %v", err)
		}
	}
	if r.Timeout == 0 {
		switch r.Kind {
		case KindPing:
			r.Timeout = 2 * time.Second
		case KindTraceroute:
			r.Timeout = time.Second
		default:
			r.Timeout = 5 * time.Second
		}
	}
	return nil
}

// hostPort splits "host[:port]" (an IPv6 address in brackets with a
// port), port def when it has none.
func hostPort(s string, def int) (string, int, error) {
	host, portText, err := net.SplitHostPort(s)
	if err != nil {
		if strings.Count(s, ":") > 1 || !strings.Contains(s, ":") {
			host, portText = strings.Trim(s, "[]"), strconv.Itoa(def)
		} else {
			return "", 0, err
		}
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 0 || port > 65535 || (port == 0 && def != 0) {
		return "", 0, fmt.Errorf("port %q", portText)
	}
	if host == "" {
		return "", 0, errors.New("no host")
	}
	return host, port, nil
}

// localAddress reports whether a is on one of the node's interfaces now.
var localAddress = func(a netip.Addr) bool {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, x := range addrs {
		if n, ok := x.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(n.IP); ok && ip.Unmap() == a.Unmap() {
				return true
			}
		}
	}
	return false
}

func since(t time.Time) float64 {
	return float64(time.Since(t).Microseconds()) / 1000
}

// done closes the probe.
func (p *prober) done(ok bool, msg string, s *Summary) error {
	return p.emit(Event{Stage: "done", OK: ok, Message: msg, Summary: s})
}
