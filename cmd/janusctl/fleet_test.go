package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/swenske/Janus/internal/pki"
)

func TestParseAnywhere(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	ep := fs.String("endpoint", "", "")
	kit := fs.Bool("yes", false, "")
	parseAnywhere(fs, []string{"edge-1", "-endpoint", "10.0.0.1", "-yes", "second"})
	if *ep != "10.0.0.1" || !*kit || !slices.Equal(fs.Args(), []string{"edge-1", "second"}) {
		t.Errorf("endpoint %q, yes %v, args %q", *ep, *kit, fs.Args())
	}
}

func TestBundleVersions(t *testing.T) {
	now := uint64(time.Now().UnixMilli())
	if v := nextVersion(1); v < now {
		t.Errorf("after 1: %d", v)
	}
	if v := nextVersion(now + 1000); v != now+1001 {
		t.Errorf("after one ahead of the clock: %d", v)
	}
	root, _ := pki.NewCAFor("root", time.Hour)
	issuing, _ := root.IssueCA(issuingNamePrefix+"a", time.Hour)
	a, _ := signBundle(root, 0, []*x509.Certificate{issuing.Cert})
	var indented strings.Builder
	var v any
	_ = json.Unmarshal(a, &v)
	out, _ := json.MarshalIndent(v, "", "  ")
	indented.Write(out)
	b, _ := signBundle(root, 0, []*x509.Certificate{issuing.Cert})
	if !sameBundle(a, []byte(indented.String())) || sameBundle(a, b) {
		t.Error("sameBundle")
	}
}

// TestLocalSignIn: the certificate a fleet context signs itself is one a
// node of the fleet lets in, with the context's name and role.
func TestLocalSignIn(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(configEnv, filepath.Join(dir, "janusctl.json"))
	root, _ := pki.NewCAFor("root", time.Hour*24*365)
	issuing, _ := root.IssueCA(issuingNamePrefix+"laptop", time.Hour*24*365)
	signed, _ := signBundle(root, 0, []*x509.Certificate{issuing.Cert})
	cfg, _ := loadConfig()
	c := &cliContext{User: "alice", Role: pki.RoleOperator, Fleet: &ctxFleet{Name: "lab", Issuer: "laptop"}}
	newFleetContext(cfg, "lab", c, root.CertPEM, issuing, signed)
	if time.Until(c.Expires) < fleetCertValidity-time.Minute {
		t.Errorf("expires %s", c.Expires)
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "lab", "cert.pem"), filepath.Join(dir, "lab", "key.pem"))
	if err != nil || len(cert.Certificate) != 2 {
		t.Fatalf("the context's certificate: %v", err)
	}
	leaf, _ := x509.ParseCertificate(cert.Certificate[0])
	if leaf.Subject.CommonName != "alice" || !slices.Equal(leaf.Subject.Organization, []string{pki.RoleOperator}) {
		t.Errorf("subject %v", leaf.Subject)
	}
	nodeFleet, _ := pki.OpenFleet(t.TempDir())
	if err := nodeFleet.Set(root.CertPEM, signed); err != nil {
		t.Fatal(err)
	}
	local, _ := pki.NewCA("node")
	roots := x509.NewCertPool()
	roots.AddCert(root.Cert)
	inter := x509.NewCertPool()
	inter.AddCert(issuing.Cert)
	chains, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	if err != nil {
		t.Fatal(err)
	}
	if err := nodeFleet.AcceptChains(chains, local.Cert); err != nil {
		t.Errorf("a node of the fleet refuses it: %v", err)
	}
	f, err := openLocalFleet("lab")
	if err != nil || issuerName(f.issuing.Cert) != "laptop" || f.bundle.Version == 0 {
		t.Errorf("the context's fleet: %v", err)
	}
}

// TestCheckNodeCA: a node is adopted on its CA's fingerprint only when
// its server certificate is that CA's.
func TestCheckNodeCA(t *testing.T) {
	ca, _ := pki.NewCA("node")
	other, _ := pki.NewCA("another node")
	serverPEM, _, err := ca.Issue(pki.IssueOptions{CommonName: "node", ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	if err != nil {
		t.Fatal(err)
	}
	server, _ := parsePEMCert(serverPEM)
	fp := pki.Fingerprint(ca.Cert.Raw)
	spaced := strings.ToUpper(fp[:8]) + " " + fp[8:]
	if err := checkNodeCA(ca.CertPEM, [][]byte{server.Raw}, spaced); err != nil {
		t.Errorf("the node's own: %v", err)
	}
	if err := checkNodeCA(ca.CertPEM, [][]byte{server.Raw}, pki.Fingerprint(other.Cert.Raw)); err == nil {
		t.Error("another fingerprint")
	}
	// A CA whose fingerprint matches, but a server certificate of another.
	otherServer, _, _ := other.Issue(pki.IssueOptions{CommonName: "x", ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	os, _ := parsePEMCert(otherServer)
	if err := checkNodeCA(ca.CertPEM, [][]byte{os.Raw}, fp); err == nil {
		t.Error("a server certificate of another CA")
	}
}
