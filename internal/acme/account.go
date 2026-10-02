package acme

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
)

// parseAccountKey reads an account private key: PKCS#8, SEC 1 (EC) or
// PKCS#1 (RSA), ECDSA P-256/P-384 or RSA of 2048 bits or more - what ACME
// CAs accept.
func parseAccountKey(data []byte) (crypto.Signer, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("no PEM block in the account key")
	}
	var key any
	var err error
	switch block.Type {
	case "EC PRIVATE KEY":
		key, err = x509.ParseECPrivateKey(block.Bytes)
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		key, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	default:
		return nil, fmt.Errorf("the account key is a %q PEM block, not a private key", block.Type)
	}
	if err != nil {
		return nil, fmt.Errorf("account key: %w", err)
	}
	switch k := key.(type) {
	case *ecdsa.PrivateKey:
		if k.Curve != elliptic.P256() && k.Curve != elliptic.P384() {
			return nil, fmt.Errorf("account key: curve %s (P-256 or P-384)", k.Curve.Params().Name)
		}
		return k, nil
	case *rsa.PrivateKey:
		if k.N.BitLen() < 2048 {
			return nil, fmt.Errorf("account key: RSA %d bits (2048 or more)", k.N.BitLen())
		}
		return k, nil
	}
	return nil, fmt.Errorf("account key: %T (ECDSA or RSA)", key)
}

// newAccountKey is a fresh ECDSA P-256 key, PEM (SEC 1).
func newAccountKey() ([]byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}

// thumbprint is the JWK thumbprint (RFC 7638, SHA-256) of key's public
// key, base64url - the part after the dot of an HTTP-01 key
// authorization (RFC 8555 8.1).
func thumbprint(key crypto.Signer) (string, error) {
	var jwk string
	switch pub := key.Public().(type) {
	case *ecdsa.PublicKey:
		// Uncompressed: 0x04, then x and y, each the curve's size.
		raw, err := pub.Bytes()
		if err != nil {
			return "", err
		}
		size := (len(raw) - 1) / 2
		// The members in lexicographic order, no whitespace (RFC 7638 3.2).
		jwk = fmt.Sprintf(`{"crv":%q,"kty":"EC","x":%q,"y":%q}`, pub.Curve.Params().Name,
			b64(raw[1:1+size]), b64(raw[1+size:]))
	case *rsa.PublicKey:
		jwk = fmt.Sprintf(`{"e":%q,"kty":"RSA","n":%q}`, b64(big.NewInt(int64(pub.E)).Bytes()), b64(pub.N.Bytes()))
	default:
		return "", fmt.Errorf("%T", pub)
	}
	sum := sha256.Sum256([]byte(jwk))
	return b64(sum[:]), nil
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
