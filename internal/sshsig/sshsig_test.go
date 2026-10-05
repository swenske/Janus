package sshsig

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func keys(t *testing.T) map[string]any {
	t.Helper()
	_, ed, _ := ed25519.GenerateKey(rand.Reader)
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	rs, _ := rsa.GenerateKey(rand.Reader, 2048)
	return map[string]any{"ed25519": ed, "ecdsa": ec, "rsa": rs}
}

func TestSignVerify(t *testing.T) {
	msg := []byte("a challenge")
	for name, k := range keys(t) {
		signer, err := ssh.NewSignerFromKey(k)
		if err != nil {
			t.Fatal(err)
		}
		sig, err := Sign(rand.Reader, signer, "janus-login", msg)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := Verify(sig, signer.PublicKey(), "janus-login", msg); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if err := Verify(sig, signer.PublicKey(), "git", msg); err == nil {
			t.Errorf("%s: verified for another namespace", name)
		}
		if err := Verify(sig, signer.PublicKey(), "janus-login", []byte("another challenge")); err == nil {
			t.Errorf("%s: verified for another message", name)
		}
		other, _ := ssh.NewSignerFromKey(keys(t)[name])
		if err := Verify(sig, other.PublicKey(), "janus-login", msg); err == nil {
			t.Errorf("%s: verified with another key", name)
		}
	}
	if err := Verify([]byte("hello"), nil, "janus-login", msg); err == nil {
		t.Error("not a signature")
	}
}

// TestOpenSSH: ssh-keygen -Y checks what Sign makes, and Verify checks
// what ssh-keygen -Y sign makes.
func TestOpenSSH(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("no ssh-keygen")
	}
	dir := t.TempDir()
	msg := []byte("a challenge from the Controller")
	msgFile := filepath.Join(dir, "msg")
	if err := os.WriteFile(msgFile, msg, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, k := range keys(t) {
		block, err := ssh.MarshalPrivateKey(k, "")
		if err != nil {
			t.Fatal(err)
		}
		keyFile := filepath.Join(dir, name)
		if err := os.WriteFile(keyFile, pem.EncodeToMemory(block), 0o600); err != nil {
			t.Fatal(err)
		}
		signer, _ := ssh.NewSignerFromKey(k)

		// Ours, checked by ssh-keygen.
		sig, _ := Sign(rand.Reader, signer, "janus-login", msg)
		sigFile := filepath.Join(dir, name+".ours.sig")
		if err := os.WriteFile(sigFile, sig, 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("ssh-keygen", "-Y", "check-novalidate", "-n", "janus-login", "-s", sigFile)
		cmd.Stdin = bytes.NewReader(msg)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s: ssh-keygen refuses our signature: %v %s", name, err, out)
		}

		// ssh-keygen's, checked by ours.
		if out, err := exec.Command("ssh-keygen", "-Y", "sign", "-q", "-f", keyFile, "-n", "janus-login", msgFile).CombinedOutput(); err != nil {
			t.Fatalf("%s: ssh-keygen -Y sign: %v %s", name, err, out)
		}
		theirs, err := os.ReadFile(msgFile + ".sig")
		if err != nil {
			t.Fatal(err)
		}
		_ = os.Remove(msgFile + ".sig")
		if !strings.Contains(string(theirs), armorBegin) {
			t.Fatalf("%s: %s", name, theirs)
		}
		if err := Verify(theirs, signer.PublicKey(), "janus-login", msg); err != nil {
			t.Errorf("%s: ssh-keygen's signature refused: %v", name, err)
		}
	}
}
