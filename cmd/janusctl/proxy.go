package main

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"
)

// janusctl reaches the Controller and the nodes through the proxy the
// environment names, as curl and Go programs do: HTTPS_PROXY (or
// https_proxy), NO_PROXY for the hosts reached directly - loopback
// addresses always are. The HTTP calls to the Controller go through
// http.Transport's Proxy, the nodes' gRPC connections through grpc-go's
// own (an HTTP CONNECT), and the TLS handshakes login makes to read the
// Controller's certificate through controllerConn below. Claude Code's
// sandbox lets commands out only this way (docs/guide/claude-code.md).
//
// JANUS_NO_PROXY, when set, replaces NO_PROXY for janusctl: the sandbox
// sets NO_PROXY to the private ranges (10.0.0.0/8...), where its network
// namespace has no route - a node's private address must go through its
// proxy, JANUS_NO_PROXY=localhost in Claude Code's settings.

// noProxyEnv replaces NO_PROXY for janusctl when set.
const noProxyEnv = "JANUS_NO_PROXY"

// applyNoProxy puts JANUS_NO_PROXY in NO_PROXY's place - before anything
// reads the proxy settings (net/http and grpc-go read them once).
func applyNoProxy() {
	if v, ok := os.LookupEnv(noProxyEnv); ok {
		_ = os.Setenv("NO_PROXY", v)
		_ = os.Setenv("no_proxy", v)
	}
}

// proxyFor is the proxy for a connection to addr (host:port) over TLS,
// nil for none - http.ProxyFromEnvironment's answer.
func proxyFor(addr string) (*url.URL, error) {
	return http.ProxyFromEnvironment(&http.Request{URL: &url.URL{Scheme: "https", Host: addr}})
}

// controllerConn is a TCP connection to the Controller at addr, through
// the proxy when the environment names one for it.
func controllerConn(addr string, timeout time.Duration) (net.Conn, error) {
	d := &net.Dialer{Timeout: timeout}
	proxy, err := proxyFor(addr)
	if err != nil {
		return nil, fmt.Errorf("the proxy for %s: %w", addr, err)
	}
	if proxy == nil {
		return d.Dial("tcp", addr)
	}
	return connectThrough(d, proxy, addr, timeout)
}

// connectThrough opens a tunnel to addr through an http:// or https://
// proxy: CONNECT, its credentials from the URL.
func connectThrough(d *net.Dialer, proxy *url.URL, addr string, timeout time.Duration) (net.Conn, error) {
	port := proxy.Port()
	switch proxy.Scheme {
	case "http":
		if port == "" {
			port = "80"
		}
	case "https":
		if port == "" {
			port = "443"
		}
	default:
		return nil, fmt.Errorf("the proxy %s: janusctl reaches the Controller through an http:// or https:// proxy only", proxy.Redacted())
	}
	conn, err := d.Dial("tcp", net.JoinHostPort(proxy.Hostname(), port))
	if err != nil {
		return nil, fmt.Errorf("reach the proxy %s: %w", proxy.Redacted(), err)
	}
	if proxy.Scheme == "https" {
		conn = tls.Client(conn, &tls.Config{ServerName: proxy.Hostname(), MinVersion: tls.VersionTLS12})
	}
	_ = conn.SetDeadline(time.Now().Add(timeout))
	req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: addr}, Host: addr, Header: http.Header{}}
	if u := proxy.User; u != nil {
		password, _ := u.Password()
		req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(u.Username()+":"+password)))
	}
	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("the proxy %s: %w", proxy.Redacted(), err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("the proxy %s: %w", proxy.Redacted(), err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		conn.Close()
		return nil, fmt.Errorf("the proxy %s refused %s: %s", proxy.Redacted(), addr, resp.Status)
	}
	if br.Buffered() > 0 {
		conn.Close()
		return nil, fmt.Errorf("the proxy %s sent more than its answer to CONNECT", proxy.Redacted())
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

// tlsToController is a TLS connection to the Controller at addr, through
// the proxy as above, its handshake done.
func tlsToController(addr string, cfg *tls.Config) (*tls.Conn, error) {
	const timeout = 10 * time.Second
	raw, err := controllerConn(addr, timeout)
	if err != nil {
		return nil, err
	}
	conn := tls.Client(raw, cfg)
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if err := conn.Handshake(); err != nil {
		raw.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}
