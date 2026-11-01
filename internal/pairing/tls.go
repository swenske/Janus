package pairing

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/pki"
)

// DefaultPort is the pairing window's port, next to the API's 9505.
const DefaultPort = "9506"

// leafValidity: a window lasts until it's closed or the node restarts.
const leafValidity = 365 * 24 * time.Hour

// NewLeaf is a window's certificate: a key made for the window, signed
// by the node's CA - served with the CA, which a client pins once the
// code named the certificate.
func NewLeaf(ca *pki.CA, hostname string, ips []net.IP) (tls.Certificate, error) {
	certPEM, keyPEM, err := ca.Issue(pki.IssueOptions{
		CommonName: "pairing " + hostname, DNSNames: []string{hostname}, IPAddresses: ips,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, Validity: leafValidity,
	})
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.X509KeyPair(append(certPEM, ca.CertPEM...), keyPEM)
}

// ServerTLS is the window's TLS: its certificate, no client certificate
// asked - the secret, checked by Pair, is the client's proof.
func ServerTLS(leaf tls.Certificate) *tls.Config {
	return &tls.Config{Certificates: []tls.Certificate{leaf}, ClientAuth: tls.NoClientCert, MinVersion: tls.VersionTLS13}
}

// verifier checks a window's chain against a code - the leaf the code
// names, signed by the CA that follows it - and keeps that CA.
type verifier struct {
	code Code
	mu   sync.Mutex
	ca   *x509.Certificate
}

func (v *verifier) verify(raw [][]byte, _ [][]*x509.Certificate) error {
	if len(raw) < 2 {
		return errors.New("the node presents no CA with its pairing certificate")
	}
	if !v.code.Names(raw[0]) {
		return errors.New("the node's pairing certificate isn't the one its code names - another code, or not that node")
	}
	leaf, err := x509.ParseCertificate(raw[0])
	if err != nil {
		return err
	}
	ca, err := x509.ParseCertificate(raw[1])
	if err != nil {
		return err
	}
	// Times aren't checked: a node's first boot may have no clock yet.
	if !ca.IsCA || leaf.CheckSignatureFrom(ca) != nil || ca.CheckSignatureFrom(ca) != nil {
		return errors.New("the node's pairing certificate isn't signed by its own CA")
	}
	v.mu.Lock()
	v.ca = ca
	v.mu.Unlock()
	return nil
}

// Pair gives the node at addr - its pairing window, host:9506 - the
// fleet of rootPEM and bundle, the window checked against code before
// anything is sent. It returns the node's answer and its CA, verified:
// what a client pins to reach the node's API from then on.
func Pair(ctx context.Context, addr, code string, rootPEM, bundle []byte, opts ...grpc.DialOption) (*janusv1alpha1.PairResponse, *x509.Certificate, error) {
	c, err := ParseCode(code)
	if err != nil {
		return nil, nil, err
	}
	v := &verifier{code: c}
	creds := credentials.NewTLS(&tls.Config{
		MinVersion:            tls.VersionTLS13,
		InsecureSkipVerify:    true,     //nolint:gosec // the code names the certificate: verify checks it, and its CA, before anything is sent
		VerifyPeerCertificate: v.verify, //nolint:gosec // G123: no ClientSessionCache, so no resumption - every connection presents its chain
	})
	conn, err := grpc.NewClient(addr, append([]grpc.DialOption{grpc.WithTransportCredentials(creds)}, opts...)...)
	if err != nil {
		return nil, nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	defer conn.Close()
	resp, err := janusv1alpha1.NewPairingServiceClient(conn).Pair(ctx, &janusv1alpha1.PairRequest{Secret: c.SecretBytes(), RootCert: rootPEM, Bundle: bundle})
	if err != nil {
		return nil, nil, err
	}
	v.mu.Lock()
	ca := v.ca
	v.mu.Unlock()
	if ca == nil {
		return nil, nil, errors.New("the node's pairing certificate wasn't checked")
	}
	if got, err := parsePEM(resp.GetCaCert()); err != nil || !got.Equal(ca) {
		return nil, nil, errors.New("the node answered with another CA than its pairing certificate's")
	}
	return resp, ca, nil
}

func parsePEM(p []byte) (*x509.Certificate, error) {
	b, _ := pem.Decode(p)
	if b == nil || b.Type != "CERTIFICATE" {
		return nil, errors.New("no PEM certificate")
	}
	return x509.ParseCertificate(b.Bytes)
}
