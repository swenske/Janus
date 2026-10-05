package pki

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

type testFleet struct {
	root, issuing, other *CA
}

func newTestFleet(t *testing.T) testFleet {
	t.Helper()
	root, err := NewCAFor("test fleet root", 20*365*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	issuing, err := root.IssueCA("test issuing CA", 2*365*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	other, err := root.IssueCA("another issuing CA", 2*365*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return testFleet{root, issuing, other}
}

func (tf testFleet) bundle(t *testing.T, version uint64, cas ...*CA) []byte {
	t.Helper()
	b := Bundle{Version: version, Issued: time.Now().UTC()}
	for _, ca := range cas {
		b.IssuingCAs = append(b.IssuingCAs, string(ca.CertPEM))
	}
	signed, err := SignBundle(tf.root, b)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

// TestFleetSet: a node pins its fleet's root once, then only takes a
// newer bundle that root signed - and keeps it across restarts.
func TestFleetSet(t *testing.T) {
	tf := newTestFleet(t)
	dir := t.TempDir()
	f, err := OpenFleet(dir)
	if err != nil || f.Root() != nil {
		t.Fatalf("empty: %v, %v", f.Root(), err)
	}
	if err := f.Set(nil, tf.bundle(t, 1, tf.issuing)); err == nil {
		t.Error("a bundle without a pinned root was taken")
	}
	if err := f.Set(tf.root.CertPEM, tf.bundle(t, 2, tf.issuing)); err != nil {
		t.Fatal(err)
	}
	if b, cas := f.Bundle(); b.Version != 2 || len(cas) != 1 || !cas[0].Equal(tf.issuing.Cert) {
		t.Fatalf("bundle %+v %v", b, cas)
	}

	again, err := OpenFleet(dir)
	if err != nil || !again.Root().Equal(tf.root.Cert) {
		t.Fatalf("reopened: %v, %v", again.Root(), err)
	}
	if b, _ := again.Bundle(); b == nil || b.Version != 2 {
		t.Fatalf("reopened bundle: %+v", b)
	}

	stranger := newTestFleet(t)
	for name, c := range map[string]struct {
		root, bundle []byte
		want         string
	}{
		"an older bundle":                {nil, tf.bundle(t, 1, tf.issuing), "older"},
		"another bundle, same version":   {nil, tf.bundle(t, 2, tf.other), "another bundle version 2"},
		"another root":                   {stranger.root.CertPEM, stranger.bundle(t, 3, stranger.issuing), "another fleet root"},
		"a bundle another root signed":   {nil, stranger.bundle(t, 3, tf.issuing), "isn't signed by the fleet's root"},
		"an issuing CA of another fleet": {nil, tf.bundle(t, 3, stranger.issuing), "isn't a CA the fleet's root signed"},
		"no bundle":                      {tf.root.CertPEM, nil, "no bundle"},
	} {
		if err := f.Set(c.root, c.bundle); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	if err := f.Set(tf.root.CertPEM, f.signed); err != nil {
		t.Errorf("the same bundle again: %v", err)
	}
	if err := f.Set(nil, tf.bundle(t, 3, tf.other)); err != nil {
		t.Errorf("a newer bundle: %v", err)
	}

	if err := f.Reset(); err != nil || f.Root() != nil {
		t.Fatalf("reset: %v, %v", f.Root(), err)
	}
	if again, err := OpenFleet(dir); err != nil || again.Root() != nil {
		t.Errorf("reopened after reset: %v, %v", again.Root(), err)
	}
}

// handshake connects to a NodeTLSConfig listener with client (leaf,
// then the CAs it sends along) and reports whether the node let it in.
func handshake(t *testing.T, server *tls.Config, client tls.Certificate, roots *x509.CertPool) error {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", server)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			done <- err
			return
		}
		defer c.Close()
		done <- c.(*tls.Conn).Handshake()
	}()
	conn, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{RootCAs: roots, ServerName: "localhost", Certificates: []tls.Certificate{client}, MinVersion: tls.VersionTLS13})
	if err == nil {
		// TLS 1.3: the server's verdict on the client certificate comes
		// with the first read.
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, err = conn.Read(make([]byte, 1))
		conn.Close()
	}
	if serr := <-done; serr != nil {
		return serr
	}
	if err != nil && !errors.Is(err, net.ErrClosed) && !strings.Contains(err.Error(), "EOF") {
		return err
	}
	return nil
}

func clientCert(t *testing.T, issuer *CA, chain ...*CA) tls.Certificate {
	t.Helper()
	certPEM, keyPEM, err := issuer.Issue(IssueOptions{CommonName: "client", Roles: []string{RoleAdmin}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	if err != nil {
		t.Fatal(err)
	}
	for _, ca := range chain {
		certPEM = append(certPEM, ca.CertPEM...)
	}
	c, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestNodeTLSConfig: who a node lets in - its own CA's certificates
// always; its fleet's once it trusts one, as long as their issuing CA is
// in the latest bundle, or the root signed them itself.
func TestNodeTLSConfig(t *testing.T) {
	tf := newTestFleet(t)
	local, err := NewCA("node CA")
	if err != nil {
		t.Fatal(err)
	}
	srvPEM, srvKey, err := local.Issue(IssueOptions{CommonName: "localhost", DNSNames: []string{"localhost"}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := tls.X509KeyPair(srvPEM, srvKey)
	if err != nil {
		t.Fatal(err)
	}
	fleet, err := OpenFleet(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := NodeTLSConfig(func() *CA { return local }, NewServerCert(local, t.TempDir(), srv), fleet)
	roots := local.CertPool()

	stranger, err := NewCA("stranger")
	if err != nil {
		t.Fatal(err)
	}
	certs := map[string]tls.Certificate{
		"local":          clientCert(t, local),
		"issuing":        clientCert(t, tf.issuing, tf.issuing),
		"other issuing":  clientCert(t, tf.other, tf.other),
		"root itself":    clientCert(t, tf.root),
		"stranger":       clientCert(t, stranger),
		"issuing, alone": clientCert(t, tf.issuing), // its CA not sent along
	}
	check := func(stage string, want map[string]bool) {
		t.Helper()
		for name, ok := range want {
			err := handshake(t, cfg, certs[name], roots)
			if (err == nil) != ok {
				t.Errorf("%s: %s: let in %v (%v), want %v", stage, name, err == nil, err, ok)
			}
		}
	}

	check("no fleet", map[string]bool{"local": true, "issuing": false, "root itself": false, "stranger": false})
	if err := fleet.Set(tf.root.CertPEM, tf.bundle(t, 1, tf.issuing)); err != nil {
		t.Fatal(err)
	}
	check("bundle 1", map[string]bool{"local": true, "issuing": true, "other issuing": false, "root itself": true, "stranger": false, "issuing, alone": false})
	if err := fleet.Set(nil, tf.bundle(t, 2, tf.other)); err != nil {
		t.Fatal(err)
	}
	check("bundle 2 drops the first issuing CA", map[string]bool{"local": true, "issuing": false, "other issuing": true, "root itself": true})
}

// TestProvisionFleet: a fleet put under a PKI directory before janusd
// starts is the one OpenFleet loads; a second one is refused, and so is
// a bundle another root signed.
func TestProvisionFleet(t *testing.T) {
	dir := t.TempDir()
	root, _ := NewCAFor("root", time.Hour)
	issuing, _ := root.IssueCA("issuing", time.Hour)
	signed, err := SignBundle(root, Bundle{Version: 4, Issued: time.Now(), IssuingCAs: []string{string(issuing.CertPEM)}})
	if err != nil {
		t.Fatal(err)
	}
	other, _ := NewCAFor("another root", time.Hour)
	if err := ProvisionFleet(dir, other.CertPEM, signed); err == nil {
		t.Error("a bundle another root signed was provisioned")
	}
	if err := ProvisionFleet(dir, root.CertPEM, signed); err != nil {
		t.Fatal(err)
	}
	f, err := OpenFleet(dir)
	if err != nil {
		t.Fatal(err)
	}
	if b, cas := f.Bundle(); f.Root() == nil || !f.Root().Equal(root.Cert) || b == nil || b.Version != 4 || len(cas) != 1 {
		t.Errorf("loaded: root %v, bundle %v", f.Root(), b)
	}
	if err := ProvisionFleet(dir, root.CertPEM, signed); err == nil {
		t.Error("a second fleet was provisioned")
	}
	// The same bundle, indented (as a user-data may carry it), is the
	// same bundle.
	var indented bytes.Buffer
	if err := json.Indent(&indented, signed, "", "  "); err != nil {
		t.Fatal(err)
	}
	if err := f.Set(nil, indented.Bytes()); err != nil {
		t.Errorf("the same bundle indented: %v", err)
	}
}
