package pki

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// The custom orchestrator example (examples/orchestrator) makes a fleet
// with openssl and jq alone: what it makes is what a node takes - its
// bundle, its client certificates within their roles, its registration
// endpoint's certificate. make qemu-orchestrator-test runs the same
// scripts against real nodes; this runs on every push.
func TestOrchestratorExampleCertificates(t *testing.T) {
	for _, tool := range []string{"openssl", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s isn't installed", tool)
		}
	}
	scripts, err := filepath.Abs("../../examples/orchestrator")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	run := func(script string, args ...string) {
		t.Helper()
		out, err := exec.Command(filepath.Join(scripts, script), args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v\n%s", script, args, err, out)
		}
	}
	read := func(name string) []byte {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	chain := func(name string) []*x509.Certificate {
		t.Helper()
		var certs []*x509.Certificate
		for rest := read(name); ; {
			var block *pem.Block
			if block, rest = pem.Decode(rest); block == nil {
				return certs
			}
			c, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				t.Fatal(err)
			}
			certs = append(certs, c)
		}
	}

	run("fleet.sh", filepath.Join(dir, "fleet"))
	rootPEM, bundle := read("fleet/root.crt"), read("fleet/bundle.json")
	b, err := CheckFleet(rootPEM, bundle)
	if err != nil {
		t.Fatalf("a node refuses fleet.sh's fleet: %v", err)
	}
	if b.Version != 1 || len(b.IssuingCAs) != 1 {
		t.Fatalf("bundle = version %d, %d issuing CAs", b.Version, len(b.IssuingCAs))
	}
	fleet, err := OpenFleet(filepath.Join(dir, "node"))
	if err != nil {
		t.Fatal(err)
	}
	if err := fleet.Set(rootPEM, bundle); err != nil {
		t.Fatalf("TrustSet with fleet.sh's fleet: %v", err)
	}
	root := chain("fleet/root.crt")[0]
	local, err := NewCA("a node")
	if err != nil {
		t.Fatal(err)
	}

	// The node's own check of a client: its TLS stack's, then the fleet's.
	accepts := func(name string) error {
		t.Helper()
		certs := chain(name)
		if len(certs) != 2 {
			t.Fatalf("%s holds %d certificates, not the certificate and its issuing CA", name, len(certs))
		}
		roots, inter := x509.NewCertPool(), x509.NewCertPool()
		roots.AddCert(root)
		inter.AddCert(certs[1])
		chains, err := certs[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
		if err != nil {
			return err
		}
		if !certs[0].NotBefore.Before(time.Now().Add(-4 * time.Minute)) {
			t.Errorf("%s starts at %s: a node a little behind refuses it", name, certs[0].NotBefore)
		}
		return fleet.AcceptChains(chains, local.Cert)
	}
	run("client-cert.sh", filepath.Join(dir, "fleet"), "orchestrator", RoleAdmin, filepath.Join(dir, "admin"))
	if err := accepts("admin.crt"); err != nil {
		t.Errorf("an os:admin certificate from client-cert.sh: %v", err)
	}
	run("client-cert.sh", filepath.Join(dir, "fleet"), "portal", RoleController, filepath.Join(dir, "portal"))
	if err := accepts("portal.crt"); err != nil {
		t.Errorf("a janus:controller certificate from client-cert.sh: %v", err)
	}

	// The registration endpoint's certificate, as a node checks it with
	// the fleet's root.
	run("registration-cert.sh", filepath.Join(dir, "fleet"), filepath.Join(dir, "registration"))
	reg := chain("registration.crt")
	roots, inter := x509.NewCertPool(), x509.NewCertPool()
	roots.AddCert(root)
	inter.AddCert(reg[1])
	if _, err := reg[0].Verify(x509.VerifyOptions{DNSName: FleetControllerName, Roots: roots, Intermediates: inter}); err != nil {
		t.Errorf("a node refuses registration-cert.sh's certificate: %v", err)
	}

	// A newer bundle is taken; the same version, made again, isn't.
	run("fleet.sh", filepath.Join(dir, "fleet"), "2")
	if err := fleet.Set(nil, read("fleet/bundle.json")); err != nil {
		t.Errorf("bundle version 2: %v", err)
	}
	run("fleet.sh", filepath.Join(dir, "fleet"), "2")
	if err := fleet.Set(nil, read("fleet/bundle.json")); err == nil {
		t.Error("another bundle version 2 was taken")
	}
}
