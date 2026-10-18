package probe

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/ocsp"
)

// connection answers tcp, tls and http probes: resolve (unless
// ConnectTo or an address), connect - each address in turn until one
// answers -, then the TLS handshake and the HTTP request.
func (p *prober) connection(ctx context.Context) error {
	host, port, scheme := p.endpoint()
	dialHost, dialPort := host, port
	if p.r.ConnectTo != "" {
		h, pt, _ := hostPort(p.r.ConnectTo, 0)
		dialHost = h
		if pt != 0 {
			dialPort = pt
		}
	}
	addrs, failed, err := p.addresses(ctx, dialHost)
	if err != nil {
		return err
	}
	if failed != "" {
		return p.done(false, failed, nil)
	}

	var conn net.Conn
	for _, a := range addrs {
		start := time.Now()
		d := net.Dialer{Timeout: p.r.Timeout}
		if p.r.Source.IsValid() {
			d.LocalAddr = &net.TCPAddr{IP: p.r.Source.AsSlice()}
		}
		remote := netip.AddrPortFrom(a, uint16(dialPort)).String() //nolint:gosec // G115: hostPort keeps ports to 0-65535
		c, err := d.DialContext(ctx, "tcp", remote)
		ev := Event{Stage: "connect", OK: err == nil, ElapsedMS: since(start), Connect: &ConnectResult{Remote: remote}}
		if err != nil {
			ev.Message = err.Error()
		} else {
			ev.Connect.Local = c.LocalAddr().String()
		}
		if e := p.emit(ev); e != nil {
			if c != nil {
				c.Close()
			}
			return e
		}
		if err == nil {
			conn = c
			break
		}
	}
	if conn == nil {
		return p.done(false, "no address answered", nil)
	}
	defer conn.Close()
	if p.r.Kind == KindTCP {
		return p.done(true, "connected", nil)
	}

	if p.r.Kind == KindTLS || scheme == "https" {
		tc, ok, err := p.handshake(ctx, conn, host)
		if err != nil {
			return err
		}
		if !ok {
			return p.done(false, "the TLS handshake failed", nil)
		}
		if p.r.Kind == KindTLS {
			return p.done(true, "handshake done", nil)
		}
		conn = tc
	}
	return p.request(ctx, conn)
}

// endpoint is the target's host and port, and an http probe's scheme.
func (p *prober) endpoint() (host string, port int, scheme string) {
	switch p.r.Kind {
	case KindHTTP:
		u, _ := url.Parse(p.r.Target)
		port = 80
		if u.Scheme == "https" {
			port = 443
		}
		if u.Port() != "" {
			port, _ = strconv.Atoi(u.Port())
		}
		return u.Hostname(), port, u.Scheme
	case KindTLS:
		host, port, _ = hostPort(p.r.Target, 443)
		return host, port, ""
	}
	h, pt, _ := net.SplitHostPort(p.r.Target)
	port, _ = strconv.Atoi(pt)
	return h, port, ""
}

// handshake runs TLS over conn - accepting any certificate, then
// verifying it here, so a bad certificate is a result to show rather
// than a handshake cut short. ok: the handshake completed.
func (p *prober) handshake(ctx context.Context, conn net.Conn, host string) (*tls.Conn, bool, error) {
	sni := p.r.SNI
	if sni == "" {
		if _, err := netip.ParseAddr(host); err != nil {
			sni = host
		}
	}
	alpn := p.r.ALPN
	if len(alpn) == 0 && p.r.Kind == KindHTTP {
		alpn = []string{"h2", "http/1.1"}
	}
	tc := tls.Client(conn, &tls.Config{
		ServerName:         sni,
		NextProtos:         alpn,
		InsecureSkipVerify: true,             //nolint:gosec // G402: the chain is verified below, and reported - not trusted
		MinVersion:         tls.VersionTLS10, //nolint:gosec // G402: a probe reports an old server's version rather than refusing it
	})
	hctx, cancel := context.WithTimeout(ctx, p.r.Timeout)
	defer cancel()
	start := time.Now()
	err := tc.HandshakeContext(hctx)
	ev := Event{Stage: "tls", OK: err == nil, ElapsedMS: since(start)}
	if err != nil {
		ev.Message = err.Error()
		return nil, false, p.emit(ev)
	}
	st := tc.ConnectionState()
	res := &TLSResult{ServerName: sni, Version: tls.VersionName(st.Version), Cipher: tls.CipherSuiteName(st.CipherSuite), ALPN: st.NegotiatedProtocol}
	for _, c := range st.PeerCertificates {
		sum := sha256.Sum256(c.Raw)
		cert := Cert{Subject: c.Subject.String(), Issuer: c.Issuer.String(), DNSNames: c.DNSNames, NotBefore: c.NotBefore, NotAfter: c.NotAfter, SHA256: hex.EncodeToString(sum[:])}
		for _, ip := range c.IPAddresses {
			cert.IPs = append(cert.IPs, ip.String())
		}
		res.Chain = append(res.Chain, cert)
	}
	name := cmpOr(sni, host)
	if verr := p.verify(st, name); verr != nil {
		res.VerifyError = verr.Error()
		ev.Message = "certificate not trusted: " + verr.Error()
	} else {
		res.Verified = true
	}
	res.OCSP = stapled(st)
	ev.TLS = res
	return tc, true, p.emit(ev)
}

// verify checks the chain the server sent for name, against CAPEM or
// the system's roots.
func (p *prober) verify(st tls.ConnectionState, name string) error {
	if len(st.PeerCertificates) == 0 {
		return errors.New("no certificate")
	}
	opts := x509.VerifyOptions{DNSName: name, Intermediates: x509.NewCertPool()}
	if len(p.r.CAPEM) > 0 {
		opts.Roots = x509.NewCertPool()
		if !opts.Roots.AppendCertsFromPEM(p.r.CAPEM) {
			return errors.New("the CA given holds no certificate")
		}
	}
	for _, c := range st.PeerCertificates[1:] {
		opts.Intermediates.AddCert(c)
	}
	_, err := st.PeerCertificates[0].Verify(opts)
	return err
}

// stapled reads the OCSP response the server stapled, if any.
func stapled(st tls.ConnectionState) string {
	if len(st.OCSPResponse) == 0 {
		return ""
	}
	var issuer *x509.Certificate
	if len(st.PeerCertificates) > 1 {
		issuer = st.PeerCertificates[1]
	}
	resp, err := ocsp.ParseResponse(st.OCSPResponse, issuer)
	if err != nil {
		return "unreadable"
	}
	switch resp.Status {
	case ocsp.Good:
		return "good"
	case ocsp.Revoked:
		return "revoked"
	}
	return "unknown"
}

// request sends the HTTP request over conn - already connected, and
// TLS done when https - and reads the answer, its body counted up to
// BodyRead.
func (p *prober) request(ctx context.Context, conn net.Conn) error {
	var used atomic.Bool
	once := func(context.Context, string, string) (net.Conn, error) {
		if used.Swap(true) {
			return nil, errors.New("one connection per probe")
		}
		return conn, nil
	}
	tr := &http.Transport{DisableKeepAlives: true, ResponseHeaderTimeout: p.r.Timeout, ForceAttemptHTTP2: true}
	if _, ok := conn.(*tls.Conn); ok {
		tr.DialTLSContext = once
	} else {
		tr.DialContext = once
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	req, err := http.NewRequestWithContext(ctx, p.r.Method, p.r.Target, nil)
	if err != nil {
		return p.done(false, err.Error(), nil)
	}
	req.Header.Set("User-Agent", "janus-probe")
	for _, h := range p.r.Headers {
		if strings.EqualFold(h[0], "Host") {
			req.Host = h[1]
			continue
		}
		req.Header.Add(h[0], h[1])
	}
	// The trace's callbacks run on the transport's goroutines: HTTP/2
	// writes a request on one of its own, and may say it's written after
	// the answer's first byte came back.
	var (
		mu          sync.Mutex
		sent, first time.Time
	)
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		WroteRequest:         func(httptrace.WroteRequestInfo) { mu.Lock(); sent = time.Now(); mu.Unlock() },
		GotFirstResponseByte: func() { mu.Lock(); first = time.Now(); mu.Unlock() },
	}))
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		if e := p.emit(Event{Stage: "http", ElapsedMS: since(start), Message: err.Error()}); e != nil {
			return e
		}
		return p.done(false, "no HTTP answer", nil)
	}
	defer resp.Body.Close()
	body := make([]byte, BodyPreview)
	n, _ := io.ReadFull(resp.Body, body)
	rest, _ := io.Copy(io.Discard, io.LimitReader(resp.Body, BodyRead-int64(n)+1))
	total := int64(n) + rest
	res := &HTTPResult{
		Status: resp.StatusCode, StatusText: http.StatusText(resp.StatusCode), Proto: resp.Proto,
		BodyBytes: min(total, BodyRead), BodyTruncated: total > BodyRead,
		BodyPreview: strings.ToValidUTF8(string(body[:n]), string(utf8.RuneError)),
	}
	mu.Lock()
	if !first.IsZero() {
		from := start // the request, when it said it was written too late to tell
		if !sent.IsZero() && sent.Before(first) {
			from = sent
		}
		res.TTFBMS = float64(first.Sub(from).Microseconds()) / 1000
	}
	mu.Unlock()
	for name, values := range resp.Header {
		for _, v := range values {
			if len(res.Headers) < maxHeadersBack {
				res.Headers = append(res.Headers, [2]string{name, v})
			}
		}
	}
	slices.SortStableFunc(res.Headers, func(a, b [2]string) int { return strings.Compare(a[0], b[0]) })
	if err := p.emit(Event{Stage: "http", OK: true, ElapsedMS: since(start), HTTP: res, Message: resp.Status}); err != nil {
		return err
	}
	return p.done(true, fmt.Sprintf("%s %s", resp.Proto, resp.Status), nil)
}
