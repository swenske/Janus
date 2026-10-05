// Package pki is Janus's minimal internal certificate authority: one
// self-signed CA per node, used to issue the node's own gRPC server
// certificate and short-lived client certificates (see
// SystemService.GenerateClientConfiguration). Every certificate is
// ECDSA P-256 - was originally Ed25519 (small keys, fast, no
// padding-oracle history), switched after a real Proxmox deployment hit
// a browser dead end: the dashboard's own per-node view needs a real
// browser to both verify the server certificate and sign a TLS client
// certificate presented for mutual TLS (see dashboard/backend/internal/
// nodeproxy), and Chromium's TLS stack (Chrome/Brave/Edge) has a long,
// well-documented history of not supporting Ed25519 at all - general
// server-certificate support only landed around Chrome 137 (May 2025),
// and reliable client-certificate-authentication support is still far
// from guaranteed even now. P-256 has no such gap in any mainstream TLS
// stack, browser or otherwise - confirmed empirically (ERR_SSL_
// VERSION_OR_CIPHER_MISMATCH from a real Brave browser against the old
// Ed25519 dashboard identity cert, before this switch).
//
// Roles are carried in the leaf certificate's Subject.Organization field
// (the same idiom Kubernetes client-cert auth uses for group membership)
// so internal/api/authz.go reads them straight off the verified peer
// certificate without a custom X.509 extension: this package covers
// authentication (proving who you are), authz.go authorization (what
// that identity is allowed to do).
package pki

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"time"
)

const (
	caKeyPEMType  = "PRIVATE KEY"
	caCertPEMType = "CERTIFICATE"
	caValidity    = 10 * 365 * 24 * time.Hour // 10 years - this is the trust root, not meant to rotate casually
	// LeafValidity is a leaf certificate's lifetime unless
	// IssueOptions.Validity asks for less - 1 year: no renewal flow for
	// client certificates yet.
	LeafValidity   = 365 * 24 * time.Hour
	serialBitsSize = 128
)

// CA is a self-signed certificate authority and the private key that
// backs it.
type CA struct {
	Cert    *x509.Certificate
	CertPEM []byte
	Key     *ecdsa.PrivateKey
}

// NewCA generates a brand new, self-signed CA, valid 10 years.
func NewCA(commonName string) (*CA, error) {
	return NewCAFor(commonName, caValidity)
}

// NewCAFor generates a brand new, self-signed CA valid for validity.
func NewCAFor(commonName string, validity time.Duration) (*CA, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate CA key: %w", err)
	}
	pub := &priv.PublicKey

	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}

	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour), // small clock-skew margin
		NotAfter:              time.Now().Add(validity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return nil, fmt.Errorf("create CA certificate: %w", err)
	}

	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse CA certificate: %w", err)
	}

	return &CA{Cert: cert, CertPEM: encodePEM(caCertPEMType, der), Key: priv}, nil
}

// IssueCA signs a new subordinate CA - one that signs leaf certificates
// only, no CA of its own - valid for validity.
func (ca *CA) IssueCA(commonName string, validity time.Duration) (*CA, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate CA key: %w", err)
	}
	der, err := ca.issueCADER(&priv.PublicKey, commonName, validity)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse CA certificate: %w", err)
	}
	return &CA{Cert: cert, CertPEM: encodePEM(caCertPEMType, der), Key: priv}, nil
}

// IssueCAFor is IssueCA for a key made elsewhere (its public half, from
// a certificate request): the subordinate CA's certificate, PEM.
func (ca *CA) IssueCAFor(pub crypto.PublicKey, commonName string, validity time.Duration) ([]byte, error) {
	der, err := ca.issueCADER(pub, commonName, validity)
	if err != nil {
		return nil, err
	}
	return encodePEM(caCertPEMType, der), nil
}

func (ca *CA) issueCADER(pub crypto.PublicKey, commonName string, validity time.Duration) ([]byte, error) {
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(validity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, pub, ca.Key)
	if err != nil {
		return nil, fmt.Errorf("create CA certificate: %w", err)
	}
	return der, nil
}

// CrossSign vouches for next with this CA's key: a certificate of next's
// name and key, issued by this CA. A node serves it after its server
// certificate once it replaced its CA, so whoever pinned the old CA
// still verifies the node - and its new CA's client certificates, as the
// Controller does browsers' - and can pin the new one (ServerCert.Rotate).
// The node itself no longer trusts the old CA for anything.
func (ca *CA) CrossSign(next *CA) ([]byte, error) {
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               next.Cert.Subject,
		SubjectKeyId:          next.Cert.SubjectKeyId,
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              next.Cert.NotAfter,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		// No path length nor extended key usage, as the node's own CAs:
		// once a client pins it, the next replacement's cross-signed CA
		// comes below it, and the new CA's client certificates too.
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, next.Key.Public(), ca.Key)
	if err != nil {
		return nil, fmt.Errorf("cross-sign the new CA: %w", err)
	}
	return encodePEM(caCertPEMType, der), nil
}

// LoadCA parses a CA from its PEM-encoded certificate and private key
// (as previously written by CA.KeyPEM/CertPEM).
func LoadCA(certPEM, keyPEM []byte) (*CA, error) {
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return nil, fmt.Errorf("decode CA certificate PEM")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse CA certificate: %w", err)
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, fmt.Errorf("decode CA key PEM")
	}
	key, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse CA key: %w", err)
	}
	ecKey, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("CA key is not ECDSA")
	}

	return &CA{Cert: cert, CertPEM: certPEM, Key: ecKey}, nil
}

// KeyPEM returns the CA's private key, PKCS#8/PEM-encoded.
func (ca *CA) KeyPEM() ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(ca.Key)
	if err != nil {
		return nil, fmt.Errorf("marshal CA key: %w", err)
	}
	return encodePEM(caKeyPEMType, der), nil
}

// IssueOptions describes the leaf certificate CA.Issue should produce.
type IssueOptions struct {
	CommonName string
	// Roles is carried in the certificate's Subject.Organization - see
	// the package doc comment.
	Roles []string
	// DNSNames/IPAddresses are only meaningful for server certificates
	// (ExtKeyUsageServerAuth) - a pure client cert needs neither.
	DNSNames    []string
	IPAddresses []net.IP
	ExtKeyUsage []x509.ExtKeyUsage
	// Validity is how long the certificate is valid from now: 0 means
	// LeafValidity.
	Validity time.Duration
}

// Issue signs a new ECDSA P-256 leaf certificate with the CA's key,
// valid for opts.Validity (LeafValidity by default) from now. Returns
// (certPEM, keyPEM).
func (ca *CA) Issue(opts IssueOptions) (certPEM, keyPEM []byte, err error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate leaf key: %w", err)
	}
	certPEM, err = ca.IssueFor(&priv.PublicKey, opts)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal leaf key: %w", err)
	}
	return certPEM, encodePEM(caKeyPEMType, keyDER), nil
}

// IssueFor signs a leaf certificate for pub, whose private key stays
// with whoever holds it - as Issue does otherwise.
func (ca *CA) IssueFor(pub crypto.PublicKey, opts IssueOptions) ([]byte, error) {
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	validity := opts.Validity
	if validity == 0 {
		validity = LeafValidity
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   opts.CommonName,
			Organization: opts.Roles,
		},
		NotBefore:   time.Now().Add(-time.Hour),
		NotAfter:    time.Now().Add(validity),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: opts.ExtKeyUsage,
		DNSNames:    opts.DNSNames,
		IPAddresses: opts.IPAddresses,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, pub, ca.Key)
	if err != nil {
		return nil, fmt.Errorf("create leaf certificate: %w", err)
	}
	return encodePEM(caCertPEMType, der), nil
}

func randomSerial() (*big.Int, error) {
	max := new(big.Int).Lsh(big.NewInt(1), serialBitsSize)
	serial, err := rand.Int(rand.Reader, max)
	if err != nil {
		return nil, fmt.Errorf("generate serial number: %w", err)
	}
	return serial, nil
}

func encodePEM(blockType string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
}

// CheckIssued reports whether certPEM, a client certificate a node just
// issued, is the one asked for: named name (when not empty), valid no
// longer than validity (when not 0). A node older than these two
// requests ignores them, and would hand out a certificate named "client"
// valid for a year.
func CheckIssued(certPEM []byte, name string, validity time.Duration) error {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return fmt.Errorf("no certificate PEM in the node's answer")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("parse the issued certificate: %w", err)
	}
	if name != "" && cert.Subject.CommonName != name {
		return fmt.Errorf("the node named the certificate %q, not %q: it predates naming them - update it", cert.Subject.CommonName, name)
	}
	if validity != 0 && time.Until(cert.NotAfter) > validity+time.Minute {
		return fmt.Errorf("the node made the certificate valid until %s, longer than asked: it predates choosing the validity - update it", cert.NotAfter.UTC().Format(time.RFC3339))
	}
	return nil
}
