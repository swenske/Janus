// Package releasetrust decides whether a release bundle may be installed
// on this node: it checks that a Unified Kernel Image carries an
// Authenticode signature from one of the release signing certificates
// built into this binary (certs/, a copy of image/secureboot/
// production-cert.pem - a test keeps the two identical).
//
// Checking the UKI is enough to authenticate the whole bundle: its
// signed .cmdline section holds the rootfs's dm-verity root hash, and the
// kernel verifies every block of rootfs.squashfs against it. This holds
// whatever the bundle's transport was - an http:// mirror, an https://
// download, a relay through the Controller, a local path.
//
// It's the same signature UEFI Secure Boot checks, verified here before
// anything is written, so it also protects nodes whose firmware doesn't
// enforce Secure Boot, and keeps a node that does from switching to a
// UKI its firmware would then refuse to boot (with no fallback left).
//
// github.com/foxboron/go-uefi parses the PE and computes its Authenticode
// hash. The trust decision itself is made here, every check an
// Authenticode verification needs spelled out explicitly below, so what
// janusd accepts doesn't depend on another library's verification policy.
package releasetrust

import (
	"bytes"
	"crypto"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"embed"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"fmt"
	"path"

	"github.com/foxboron/go-uefi/authenticode"
	"github.com/foxboron/go-uefi/efi/signature"
	"github.com/foxboron/go-uefi/pkcs7"
	"golang.org/x/crypto/cryptobyte"
	cbasn1 "golang.org/x/crypto/cryptobyte/asn1"
)

//go:embed certs/*.pem
var certFiles embed.FS

var (
	// ErrUnsigned is returned for a UKI with no signature at all.
	ErrUnsigned = errors.New("the UKI isn't signed")
	// ErrUntrusted is returned for a UKI whose signatures are all
	// invalid, or made with a key none of the trusted certificates
	// hold.
	ErrUntrusted = errors.New("the UKI isn't signed by a trusted release key")
)

var (
	oidSHA256              = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 1}
	oidRSAEncryption       = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 1}
	oidSHA256WithRSA       = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 11}
	oidSpcIndirectDataCont = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 311, 2, 1, 4}
)

// Trusted returns the release signing certificates built into this
// binary.
func Trusted() ([]*x509.Certificate, error) {
	entries, err := certFiles.ReadDir("certs")
	if err != nil {
		return nil, err
	}
	var certs []*x509.Certificate
	for _, e := range entries {
		data, err := certFiles.ReadFile(path.Join("certs", e.Name()))
		if err != nil {
			return nil, err
		}
		block, _ := pem.Decode(data)
		if block == nil || block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("%s: not a PEM certificate", e.Name())
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		certs = append(certs, c)
	}
	if len(certs) == 0 {
		return nil, errors.New("no release signing certificate built in")
	}
	return certs, nil
}

// VerifyUKI checks uki against the built-in release certificates.
func VerifyUKI(uki []byte) error {
	certs, err := Trusted()
	if err != nil {
		return err
	}
	return Verify(uki, certs)
}

// Verify checks that pe carries a valid Authenticode signature made with
// the key of one of trusted. Signatures are matched to a certificate by
// issuer and serial number, and only the trusted certificate's own public
// key is used - certificates embedded in the signature are ignored.
func Verify(pe []byte, trusted []*x509.Certificate) (err error) {
	// The PE and ASN.1 parsers are fed data from outside; a panic on a
	// malformed file must not take janusd down.
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%w: malformed signature or binary (%v)", ErrUntrusted, r)
		}
	}()

	bin, err := authenticode.Parse(bytes.NewReader(pe))
	if err != nil {
		return fmt.Errorf("%w: not a valid PE/COFF binary: %v", ErrUntrusted, err)
	}
	sigs, err := bin.Signatures()
	if err != nil {
		return fmt.Errorf("%w: read signatures: %v", ErrUntrusted, err)
	}
	if len(sigs) == 0 {
		return ErrUnsigned
	}
	imageDigest := bin.Hash(crypto.SHA256)

	var reasons []string
	for i, sig := range sigs {
		if err := verifySignature(sig, imageDigest, trusted); err != nil {
			reasons = append(reasons, fmt.Sprintf("signature %d: %v", i+1, err))
			continue
		}
		return nil
	}
	return fmt.Errorf("%w (%v)", ErrUntrusted, reasons)
}

func verifySignature(sig *signature.WINCertificate, imageDigest []byte, trusted []*x509.Certificate) error {
	if sig.CertType != signature.WIN_CERT_TYPE_PKCS_SIGNED_DATA {
		return fmt.Errorf("not a PKCS#7 signature (type %#x)", sig.CertType)
	}
	ac, err := authenticode.ParseAuthenticode(sig.Certificate)
	if err != nil {
		return err
	}

	// 1. The signed content names this image: its digest is the image's
	//    Authenticode hash.
	if ac.Algid == nil || !ac.Algid.Algorithm.Equal(oidSHA256) {
		return errors.New("image digest isn't SHA-256")
	}
	if !bytes.Equal(ac.Digest, imageDigest) {
		return errors.New("signed image digest doesn't match this binary")
	}

	// 2. The signed attributes cover that content: messageDigest is the
	//    SHA-256 of the SpcIndirectDataContent value (its contents,
	//    without the outer SEQUENCE header - Authenticode's own rule,
	//    confirmed against sbsign's output). It's what ties the image
	//    digest above to the signature: without it, the signed content
	//    could be swapped under a still-valid signature.
	content := cryptobyte.String(ac.Pkcs.ContentInfo)
	var inner cryptobyte.String
	if !content.ReadASN1(&inner, cbasn1.SEQUENCE) || !content.Empty() {
		return errors.New("malformed signed content")
	}
	contentDigest := sha256.Sum256(inner)

	// go-uefi's signerinfo type is unexported, but its fields aren't:
	// they're read here directly through the slice it hands out.
	lastErr := errors.New("no signer")
	for _, si := range ac.Pkcs.SignerInfo {
		// 3. The signer is one of the trusted certificates - by issuer
		//    and serial number, the only link PKCS#7 records.
		if si.IssuerAndSerialnumber == nil || si.IssuerAndSerialnumber.SerialNumber == nil {
			lastErr = errors.New("signer has no issuer/serial")
			continue
		}
		var cert *x509.Certificate
		for _, c := range trusted {
			if bytes.Equal(c.RawIssuer, si.IssuerAndSerialnumber.RawIssuer) && c.SerialNumber.Cmp(si.IssuerAndSerialnumber.SerialNumber) == 0 {
				cert = c
				break
			}
		}
		if cert == nil {
			lastErr = errors.New("signed by a certificate that isn't trusted")
			continue
		}

		// 4. The signed attributes vouch for this content.
		if si.DigestAlgorithm == nil || !si.DigestAlgorithm.Algorithm.Equal(oidSHA256) {
			lastErr = errors.New("signer digest algorithm isn't SHA-256")
			continue
		}
		attrs := si.AuthenticatedAttributes
		if attrs == nil || len(attrs.RawBytes) == 0 {
			lastErr = errors.New("no signed attributes")
			continue
		}
		if !attrs.ContentType.Equal(oidSpcIndirectDataCont) {
			lastErr = errors.New("signed contentType isn't SpcIndirectDataContent")
			continue
		}
		if !bytes.Equal(attrs.MessageDigest, contentDigest[:]) {
			lastErr = errors.New("signed messageDigest doesn't match the signed content")
			continue
		}

		// 5. The signature over those attributes is the trusted key's.
		if !isRSA(si.EncryptedDigestAlgorithm) {
			lastErr = errors.New("signature algorithm isn't RSA")
			continue
		}
		if err := cert.CheckSignature(x509.SHA256WithRSA, attrs.RawBytes, si.EncryptedDigest); err != nil {
			lastErr = fmt.Errorf("signature check: %w", err)
			continue
		}
		return nil
	}
	return lastErr
}

// isRSA accepts the two ways a signer records an RSA signature: the bare
// key algorithm (what sbsign writes) or sha256WithRSAEncryption.
func isRSA(alg *pkix.AlgorithmIdentifier) bool {
	return alg != nil && (alg.Algorithm.Equal(oidRSAEncryption) || alg.Algorithm.Equal(oidSHA256WithRSA))
}

// OIDAttributeMessageDigest is referenced so a go-uefi upgrade that
// renames its pkcs7 attribute handling breaks the build here, where the
// messageDigest check above depends on it being parsed.
var _ = pkcs7.OIDAttributeMessageDigest
