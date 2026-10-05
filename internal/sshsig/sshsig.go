// Package sshsig signs and checks OpenSSH's signatures of data (the
// SSHSIG format, PROTOCOL.sshsig: what `ssh-keygen -Y sign` makes) - how
// janusctl proves to the Controller it holds an SSH key of the account,
// with a namespace of its own, so the signature means nothing anywhere
// else (an SSH login, a git commit).
package sshsig

import (
	"bytes"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/ssh"
)

const (
	magic   = "SSHSIG"
	version = 1

	armorBegin = "-----BEGIN SSH SIGNATURE-----"
	armorEnd   = "-----END SSH SIGNATURE-----"
)

// blob is a signature, as PROTOCOL.sshsig lays it out after the magic.
type blob struct {
	Version   uint32
	PublicKey []byte
	Namespace string
	Reserved  string
	HashAlg   string
	Signature []byte
}

// signedData is what the key signs: the namespace and the message's hash.
type signedData struct {
	Namespace string
	Reserved  string
	HashAlg   string
	Hash      []byte
}

func digest(alg string, message []byte) ([]byte, error) {
	switch alg {
	case "sha512":
		h := sha512.Sum512(message)
		return h[:], nil
	case "sha256":
		h := sha256.Sum256(message)
		return h[:], nil
	}
	return nil, fmt.Errorf("unsupported hash algorithm %q", alg)
}

func toSign(namespace, alg string, message []byte) ([]byte, error) {
	h, err := digest(alg, message)
	if err != nil {
		return nil, err
	}
	return append([]byte(magic), ssh.Marshal(signedData{Namespace: namespace, HashAlg: alg, Hash: h})...), nil
}

// Sign signs message for namespace with signer, as ssh-keygen -Y sign
// would (SHA-512; an RSA key signs with rsa-sha2-512), armored.
func Sign(rand io.Reader, signer ssh.Signer, namespace string, message []byte) ([]byte, error) {
	data, err := toSign(namespace, "sha512", message)
	if err != nil {
		return nil, err
	}
	var sig *ssh.Signature
	if as, ok := signer.(ssh.AlgorithmSigner); ok && signer.PublicKey().Type() == ssh.KeyAlgoRSA {
		sig, err = as.SignWithAlgorithm(rand, data, ssh.KeyAlgoRSASHA512)
	} else {
		sig, err = signer.Sign(rand, data)
	}
	if err != nil {
		return nil, err
	}
	b := append([]byte(magic), ssh.Marshal(blob{
		Version:   version,
		PublicKey: signer.PublicKey().Marshal(),
		Namespace: namespace,
		HashAlg:   "sha512",
		Signature: ssh.Marshal(sig),
	})...)
	return armor(b), nil
}

func armor(b []byte) []byte {
	enc := base64.StdEncoding.EncodeToString(b)
	var out bytes.Buffer
	out.WriteString(armorBegin + "\n")
	for len(enc) > 70 {
		out.WriteString(enc[:70] + "\n")
		enc = enc[70:]
	}
	out.WriteString(enc + "\n" + armorEnd + "\n")
	return out.Bytes()
}

func unarmor(armored []byte) ([]byte, error) {
	s := strings.TrimSpace(string(armored))
	if !strings.HasPrefix(s, armorBegin) || !strings.HasSuffix(s, armorEnd) {
		return nil, errors.New("not an SSH signature")
	}
	s = strings.TrimSuffix(strings.TrimPrefix(s, armorBegin), armorEnd)
	return base64.StdEncoding.DecodeString(strings.Join(strings.Fields(s), ""))
}

// Verify checks that armored is pub's signature of message for
// namespace. A signature with SHA-1 (ssh-rsa) is refused.
func Verify(armored []byte, pub ssh.PublicKey, namespace string, message []byte) error {
	raw, err := unarmor(armored)
	if err != nil {
		return err
	}
	rest, ok := bytes.CutPrefix(raw, []byte(magic))
	if !ok {
		return errors.New("not an SSH signature")
	}
	var b blob
	if err := ssh.Unmarshal(rest, &b); err != nil {
		return fmt.Errorf("an SSH signature: %w", err)
	}
	if b.Version != version {
		return fmt.Errorf("SSH signature version %d", b.Version)
	}
	if b.Namespace != namespace {
		return fmt.Errorf("signed for %q, not %q", b.Namespace, namespace)
	}
	if !bytes.Equal(b.PublicKey, pub.Marshal()) {
		return errors.New("signed by another key")
	}
	var sig ssh.Signature
	if err := ssh.Unmarshal(b.Signature, &sig); err != nil {
		return fmt.Errorf("the signature: %w", err)
	}
	if sig.Format == ssh.KeyAlgoRSA {
		return errors.New("an RSA signature must be rsa-sha2-256 or rsa-sha2-512, not SHA-1")
	}
	data, err := toSign(namespace, b.HashAlg, message)
	if err != nil {
		return err
	}
	return pub.Verify(data, &sig)
}
