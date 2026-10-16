package tlstrust

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

type keyCert struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  string
}

func mint(t *testing.T, cn string, parent *keyCert, ca bool, names ...string) *keyCert {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames: names, IsCA: ca, BasicConstraintsValid: true,
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	signer, signerKey := tmpl, key
	if parent != nil {
		signer, signerKey = parent.cert, parent.key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signer, &key.PublicKey, signerKey)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return &keyCert{cert: c, key: key, pem: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
}

func (k *keyCert) keyPEM(t *testing.T) string {
	der, err := x509.MarshalECPrivateKey(k.key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))
}

// serve answers TLS handshakes with leaf (and its chain) until the test
// ends; it returns the address.
func serve(t *testing.T, leaf *keyCert, chain ...*keyCert) string {
	t.Helper()
	cert := tls.Certificate{Certificate: [][]byte{leaf.cert.Raw}, PrivateKey: leaf.key}
	for _, c := range chain {
		cert.Certificate = append(cert.Certificate, c.cert.Raw)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _ = c.(*tls.Conn).Handshake(); c.Close() }()
		}
	}()
	return ln.Addr().String()
}

func dial(t *testing.T, tr Trust, addr string) error {
	t.Helper()
	cfg, err := tr.Config("logs.example")
	if err != nil {
		return err
	}
	c, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", addr, cfg)
	if err == nil {
		c.Close()
	}
	return err
}

func TestTrustModes(t *testing.T) {
	ca := mint(t, "Logs CA", nil, true)
	leaf := mint(t, "logs", ca, false, "logs.example")
	other := mint(t, "Other CA", nil, true)
	addr := serve(t, leaf, ca)

	for _, c := range []struct {
		name string
		tr   Trust
		ok   bool
	}{
		{"the CA", Trust{CAPEM: ca.pem}, true},
		{"another CA", Trust{CAPEM: other.pem}, false},
		{"the CA's fingerprint", Trust{Fingerprint: Fingerprint(ca.cert.Raw)}, true},
		{"the leaf's fingerprint, colons and caps", Trust{Fingerprint: strings.ToUpper(colons(Fingerprint(leaf.cert.Raw)))}, true},
		{"another fingerprint", Trust{Fingerprint: Fingerprint(other.cert.Raw)}, false},
		{"the CA's fingerprint, another name", Trust{Fingerprint: Fingerprint(ca.cert.Raw), ServerName: "elsewhere.example"}, false},
		{"the system store", Trust{}, false},
	} {
		err := dial(t, c.tr, addr)
		if (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok %v", c.name, err, c.ok)
		}
	}
}

// A server that sends its own certificate alone (Logstash, rsyslog) is
// trusted by that certificate's fingerprint, not its CA's - and the
// refusal says which one to pin.
func TestLeafOnlyServer(t *testing.T) {
	ca := mint(t, "Logs CA", nil, true)
	leaf := mint(t, "logs", ca, false, "logs.example")
	addr := serve(t, leaf)
	if err := dial(t, Trust{Fingerprint: Fingerprint(leaf.cert.Raw)}, addr); err != nil {
		t.Errorf("the leaf's fingerprint: %v", err)
	}
	err := dial(t, Trust{Fingerprint: Fingerprint(ca.cert.Raw)}, addr)
	if err == nil || !strings.Contains(err.Error(), "only its own certificate") || !strings.Contains(err.Error(), Fingerprint(leaf.cert.Raw)) {
		t.Errorf("the CA's fingerprint: err = %v, want the leaf's fingerprint named", err)
	}
	if err := dial(t, Trust{CAPEM: ca.pem}, addr); err != nil {
		t.Errorf("the CA as a PEM: %v", err)
	}
}

func colons(h string) string {
	var parts []string
	for i := 0; i < len(h); i += 2 {
		parts = append(parts, h[i:i+2])
	}
	return strings.Join(parts, ":")
}

func TestValidate(t *testing.T) {
	ca := mint(t, "CA", nil, true)
	client := mint(t, "client", ca, false)
	for _, c := range []struct {
		tr Trust
		ok bool
	}{
		{Trust{}, true},
		{Trust{CAPEM: ca.pem}, true},
		{Trust{CAPEM: "not pem"}, false},
		{Trust{CAPEM: ca.pem, Fingerprint: Fingerprint(ca.cert.Raw)}, false},
		{Trust{Fingerprint: "abcd"}, false},
		{Trust{ClientCertPEM: client.pem}, false},
		{Trust{ClientCertPEM: client.pem, ClientKeyPEM: client.keyPEM(t)}, true},
		{Trust{ClientCertPEM: client.pem, ClientKeyPEM: ca.keyPEM(t)}, false},
	} {
		if err := c.tr.Validate(); (err == nil) != c.ok {
			t.Errorf("Validate(%+v) = %v", c.tr.Mode(), err)
		}
	}
}
