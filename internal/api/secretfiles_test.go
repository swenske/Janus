package api

import (
	"archive/tar"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/pki"
)

// secretTree builds root/{pki/{ca.key,token}, public/{notes.txt,
// cert.pem, combined.pem, link -> ../pki/token}}, with root/pki one of
// SecretPaths for the test's duration.
func secretTree(t *testing.T) (root string, keyPEM, certPEM []byte) {
	root = t.TempDir()
	ca, err := pki.NewCA("test")
	if err != nil {
		t.Fatal(err)
	}
	certPEM = ca.CertPEM
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	for name, content := range map[string][]byte{
		"pki/ca.key":          keyPEM,
		"pki/token":           []byte("one-time-registration-token\n"),
		"public/notes.txt":    []byte("hello"),
		"public/cert.pem":     certPEM,
		"public/combined.pem": append(append([]byte{}, certPEM...), keyPEM...),
	} {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("../pki/token", filepath.Join(root, "public/link")); err != nil {
		t.Fatal(err)
	}
	prev := SecretPaths
	SecretPaths = append(append([]string{}, prev...), filepath.Join(root, "pki"))
	t.Cleanup(func() { SecretPaths = prev })
	return root, keyPEM, certPEM
}

// TestReadRefusesSecrets: a secret location, however it's reached, and a
// PEM private key anywhere are never read; everything else still is.
func TestReadRefusesSecrets(t *testing.T) {
	s := &System{}
	root, _, certPEM := secretTree(t)

	if got, err := readAll(t, s, filepath.Join(root, "public/notes.txt")); err != nil || got != "hello" {
		t.Errorf("Read(notes.txt) = %q, %v", got, err)
	}
	if got, err := readAll(t, s, filepath.Join(root, "public/cert.pem")); err != nil || got != string(certPEM) {
		t.Errorf("Read(a certificate) = %q, %v", got, err)
	}
	for _, p := range []string{
		filepath.Join(root, "pki/token"),           // a secret location, not PEM
		filepath.Join(root, "pki/ca.key"),          // both
		filepath.Join(root, "public/link"),         // a symlink to a secret
		filepath.Join(root, "public/../pki/token"), // a path cleaned into one
		"/proc/self/root" + root + "/pki/token",    // through /proc's magic link
		filepath.Join(root, "public/combined.pem"), // a private key outside any secret location
	} {
		got, err := readAll(t, s, p)
		if status.Code(err) != codes.PermissionDenied {
			t.Errorf("Read(%s) = %v, want PermissionDenied", p, err)
		}
		if got != "" {
			t.Errorf("Read(%s) sent %q before refusing", p, got)
		}
	}
}

// TestCopyLeavesSecretsOut: a tree's secrets aren't in its archive, and a
// secret asked for by name is refused.
func TestCopyLeavesSecretsOut(t *testing.T) {
	s := &System{}
	root, _, certPEM := secretTree(t)

	st := &fakeStream[janusv1alpha1.Data]{}
	if err := s.Copy(&janusv1alpha1.CopyRequest{RootPath: root}, st); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	for _, d := range st.got {
		archive.Write(d.Bytes)
	}
	base := filepath.Base(root)
	got := map[string]string{}
	tr := tar.NewReader(&archive)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("not a valid tar: %v", err)
		}
		content, _ := io.ReadAll(tr)
		switch hdr.Typeflag {
		case tar.TypeSymlink:
			got[hdr.Name] = "-> " + hdr.Linkname
		case tar.TypeDir:
			got[hdr.Name] = "dir"
		default:
			got[hdr.Name] = string(content)
		}
	}
	want := map[string]string{
		base + "/": "dir", base + "/public/": "dir",
		base + "/public/notes.txt": "hello", base + "/public/cert.pem": string(certPEM),
		base + "/public/link": "-> ../pki/token", // the link itself, not what it points to
	}
	if len(got) != len(want) {
		t.Errorf("archive = %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("archive[%q] = %q, want %q", k, got[k], v)
		}
	}

	for _, p := range []string{filepath.Join(root, "pki"), filepath.Join(root, "pki/token"), filepath.Join(root, "public/combined.pem")} {
		if err := s.Copy(&janusv1alpha1.CopyRequest{RootPath: p}, &fakeStream[janusv1alpha1.Data]{}); status.Code(err) != codes.PermissionDenied {
			t.Errorf("Copy(%s) = %v, want PermissionDenied", p, err)
		}
	}
}

// TestKeyGuardSplitMarker: a private key block split across reads is
// still caught, before any byte past its opening line is handed over.
func TestKeyGuardSplitMarker(t *testing.T) {
	_, keyPEM, _ := secretTree(t)
	content := append([]byte("some text first\n"), keyPEM...)
	got, err := io.ReadAll(&keyGuard{r: iotest.OneByteReader(bytes.NewReader(content))})
	if !errors.Is(err, errPrivateKey) {
		t.Fatalf("err = %v, want errPrivateKey", err)
	}
	if !strings.HasPrefix(string(content), string(got)) || len(got) >= len("some text first\n-----BEGIN EC PRIVATE KEY-----") {
		t.Errorf("handed over %q", got)
	}
}
