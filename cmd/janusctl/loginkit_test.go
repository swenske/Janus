package main

import (
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/swenske/Janus/internal/fleetkit"
	"github.com/swenske/Janus/internal/pki"
)

// TestLoginKit: with the Controller down, the fleet's recovery kit signs
// a context a certificate for an hour - for the context's own key, named
// kit:<account>, with the role asked - that a node of the fleet lets in,
// without its bundle listing anything new; fresh() never asks the
// Controller to renew it; another fleet's kit is refused.
func TestLoginKit(t *testing.T) {
	fleetkit.SetWorkFactorForTests(10)
	dir := t.TempDir()
	t.Setenv(configEnv, filepath.Join(dir, "janusctl.json"))
	root, _ := pki.NewCAFor("root", 24*time.Hour)
	issuing, _ := root.IssueCA(issuingNamePrefix+"controller", 24*time.Hour)
	bundle, _ := signBundle(root, 0, []*x509.Certificate{issuing.Cert}, nil)

	// The context as a sign-in left it: a key made here, the Controller's
	// certificate (ended), its nodes.
	key, err := contextKey(contextDir("ops"))
	if err != nil {
		t.Fatal(err)
	}
	old, err := issuing.IssueFor(key.Public(), pki.IssueOptions{CommonName: "alice", Roles: []string{pki.RoleOperator}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, Validity: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	certFile := filepath.Join(contextDir("ops"), "cert.pem")
	if err := writeFileAtomic(certFile, append(old, issuing.CertPEM...), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := &cliContext{Controller: "ctl.example:443", User: "alice", Role: "operator", Expires: time.Now().Add(-time.Hour), Nodes: []ctxNode{{ID: "n1", Name: "edge-1", Address: "192.0.2.10:9505", Fleet: true}}}

	kitFile := filepath.Join(dir, "kit.age")
	rootKey, _ := root.KeyPEM()
	kit, err := fleetkit.Make(fleetkit.Kit{Created: time.Now().UTC(), RootCert: string(root.CertPEM), RootKey: string(rootKey)}, "ABCD")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kitFile, kit, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(kitPassphraseEnv, "ABCD")

	if err := loginKit("ops", ctx, kitFile, "admin"); err == nil || !strings.Contains(err.Error(), "os:admin, os:operator or os:reader") {
		t.Errorf("a role that isn't one: %v", err)
	}
	if err := loginKit("ops", ctx, kitFile, pki.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if !ctx.Kit || ctx.Role != pki.RoleAdmin || time.Until(ctx.Expires) < kitCertValidity-time.Minute || time.Until(ctx.Expires) > kitCertValidity {
		t.Errorf("context: kit %v, role %q, expires %s", ctx.Kit, ctx.Role, ctx.Expires)
	}
	cert, err := tls.LoadX509KeyPair(certFile, filepath.Join(contextDir("ops"), "key.pem"))
	if err != nil {
		t.Fatalf("the certificate isn't for the context's key: %v", err)
	}
	leaf, _ := x509.ParseCertificate(cert.Certificate[0])
	if leaf.Subject.CommonName != "kit:alice" || !slices.Equal(leaf.Subject.Organization, []string{pki.RoleAdmin}) {
		t.Errorf("subject %v", leaf.Subject)
	}

	// A node of the fleet - its bundle the Controller's - lets it in.
	nodeFleet, _ := pki.OpenFleet(t.TempDir())
	if err := nodeFleet.Set(root.CertPEM, bundle); err != nil {
		t.Fatal(err)
	}
	local, _ := pki.NewCA("node")
	roots := x509.NewCertPool()
	roots.AddCert(root.Cert)
	chains, err := leaf.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	if err != nil {
		t.Fatal(err)
	}
	if err := nodeFleet.AcceptChains(chains, local.Cert); err != nil {
		t.Errorf("a node of the fleet refuses it: %v", err)
	}

	// Ending, it isn't renewed by asking the Controller.
	ctx.Expires = time.Now().Add(time.Minute)
	cfg, _ := loadConfig()
	if err := fresh(cfg, "ops", ctx); err == nil || !strings.Contains(err.Error(), "janusctl login -kit KIT again") {
		t.Errorf("fresh: %v", err)
	}

	// Again with the kit: the context's certificate is the root's now.
	if err := loginKit("ops", ctx, kitFile, pki.RoleReader); err != nil || ctx.Role != pki.RoleReader {
		t.Errorf("again: %v %q", err, ctx.Role)
	}

	// Another fleet's kit.
	other, _ := pki.NewCAFor("other", time.Hour)
	otherKey, _ := other.KeyPEM()
	otherKit, _ := fleetkit.Make(fleetkit.Kit{Created: time.Now().UTC(), RootCert: string(other.CertPEM), RootKey: string(otherKey)}, "ABCD")
	otherFile := filepath.Join(dir, "other.age")
	_ = os.WriteFile(otherFile, otherKit, 0o600)
	if err := loginKit("ops", ctx, otherFile, pki.RoleAdmin); err == nil || !strings.Contains(err.Error(), "another fleet's") {
		t.Errorf("another fleet's kit: %v", err)
	}

	// No node known: nothing to reach.
	if err := loginKit("ops", &cliContext{Controller: "ctl.example:443"}, kitFile, pki.RoleAdmin); err == nil || !strings.Contains(err.Error(), "knows no node") {
		t.Errorf("a context without nodes: %v", err)
	}
}
