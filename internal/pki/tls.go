package pki

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
)

// CertPool returns an x509.CertPool containing just this CA - used both
// to verify client certificates (server side) and to verify the server's
// own certificate (client side, since it's signed by the same CA).
func (ca *CA) CertPool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)
	return pool
}

// ServerTLSConfig builds the tls.Config for janusd's gRPC listener:
// present serverCert, and require + verify a client certificate signed
// by this CA on every connection. There is no unauthenticated RPC - see
// docs/architecture.md.
func (ca *CA) ServerTLSConfig(serverCert tls.Certificate) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    ca.CertPool(),
		MinVersion:   tls.VersionTLS13,
	}
}

// ClientTLSConfig builds a tls.Config for janusctl (or any external
// caller) from PEM-encoded material only - no access to the CA's private
// key, just its certificate (to verify the server) and a previously
// issued client certificate/key pair (to authenticate as).
func ClientTLSConfig(caCertPEM, clientCertPEM, clientKeyPEM []byte) (*tls.Config, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caCertPEM) {
		return nil, fmt.Errorf("no valid certificates found in CA PEM")
	}

	clientCert, err := tls.X509KeyPair(clientCertPEM, clientKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("load client certificate/key: %w", err)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{clientCert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// NodeTLSConfig is janusd's listener: serverCert, and a client
// certificate required from the node's own CA or its fleet
// (Fleet.AcceptChains). Built for each connection, so a CA replaced or a
// bundle applied counts from the next one; local returns the node's
// current CA.
func NodeTLSConfig(local func() *CA, serverCert *ServerCert, fleet *Fleet) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			ca := local()
			pool := ca.CertPool()
			if root := fleet.Root(); root != nil {
				pool.AddCert(root)
			}
			return &tls.Config{
				MinVersion:     tls.VersionTLS13,
				NextProtos:     []string{"h2"}, // gRPC; credentials.NewTLS only adds it to the outer config
				GetCertificate: serverCert.GetCertificate,
				ClientAuth:     tls.RequireAndVerifyClientCert,
				ClientCAs:      pool,
				VerifyConnection: func(cs tls.ConnectionState) error {
					return fleet.AcceptChains(cs.VerifiedChains, ca.Cert)
				},
			}, nil
		},
	}
}

// Fingerprint is a certificate's SHA-256 (DER), in hex.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}
