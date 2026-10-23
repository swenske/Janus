package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swenske/Janus/dashboard/backend/internal/acmerelay"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
	"github.com/swenske/Janus/internal/pki"
)

// TestRelayNode: the relay knows a node by a certificate of the CA the
// Controller pins for it - not one of another CA, not a server
// certificate, not once the node is removed.
func TestRelayNode(t *testing.T) {
	a, _ := newTestApp(t)
	nodeCA, err := pki.NewCA("edge-1")
	if err != nil {
		t.Fatal(err)
	}
	n := &store.Node{Name: "edge-1", Address: "127.0.0.1:1", CACertPEM: nodeCA.CertPEM, Fleet: true}
	if err := a.store.Add(n); err != nil {
		t.Fatal(err)
	}
	if err := a.store.Add(&store.Node{Name: "edge-2", Address: "127.0.0.1:2", CACertPEM: []byte("not a CA"), Fleet: true}); err != nil {
		t.Fatal(err)
	}
	leaf := func(ca *pki.CA, eku x509.ExtKeyUsage) []*x509.Certificate {
		certPEM, _, err := ca.Issue(pki.IssueOptions{CommonName: "acme-relay", ExtKeyUsage: []x509.ExtKeyUsage{eku}})
		if err != nil {
			t.Fatal(err)
		}
		b, _ := pem.Decode(certPEM)
		c, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		return []*x509.Certificate{c}
	}
	if got, ok := a.relayNode(leaf(nodeCA, x509.ExtKeyUsageClientAuth)); !ok || got.ID != n.ID || got.Name != "edge-1" {
		t.Errorf("the node's own certificate: %+v %v", got, ok)
	}
	other, _ := pki.NewCA("stranger")
	if _, ok := a.relayNode(leaf(other, x509.ExtKeyUsageClientAuth)); ok {
		t.Error("a certificate of a CA the Controller doesn't pin")
	}
	if _, ok := a.relayNode(leaf(nodeCA, x509.ExtKeyUsageServerAuth)); ok {
		t.Error("a server certificate")
	}
	if _, ok := a.relayNode(nil); ok {
		t.Error("no certificate")
	}
	if err := a.store.Remove(n.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.relayNode(leaf(nodeCA, x509.ExtKeyUsageClientAuth)); ok {
		t.Error("a node removed")
	}
}

// TestRelaySettings: off at first; turned on, it listens and is saved;
// the address nodes are given follows the registration host; settings
// that are wrong change nothing.
func TestRelaySettings(t *testing.T) {
	a, _ := newTestApp(t)
	a.startACMERelay()
	t.Cleanup(a.relay.Close)
	get := func() relayView {
		rec := httptest.NewRecorder()
		a.handleACMERelay(rec, httptest.NewRequest(http.MethodGet, "/api/acme-relay", nil))
		var v relayView
		if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		return v
	}
	put := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		a.handleACMERelaySet(rec, httptest.NewRequest(http.MethodPut, "/api/acme-relay", strings.NewReader(body)))
		return rec
	}
	if v := get(); v.Config.Enabled || v.Listening != "" || v.URL != "https://192.0.2.1:8444" || !strings.Contains(v.CACertPEM, "BEGIN CERTIFICATE") {
		t.Errorf("at first: %+v", v)
	}
	if rec := put(`{"enabled": true, "listen": "127.0.0.1:0", "allowed": ["pebble.test:14000"]}`); rec.Code != http.StatusOK {
		t.Fatalf("turn it on: %d %s", rec.Code, rec.Body)
	}
	v := get()
	_, port, _ := net.SplitHostPort(v.Listening)
	if !v.Config.Enabled || v.Listening == "" || v.URL != "https://192.0.2.1:"+port || port == "0" || v.NotThroughRelay["route53"] == "" {
		t.Errorf("on: %+v", v)
	}
	saved, err := acmerelay.Load(filepath.Join(a.dataDir, acmeRelayFile))
	if err != nil || !saved.Enabled || len(saved.Allowed) != 1 {
		t.Errorf("saved: %+v %v", saved, err)
	}
	if fi, err := os.Stat(filepath.Join(a.dataDir, acmeRelayFile)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("the file: %v %v", fi.Mode(), err)
	}
	// The relay serves the Controller's identity.
	conn, err := tls.Dial("tcp", v.Listening, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // G402: only reading the certificate served
	if err == nil {
		got := conn.ConnectionState().PeerCertificates[0].Raw
		conn.Close()
		if string(got) != string(a.serverCert.Certificate[0]) {
			t.Error("the relay doesn't serve the Controller's identity")
		}
	}
	if rec := put(`{"enabled": true, "listen": "127.0.0.1:0", "address": "relay.example.net:8444"}`); rec.Code != http.StatusOK || get().URL != "https://relay.example.net:8444" {
		t.Errorf("an address of the admin's: %d %s", rec.Code, get().URL)
	}
	if rec := put(`{"enabled": true, "allowed": ["https://x.test/"]}`); rec.Code != http.StatusBadRequest {
		t.Errorf("a URL as a destination: %d", rec.Code)
	}
	if got := get(); got.Config.Address != "relay.example.net:8444" {
		t.Errorf("a refused change changed something: %+v", got.Config)
	}
	info := a.relayInfo()
	if !info.Enabled || !info.Listening || info.URL != "https://relay.example.net:8444" || info.BuiltIn["Let's Encrypt"] == nil {
		t.Errorf("what a node page gets: %+v", info)
	}
}
