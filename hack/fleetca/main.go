// Command fleetca makes a throwaway fleet for tests (hack/qemu-fleet-
// trust-test.sh): a root, two issuing CAs, the bundles that list them,
// and client certificates of every kind a node must let in or not. Not
// for a real fleet: the Controller makes and keeps that one.
//
// Usage: go run ./hack/fleetca DIR
//
//	go run ./hack/fleetca sign CA.crt CA.key ROLE NAME OUT - a client
//	certificate an issuing CA signs, whatever role (OUT.crt, the chain,
//	and OUT.key): what a machine holding that CA's key could make without
//	janusctl, which keeps to the bundle's limit (hack/qemu-fleetctl-test.sh).
//
// DIR gets root.crt, stranger-root.crt (a CA of no fleet); bundle-1.json (lists issuing), bundle-2.json (lists
// other); and <name>.crt/<name>.key, each certificate followed by the CA
// that signed it when that's an issuing CA: admin, operator and
// controller (issuing), admin-other (other), admin-root (the root
// itself), admin-stranger (a CA of no fleet).
package main

import (
	"crypto/x509"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/swenske/Janus/internal/pki"
)

func main() {
	if len(os.Args) == 7 && os.Args[1] == "sign" {
		sign(os.Args[2], os.Args[3], os.Args[4], os.Args[5], os.Args[6])
		return
	}
	if len(os.Args) != 2 {
		log.Fatal("usage: fleetca DIR | fleetca sign CA.crt CA.key ROLE NAME OUT")
	}
	dir := os.Args[1]
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Fatal(err)
	}
	write := func(name string, data []byte) {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			log.Fatal(err)
		}
	}

	root := must(pki.NewCAFor("test fleet root", 20*365*24*time.Hour))
	issuing := must(root.IssueCA("test issuing CA", 2*365*24*time.Hour))
	other := must(root.IssueCA("another issuing CA", 2*365*24*time.Hour))
	stranger := must(pki.NewCA("a CA of no fleet"))
	write("root.crt", root.CertPEM)
	write("stranger-root.crt", stranger.CertPEM)

	for version, ca := range map[uint64]*pki.CA{1: issuing, 2: other} {
		signed, err := pki.SignBundle(root, pki.Bundle{Version: version, Issued: time.Now().UTC(), IssuingCAs: []string{string(ca.CertPEM)}})
		if err != nil {
			log.Fatal(err)
		}
		write(fmt.Sprintf("bundle-%d.json", version), signed)
	}

	for _, c := range []struct {
		name, role string
		ca         *pki.CA
		chain      bool
	}{
		{"admin", pki.RoleAdmin, issuing, true},
		{"operator", pki.RoleOperator, issuing, true},
		{"controller", pki.RoleController, issuing, true},
		{"admin-other", pki.RoleAdmin, other, true},
		{"admin-root", pki.RoleAdmin, root, false},
		{"admin-stranger", pki.RoleAdmin, stranger, false},
	} {
		certPEM, keyPEM, err := c.ca.Issue(pki.IssueOptions{CommonName: c.name, Roles: []string{c.role}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, Validity: 24 * time.Hour})
		if err != nil {
			log.Fatal(err)
		}
		if c.chain {
			certPEM = append(certPEM, c.ca.CertPEM...)
		}
		write(c.name+".crt", certPEM)
		write(c.name+".key", keyPEM)
	}
}

func must(ca *pki.CA, err error) *pki.CA {
	if err != nil {
		log.Fatal(err)
	}
	return ca
}

func sign(certFile, keyFile, role, name, out string) {
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		log.Fatal(err)
	}
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		log.Fatal(err)
	}
	ca := must(pki.LoadCA(certPEM, keyPEM))
	leaf, key, err := ca.Issue(pki.IssueOptions{CommonName: name, Roles: []string{role}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, Validity: time.Hour})
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(out+".crt", append(leaf, ca.CertPEM...), 0o600); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(out+".key", key, 0o600); err != nil {
		log.Fatal(err)
	}
}
