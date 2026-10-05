package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/fleet"
	"github.com/swenske/Janus/dashboard/backend/internal/secrets"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
)

func csrFor(t *testing.T, key any) string {
	t.Helper()
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
}

// TestCLICertificate: janusctl gets a certificate of the fleet for its own
// key - an hour from a token, with the token's role; twelve from a
// session - and the nodes to use it with.
func TestCLICertificate(t *testing.T) {
	a := newAuthApp(t)
	root := a.login(t, "root", "root-password")
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	csr := csrFor(t, key)
	if code, body := a.req(t, "POST", "/api/cli/certificate", root, map[string]string{"csr_pem": csr}); code != http.StatusConflict {
		t.Errorf("before the fleet: %d %s", code, body)
	}

	dir := t.TempDir()
	mk, _ := secrets.LoadOrCreate("", dir)
	f, err := fleet.Open(filepath.Join(dir, "fleet"), mk, "0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	pass, _ := f.Setup()
	kit, _ := f.RecoveryKit()
	if err := f.Confirm(kit, pass); err != nil {
		t.Fatal(err)
	}
	a.fleet = f
	if err := a.store.Add(&store.Node{Name: "edge-1", Address: "192.0.2.5:9505", CACertPEM: []byte("node CA"), Fleet: true}); err != nil {
		t.Fatal(err)
	}

	issue := func(cred string, body map[string]string) (int, cliCertificate, string) {
		t.Helper()
		code, out := a.req(t, "POST", "/api/cli/certificate", cred, body)
		var c cliCertificate
		_ = json.Unmarshal([]byte(out), &c)
		return code, c, out
	}
	code, c, out := issue(root, map[string]string{"csr_pem": csr})
	if code != http.StatusOK || c.User != "root" || c.Role != "os:admin" || strings.Count(c.CertificatePEM, "BEGIN CERTIFICATE") != 2 {
		t.Fatalf("a session's: %d %s", code, out)
	}
	if d := time.Until(c.ExpiresAt); d < 11*time.Hour || d > 12*time.Hour {
		t.Errorf("a session's certificate is valid %v", d)
	}
	block, _ := pem.Decode([]byte(c.CertificatePEM))
	leaf, _ := x509.ParseCertificate(block.Bytes)
	if !leaf.PublicKey.(*ecdsa.PublicKey).Equal(&key.PublicKey) {
		t.Error("not for janusctl's key")
	}

	_, tok := a.tokenFor(t, root, "reader")
	code, c, out = issue(tok, map[string]string{"csr_pem": csr})
	if code != http.StatusOK || c.Role != "os:reader" {
		t.Fatalf("a reader token's: %d %s", code, out)
	}
	if d := time.Until(c.ExpiresAt); d > time.Hour {
		t.Errorf("a token's certificate is valid %v", d)
	}

	small, _ := rsa.GenerateKey(rand.Reader, 1024)
	for name, body := range map[string]map[string]string{
		"no CSR":       {},
		"a small RSA":  {"csr_pem": csrFor(t, small)},
		"not a CSR":    {"csr_pem": "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"},
		"a forged CSR": {"csr_pem": strings.Replace(csr, csr[100:104], "AAAA", 1)},
	} {
		if code, _, out := issue(root, body); code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, code, out)
		}
	}

	code, inv := a.req(t, "GET", "/api/cli/inventory", tok, nil)
	if code != http.StatusOK || !strings.Contains(inv, `"name":"edge-1"`) || !strings.Contains(inv, `"fleet":true`) || !strings.Contains(inv, `"ca_pem":"node CA"`) {
		t.Errorf("inventory: %d %s", code, inv)
	}
	if code, _ := a.req(t, "GET", "/api/cli/inventory", "", nil); code != http.StatusUnauthorized {
		t.Errorf("inventory, signed out: %d", code)
	}
}
