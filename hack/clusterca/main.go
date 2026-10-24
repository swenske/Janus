// Command clusterca is what an orchestrator does for a cluster's
// members, for the tests and for a hand-driven cluster (docs/cluster.md):
// makes a cluster's CA, and signs a node's signing request
// (SystemService.CertificateRequest) into its member certificate.
//
//	go run ./hack/clusterca new DIR NAME          # DIR/ca.crt, DIR/ca.key
//	go run ./hack/clusterca sign DIR CSR ID MEMBER OUT   # OUT: the certificate, the CA after it
package main

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/swenske/Janus/internal/cluster"
	"github.com/swenske/Janus/internal/pki"
)

func main() {
	log.SetFlags(0)
	switch {
	case len(os.Args) == 4 && os.Args[1] == "new":
		newCA(os.Args[2], os.Args[3])
	case len(os.Args) == 7 && os.Args[1] == "sign":
		sign(os.Args[2], os.Args[3], os.Args[4], os.Args[5], os.Args[6])
	default:
		log.Fatal("usage: clusterca new DIR NAME | clusterca sign DIR CSR CLUSTER-ID MEMBER OUT")
	}
}

func newCA(dir, name string) {
	ca, err := pki.NewCA("Janus cluster " + name)
	if err != nil {
		log.Fatal(err)
	}
	key, err := ca.KeyPEM()
	if err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Fatal(err)
	}
	for _, f := range []struct {
		name string
		data []byte
	}{{"ca.crt", ca.CertPEM}, {"ca.key", key}} {
		if err := os.WriteFile(filepath.Join(dir, f.name), f.data, 0o600); err != nil {
			log.Fatal(err)
		}
	}
	fmt.Println(pki.Fingerprint(ca.Cert.Raw))
}

func sign(dir, csrFile, id, member, out string) {
	certPEM, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		log.Fatal(err)
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, "ca.key"))
	if err != nil {
		log.Fatal(err)
	}
	ca, err := pki.LoadCA(certPEM, keyPEM)
	if err != nil {
		log.Fatal(err)
	}
	csrPEM, err := os.ReadFile(csrFile)
	if err != nil {
		log.Fatal(err)
	}
	block, _ := pem.Decode(csrPEM)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		log.Fatalf("%s: no certificate request", csrFile)
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		log.Fatal(err)
	}
	if err := csr.CheckSignature(); err != nil {
		log.Fatalf("%s: %v", csrFile, err)
	}
	u, err := url.Parse(cluster.MemberURI(id, member))
	if err != nil {
		log.Fatal(err)
	}
	cert, err := ca.IssueFor(csr.PublicKey, pki.IssueOptions{
		CommonName: member, Roles: []string{pki.RoleClusterMember}, DNSNames: []string{member}, URIs: []*url.URL{u},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}, Validity: 30 * 24 * time.Hour,
	})
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(out, append(cert, ca.CertPEM...), 0o600); err != nil {
		log.Fatal(err)
	}
}
