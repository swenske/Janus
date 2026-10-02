package client

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func unrelatedCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "unrelated"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func TestClient(t *testing.T) {
	phases := []string{"pending", "creating", "ready"}
	var gotAuth, gotBody string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/machines":
			var spec MachineSpec
			_ = json.NewDecoder(r.Body).Decode(&spec)
			raw, _ := json.Marshal(spec)
			gotBody = string(raw)
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(Machine{ID: "m1", Spec: spec, Phase: "pending"})
		case r.Method == "GET" && r.URL.Path == "/api/machines/m1":
			p := phases[0]
			if len(phases) > 1 {
				phases = phases[1:]
			}
			_ = json.NewEncoder(w).Encode(Machine{ID: "m1", Phase: p})
		case r.URL.Path == "/api/machines/gone":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no such machine"}`))
		default:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"nope"}`))
		}
	}))
	defer srv.Close()
	caPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))

	c, err := New(srv.URL, "janus_x_y", caPEM, false)
	if err != nil {
		t.Fatal(err)
	}
	PollInterval = time.Millisecond
	ctx := context.Background()
	m, err := c.CreateMachine(ctx, MachineSpec{Name: "lb1", NICs: []NIC{{Network: "lan", Name: "eth0", Mode: "dhcp"}}})
	if err != nil || m.ID != "m1" {
		t.Fatalf("CreateMachine: %+v, %v", m, err)
	}
	if gotAuth != "Bearer janus_x_y" || gotBody == "" {
		t.Errorf("auth %q body %q", gotAuth, gotBody)
	}
	m, err = c.WaitMachine(ctx, "m1", func(m *Machine) bool { return m.Phase == "ready" })
	if err != nil || m.Phase != "ready" {
		t.Fatalf("WaitMachine: %+v, %v", m, err)
	}
	if m, err := c.WaitMachine(ctx, "gone", func(*Machine) bool { return false }); m != nil || err != nil {
		t.Errorf("WaitMachine(gone) = %v, %v", m, err)
	}
	_, err = c.Machine(ctx, "gone")
	if !IsNotFound(err) || err.Error() != "Janus Controller: no such machine (HTTP 404)" {
		t.Errorf("404: %v", err)
	}
	if err := c.DeleteMachine(ctx, "x"); err == nil || IsNotFound(err) {
		t.Errorf("400 as %v", err)
	}

	// A server with an unrelated certificate (httptest reuses its own in
	// a process, so a real one): refused at the handshake.
	other := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(Machine{ID: "m1", Phase: "ready"})
	}))
	other.TLS = &tls.Config{Certificates: []tls.Certificate{unrelatedCert(t)}}
	other.StartTLS()
	defer other.Close()
	c2, _ := New(other.URL, "t", caPEM, false)
	if _, err := c2.Machine(ctx, "m1"); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Errorf("a server with another certificate: %v", err)
	}
	for _, bad := range []string{"http://x", "nope", ""} {
		if _, err := New(bad, "t", "", false); err == nil {
			t.Errorf("endpoint %q accepted", bad)
		}
	}
}
