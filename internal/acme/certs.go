package acme

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"time"
)

// placeholderOrg marks the self-signed stand-in written before the CA's
// certificate is there.
const placeholderOrg = "Janus ACME placeholder"

// bundle is what a certificate file holds.
type bundle struct {
	Leaf        *x509.Certificate
	Placeholder bool
}

// parseBundle reads a certificate file: the chain, leaf first, then the
// private key.
func parseBundle(data []byte) (*bundle, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("no certificate")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}
	return &bundle{Leaf: leaf, Placeholder: slices.Contains(leaf.Subject.Organization, placeholderOrg) &&
		leaf.Subject.String() == leaf.Issuer.String()}, nil
}

// keyTypeOf is a certificate's key type, as configured ("ec256"...).
func keyTypeOf(leaf *x509.Certificate) string {
	switch pub := leaf.PublicKey.(type) {
	case *ecdsa.PublicKey:
		switch pub.Curve {
		case elliptic.P256():
			return "ec256"
		case elliptic.P384():
			return "ec384"
		}
	case *rsa.PublicKey:
		return fmt.Sprintf("rsa%d", pub.N.BitLen())
	}
	return ""
}

// sameNames reports whether a certificate covers exactly domains.
func sameNames(leaf *x509.Certificate, domains []string) bool {
	have := slices.Clone(leaf.DNSNames)
	want := slices.Clone(domains)
	for i := range have {
		have[i] = strings.ToLower(have[i])
	}
	slices.Sort(have)
	slices.Sort(want)
	return slices.Equal(slices.Compact(have), slices.Compact(want))
}

// newKey is a private key of keyType.
func newKey(keyType string) (crypto.Signer, error) {
	switch keyType {
	case "ec256":
		return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	case "ec384":
		return ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	case "rsa2048":
		return rsa.GenerateKey(rand.Reader, 2048)
	case "rsa3072":
		return rsa.GenerateKey(rand.Reader, 3072)
	case "rsa4096":
		return rsa.GenerateKey(rand.Reader, 4096)
	}
	return nil, fmt.Errorf("unknown key type %q", keyType)
}

// placeholder is a self-signed certificate for domains, with its key: the
// file's content until the CA's certificate arrives, so a configuration
// referencing it can already load.
func placeholder(domains []string, keyType string) ([]byte, error) {
	key, err := newKey(keyType)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 64))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: domains[0], Organization: []string{placeholderOrg}},
		DNSNames:     domains,
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(90 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})...), nil
}

// checkIssued checks what janus-acme brought back - a chain whose leaf
// covers domains, and its key - and returns the file's content.
func checkIssued(chain, key string, domains []string) ([]byte, *x509.Certificate, error) {
	var certs []*x509.Certificate
	var out []byte
	rest := []byte(chain)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, nil, fmt.Errorf("the issued chain: %w", err)
		}
		certs = append(certs, c)
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: block.Bytes})...)
	}
	if len(certs) == 0 {
		return nil, nil, errors.New("no certificate came back")
	}
	leaf := certs[0]
	for _, d := range domains {
		if !slices.ContainsFunc(leaf.DNSNames, func(n string) bool { return strings.EqualFold(n, d) }) {
			return nil, nil, fmt.Errorf("the issued certificate doesn't cover %s", d)
		}
	}
	block, _ := pem.Decode([]byte(key))
	if block == nil {
		return nil, nil, errors.New("no private key came back")
	}
	priv, err := parsePrivateKey(block)
	if err != nil {
		return nil, nil, err
	}
	if !publicKeysEqual(priv.Public(), leaf.PublicKey) {
		return nil, nil, errors.New("the issued certificate isn't for the key that came with it")
	}
	return append(out, pem.EncodeToMemory(block)...), leaf, nil
}

func parsePrivateKey(block *pem.Block) (crypto.Signer, error) {
	var key any
	var err error
	switch block.Type {
	case "EC PRIVATE KEY":
		key, err = x509.ParseECPrivateKey(block.Bytes)
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	default:
		key, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	}
	if err != nil {
		return nil, fmt.Errorf("the issued private key: %w", err)
	}
	s, ok := key.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("the issued private key: %T", key)
	}
	return s, nil
}

func publicKeysEqual(a, b crypto.PublicKey) bool {
	ka, ok := a.(interface{ Equal(crypto.PublicKey) bool })
	return ok && ka.Equal(b)
}

// renewAt is when a certificate becomes due by its lifetime: with a third
// of it left.
func renewAt(leaf *x509.Certificate) time.Time {
	life := leaf.NotAfter.Sub(leaf.NotBefore)
	return leaf.NotAfter.Add(-life / 3)
}

func serialHex(leaf *x509.Certificate) string {
	return strings.ToUpper(hex.EncodeToString(leaf.SerialNumber.Bytes()))
}
