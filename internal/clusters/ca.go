package clusters

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/swenske/Janus/internal/cluster"
	"github.com/swenske/Janus/internal/pki"
)

// The cluster's own CA (docs/cluster.md "Trust between members"): it signs
// the members' certificates, which HAProxy's peers listeners verify - a
// CA per cluster, since a listener checks a CA and not a name. Its key is
// sealed with the Controller's master key; janusctl's (no Sealer) is kept
// as its fleet's issuing key is, 0600 in the context's directory.

const (
	caCertFile     = "ca.crt"
	caKeyFile      = "ca.key.sealed"
	caKeyPlainFile = "ca.key"

	// MemberValidity is a member certificate's lifetime; the reconcile
	// loop renews it past two thirds.
	MemberValidity = 30 * 24 * time.Hour
)

func caPurpose(id string) string { return "cluster/" + id + "/ca" }

// createCA makes c's CA. Called with mu held.
func (s *Store) createCA(c *Cluster) error {
	ca, err := pki.NewCA("Janus cluster " + c.Name)
	if err != nil {
		return err
	}
	keyPEM, err := ca.KeyPEM()
	if err != nil {
		return err
	}
	dir := s.clusterDir(c.ID)
	if s.key == nil {
		if err := writeDurable(filepath.Join(dir, caCertFile), ca.CertPEM); err != nil {
			return err
		}
		return writeDurable(filepath.Join(dir, caKeyPlainFile), keyPEM)
	}
	sealed, err := s.key.Seal(keyPEM, caPurpose(c.ID))
	if err != nil {
		return fmt.Errorf("seal the cluster's CA key: %w", err)
	}
	if err := writeDurable(filepath.Join(dir, caCertFile), ca.CertPEM); err != nil {
		return err
	}
	return writeDurable(filepath.Join(dir, caKeyFile), sealed)
}

// CA is cluster id's CA, its key opened.
func (s *Store) CA(id string) (*pki.CA, error) {
	dir := s.clusterDir(id)
	certPEM, err := os.ReadFile(filepath.Join(dir, caCertFile))
	if err != nil {
		return nil, err
	}
	if s.key == nil {
		keyPEM, err := os.ReadFile(filepath.Join(dir, caKeyPlainFile))
		if err != nil {
			return nil, err
		}
		return pki.LoadCA(certPEM, keyPEM)
	}
	sealed, err := os.ReadFile(filepath.Join(dir, caKeyFile))
	if err != nil {
		return nil, err
	}
	keyPEM, err := s.key.Open(sealed, caPurpose(id))
	if err != nil {
		return nil, fmt.Errorf("the cluster's CA key: %w - is this the master key it was sealed with?", err)
	}
	return pki.LoadCA(certPEM, keyPEM)
}

// CACertPEM is cluster id's CA certificate.
func (s *Store) CACertPEM(id string) ([]byte, error) {
	return os.ReadFile(filepath.Join(s.clusterDir(id), caCertFile))
}

// SignMember signs a member's request (SystemService.CertificateRequest)
// into its member certificate, followed by the CA: O=janus:cluster, the
// member's name as its DNS name (the peers check it), and the cluster's
// URI.
func SignMember(ca *pki.CA, csrPEM []byte, clusterID, member string) ([]byte, error) {
	block, _ := pem.Decode(csrPEM)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, errors.New("the node's answer isn't a PEM certificate request")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, err
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, errors.New("the certificate request isn't signed by its own key")
	}
	k, ok := csr.PublicKey.(*ecdsa.PublicKey)
	if !ok || (k.Curve != elliptic.P256() && k.Curve != elliptic.P384()) {
		return nil, errors.New("a member's key is ECDSA P-256 or P-384")
	}
	u, err := url.Parse(cluster.MemberURI(clusterID, member))
	if err != nil {
		return nil, err
	}
	cert, err := ca.IssueFor(csr.PublicKey, pki.IssueOptions{
		CommonName: member, Roles: []string{pki.RoleClusterMember}, DNSNames: []string{member}, URIs: []*url.URL{u},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}, Validity: MemberValidity,
	})
	if err != nil {
		return nil, err
	}
	return append(cert, ca.CertPEM...), nil
}
