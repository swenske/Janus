// Package httpsclient is how the Controller reaches a service an admin
// names - an identity provider, a Consul server: over https only,
// trusting the server's certificate as tlstrust says (the system's
// store, a CA, a fingerprint), the TLS configuration made for each host,
// no proxy, no redirect followed, each answer capped and each request
// bounded in time.
package httpsclient

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/swenske/Janus/internal/tlstrust"
)

// Options bound the client.
type Options struct {
	// MaxBytes caps each answer's body: reading past it is an error.
	MaxBytes int64
	// Timeout bounds each request, its answer read whole.
	Timeout time.Duration
	// What names the server in errors: "provider", "Consul server".
	What string
}

// New is a client trusting servers as t says.
func New(t tlstrust.Trust, o Options) *http.Client {
	tr := &http.Transport{
		Proxy: nil,
		// The TLS configuration is made for each host: a provider's token
		// and key endpoints may be on another than its issuer's.
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			cfg, err := t.Config(host)
			if err != nil {
				return nil, err
			}
			d := &tls.Dialer{NetDialer: &net.Dialer{Timeout: 10 * time.Second}, Config: cfg}
			return d.DialContext(ctx, network, addr)
		},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: o.Timeout,
		MaxIdleConns:          4,
		IdleConnTimeout:       90 * time.Second,
	}
	return &http.Client{
		Transport: capped{next: tr, o: o},
		Timeout:   o.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

type capped struct {
	next http.RoundTripper
	o    Options
}

func (t capped) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" {
		return nil, fmt.Errorf("%s: a %s is reached over https only", req.URL.Redacted(), t.o.What)
	}
	resp, err := t.next.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	resp.Body = &cappedBody{rc: resp.Body, left: t.o.MaxBytes + 1, err: fmt.Errorf("the %s's answer is over %d bytes", t.o.What, t.o.MaxBytes)}
	return resp, nil
}

type cappedBody struct {
	rc   io.ReadCloser
	left int64
	err  error
}

func (c *cappedBody) Read(p []byte) (int, error) {
	if c.left == 0 {
		return 0, c.err
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.rc.Read(p)
	c.left -= int64(n)
	if c.left == 0 {
		return n, c.err
	}
	return n, err
}

func (c *cappedBody) Close() error { return c.rc.Close() }
