package releasetrust

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"debug/pe"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/foxboron/go-uefi/authenticode"
)

// minimalPE builds a tiny, valid PE32+ EFI application with one .text
// section holding payload - enough for Authenticode hashing and signing,
// without committing someone else's binary as a fixture.
func minimalPE(t *testing.T, payload []byte) []byte {
	t.Helper()
	const (
		fileAlign = 0x200
		sectAlign = 0x1000
		headers   = 0x200
	)
	if len(payload) > fileAlign {
		t.Fatal("payload too large")
	}
	b := make([]byte, headers+fileAlign)
	le := binary.LittleEndian
	copy(b, "MZ")
	le.PutUint32(b[0x3c:], 0x40)
	copy(b[0x40:], "PE\x00\x00")
	coff := 0x44
	le.PutUint16(b[coff:], 0x8664)  // AMD64
	le.PutUint16(b[coff+2:], 1)     // one section
	le.PutUint16(b[coff+16:], 240)  // optional header size
	le.PutUint16(b[coff+18:], 0x22) // executable, large address aware
	opt := coff + 20
	le.PutUint16(b[opt:], 0x20b) // PE32+
	le.PutUint32(b[opt+4:], fileAlign)
	le.PutUint32(b[opt+16:], sectAlign)   // entry point
	le.PutUint32(b[opt+20:], sectAlign)   // base of code
	le.PutUint64(b[opt+24:], 0x140000000) // image base
	le.PutUint32(b[opt+32:], sectAlign)   // section alignment
	le.PutUint32(b[opt+36:], fileAlign)   // file alignment
	le.PutUint32(b[opt+56:], 2*sectAlign) // size of image
	le.PutUint32(b[opt+60:], headers)     // size of headers
	le.PutUint16(b[opt+68:], 10)          // subsystem: EFI application
	le.PutUint32(b[opt+108:], 16)         // number of data directories
	sect := opt + 240
	copy(b[sect:], ".text")
	le.PutUint32(b[sect+8:], uint32(len(payload))) // virtual size
	le.PutUint32(b[sect+12:], sectAlign)           // virtual address
	le.PutUint32(b[sect+16:], fileAlign)           // raw size
	le.PutUint32(b[sect+20:], headers)             // raw pointer
	le.PutUint32(b[sect+36:], 0x60000020)          // code, execute, read
	copy(b[headers:], payload)

	if _, err := pe.NewFile(bytes.NewReader(b)); err != nil {
		t.Fatalf("minimalPE isn't a valid PE: %v", err)
	}
	return b
}

func newSigner(t *testing.T, cn string) (*rsa.PrivateKey, *x509.Certificate) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return key, cert
}

func sign(t *testing.T, img []byte, key *rsa.PrivateKey, cert *x509.Certificate) []byte {
	t.Helper()
	bin, err := authenticode.Parse(bytes.NewReader(img))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bin.Sign(key, cert); err != nil {
		t.Fatal(err)
	}
	return bin.Bytes()
}

func TestVerify(t *testing.T) {
	key, cert := newSigner(t, "Janus test release key")
	otherKey, otherCert := newSigner(t, "someone else")
	img := minimalPE(t, []byte("the real kernel"))
	signed := sign(t, img, key, cert)

	if err := Verify(signed, []*x509.Certificate{cert}); err != nil {
		t.Fatalf("validly signed: %v", err)
	}
	if err := Verify(signed, []*x509.Certificate{otherCert, cert}); err != nil {
		t.Fatalf("trusted among several certificates: %v", err)
	}
	if err := Verify(img, []*x509.Certificate{cert}); !errors.Is(err, ErrUnsigned) {
		t.Errorf("unsigned: %v, want ErrUnsigned", err)
	}
	if err := Verify(signed, []*x509.Certificate{otherCert}); !errors.Is(err, ErrUntrusted) {
		t.Errorf("signed by an untrusted key: %v, want ErrUntrusted", err)
	}
	if err := Verify(sign(t, img, otherKey, otherCert), []*x509.Certificate{cert}); !errors.Is(err, ErrUntrusted) {
		t.Errorf("signed by someone else: %v, want ErrUntrusted", err)
	}

	// Changing one byte of code after signing breaks the image digest.
	tampered := append([]byte(nil), signed...)
	tampered[0x200] ^= 0xff
	if err := Verify(tampered, []*x509.Certificate{cert}); !errors.Is(err, ErrUntrusted) {
		t.Errorf("tampered after signing: %v, want ErrUntrusted", err)
	}

	for _, junk := range [][]byte{nil, []byte("MZ"), bytes.Repeat([]byte{0xff}, 4096), signed[:len(signed)-7]} {
		if err := Verify(junk, []*x509.Certificate{cert}); err == nil {
			t.Errorf("junk of %d bytes verified", len(junk))
		}
	}
}

// TestSbsignInterop: signatures made by sbsign - what image/release/
// assemble.sh actually uses, through ukify - verify. Skipped without
// sbsign.
func TestSbsignInterop(t *testing.T) {
	sbsign, err := exec.LookPath("sbsign")
	if err != nil {
		t.Skip("sbsign not installed")
	}
	key, cert := newSigner(t, "Janus test release key")
	_, otherCert := newSigner(t, "someone else")
	dir := t.TempDir()
	keyPath, certPath := filepath.Join(dir, "key.pem"), filepath.Join(dir, "cert.pem")
	writePEM(t, keyPath, "PRIVATE KEY", mustPKCS8(t, key))
	writePEM(t, certPath, "CERTIFICATE", cert.Raw)
	in, out := filepath.Join(dir, "in.efi"), filepath.Join(dir, "out.efi")
	if err := os.WriteFile(in, minimalPE(t, []byte("a kernel")), 0o644); err != nil {
		t.Fatal(err)
	}
	if o, err := exec.Command(sbsign, "--key", keyPath, "--cert", certPath, "--output", out, in).CombinedOutput(); err != nil {
		t.Fatalf("sbsign: %v\n%s", err, o)
	}
	signed, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(signed, []*x509.Certificate{cert}); err != nil {
		t.Fatalf("sbsign-signed: %v", err)
	}
	if err := Verify(signed, []*x509.Certificate{otherCert}); !errors.Is(err, ErrUntrusted) {
		t.Fatalf("sbsign-signed, other cert trusted: %v", err)
	}
}

// TestBuiltInCertIsTheSigningCert: the certificate nodes trust must be
// the one CI signs releases with (image/secureboot/production-cert.pem,
// see image-build.yml) - otherwise every release would be refused.
func TestBuiltInCertIsTheSigningCert(t *testing.T) {
	data, err := os.ReadFile("../../image/secureboot/production-cert.pem")
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		t.Fatal("production-cert.pem isn't PEM")
	}
	trusted, err := Trusted()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range trusted {
		if bytes.Equal(c.Raw, block.Bytes) {
			return
		}
	}
	t.Fatal("image/secureboot/production-cert.pem isn't among internal/releasetrust/certs - copy it there when rotating the release key")
}

func mustPKCS8(t *testing.T, key *rsa.PrivateKey) []byte {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func writePEM(t *testing.T, path, typ string, der []byte) {
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}
