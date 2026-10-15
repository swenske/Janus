// Package tlstrust builds the TLS client side of a connection to a
// server the operator names - a syslog server, an Elasticsearch cluster
// - trusted one of three ways:
//
//   - the system's trust store, by default;
//   - a CA given as PEM (an internal CA, the CA a Synology exports);
//   - a certificate's SHA-256 fingerprint: the server's own, or a CA's in
//     the chain it presents - what Elasticsearch prints at its first
//     start ("HTTP CA certificate SHA-256 fingerprint"), like Beats'
//     ca_trusted_fingerprint.
//
// A client certificate can be added for a server that asks for one.
package tlstrust

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Trust is how a server is trusted. CAPEM and Fingerprint exclude each
// other; with neither, the system's trust store.
type Trust struct {
	CAPEM       string `json:"ca_pem,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	// ServerName is the name the server's certificate must carry, when
	// it isn't the address's host.
	ServerName string `json:"server_name,omitempty"`
	// ClientCertPEM and ClientKeyPEM, both or neither: a client
	// certificate. The key is a secret.
	ClientCertPEM string `json:"client_cert_pem,omitempty"`
	ClientKeyPEM  string `json:"client_key_pem,omitempty"`
}

// Validate checks t without connecting anywhere.
func (t Trust) Validate() error {
	if t.CAPEM != "" && t.Fingerprint != "" {
		return errors.New("a CA and a fingerprint: one or the other")
	}
	if t.CAPEM != "" && !x509.NewCertPool().AppendCertsFromPEM([]byte(t.CAPEM)) {
		return errors.New("the CA isn't a PEM certificate")
	}
	if t.Fingerprint != "" {
		if _, err := NormalizeFingerprint(t.Fingerprint); err != nil {
			return err
		}
	}
	if (t.ClientCertPEM == "") != (t.ClientKeyPEM == "") {
		return errors.New("a client certificate needs its key, and the other way round")
	}
	if t.ClientCertPEM != "" {
		if _, err := tls.X509KeyPair([]byte(t.ClientCertPEM), []byte(t.ClientKeyPEM)); err != nil {
			return fmt.Errorf("the client certificate: %w", err)
		}
	}
	return nil
}

// Mode names how t trusts the server: "system", "ca" or "fingerprint".
func (t Trust) Mode() string {
	switch {
	case t.CAPEM != "":
		return "ca"
	case t.Fingerprint != "":
		return "fingerprint"
	}
	return "system"
}

// Config is the client configuration for a server reached as host.
func (t Trust) Config(host string) (*tls.Config, error) {
	if err := t.Validate(); err != nil {
		return nil, err
	}
	name := t.ServerName
	if name == "" {
		name = host
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: name}
	if t.ClientCertPEM != "" {
		cert, err := tls.X509KeyPair([]byte(t.ClientCertPEM), []byte(t.ClientKeyPEM))
		if err != nil {
			return nil, err
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	switch {
	case t.CAPEM != "":
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM([]byte(t.CAPEM))
		cfg.RootCAs = pool
	case t.Fingerprint != "":
		want, _ := NormalizeFingerprint(t.Fingerprint)
		// Go's own verification can't anchor on a fingerprint: it's done
		// here, on what the server presented.
		cfg.InsecureSkipVerify = true //nolint:gosec // G402: VerifyConnection below checks the chain against the pinned fingerprint
		cfg.VerifyConnection = func(cs tls.ConnectionState) error { return verifyPinned(cs.PeerCertificates, want, name) }
	}
	return cfg, nil
}

// verifyPinned accepts chain when its leaf is the pinned certificate, or
// when the leaf verifies for name up to a pinned CA in the chain.
func verifyPinned(chain []*x509.Certificate, want, name string) error {
	if len(chain) == 0 {
		return errors.New("the server presented no certificate")
	}
	for i, c := range chain {
		if Fingerprint(c.Raw) != want {
			continue
		}
		if i == 0 {
			return nil // the server's own certificate, pinned
		}
		roots := x509.NewCertPool()
		roots.AddCert(c)
		inter := x509.NewCertPool()
		for _, ic := range chain[1:i] {
			inter.AddCert(ic)
		}
		_, err := chain[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter, DNSName: name})
		return err
	}
	return fmt.Errorf("no certificate the server presented has the SHA-256 fingerprint %s (its own: %s)", want, Fingerprint(chain[0].Raw))
}

// Fingerprint is a certificate's SHA-256, lowercase hex.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// NormalizeFingerprint takes a SHA-256 fingerprint as people copy it -
// colons or not, any case - and gives it as Fingerprint does.
func NormalizeFingerprint(s string) (string, error) {
	h := strings.ToLower(strings.NewReplacer(":", "", " ", "").Replace(strings.TrimSpace(s)))
	if b, err := hex.DecodeString(h); err != nil || len(b) != sha256.Size {
		return "", fmt.Errorf("%q isn't a SHA-256 fingerprint (64 hex digits)", s)
	}
	return h, nil
}
