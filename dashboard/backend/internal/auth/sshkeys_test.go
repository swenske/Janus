package auth

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func authorized(t *testing.T, key any) string {
	t.Helper()
	pub, err := ssh.NewPublicKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))) + " sam@laptop"
}

// TestSSHKeys: an account's keys - one account each, their role at most
// the account's, until they expire.
func TestSSHKeys(t *testing.T) {
	s := open(t, t.TempDir())
	now := time.Unix(1_800_000_000, 0)
	s.now = func() time.Time { return now }
	if _, err := s.Setup("root", "long-enough", MFANobody); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUser("sam", Operator, "long-enough", nil); err != nil {
		t.Fatal(err)
	}
	edPub, _, _ := ed25519.GenerateKey(rand.Reader)
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	small, _ := rsa.GenerateKey(rand.Reader, 1024)

	k, err := s.AddSSHKey("sam", "laptop", authorized(t, edPub), "", 0)
	if err != nil || !strings.HasPrefix(k.Fingerprint, "SHA256:") || strings.Contains(k.PublicKey, "sam@laptop") {
		t.Fatalf("AddSSHKey: %+v %v", k, err)
	}
	if _, err := s.AddSSHKey("root", "again", authorized(t, edPub), "", 0); !errors.Is(err, errSSHKeyTaken) {
		t.Errorf("another account's key: %v", err)
	}
	if _, err := s.AddSSHKey("sam", "more", authorized(t, &ec.PublicKey), Admin, 0); err == nil {
		t.Error("a key above its account's role")
	}
	ro, err := s.AddSSHKey("sam", "shared box", authorized(t, &ec.PublicKey), Reader, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for name, line := range map[string]string{
		"a small RSA key": authorized(t, &small.PublicKey),
		"not a key":       "hello",
		"a FIDO key":      "sk-ssh-ed25519@openssh.com AAAAGnNrLXNzaC1lZDI1NTE5QG9wZW5zc2guY29tAAAAIEX/1vE4bXaRkOaS8T3ZAmOwFJ7sFaEoCCO7tqq8F4wjAAAABHNzaDo=",
	} {
		if _, err := s.AddSSHKey("sam", name, line, "", 0); err == nil {
			t.Errorf("%s accepted", name)
		}
	}

	if _, u, limit, err := s.SSHKeyFor("sam", k.Fingerprint); err != nil || limit != "" || u.Role != Operator {
		t.Errorf("the laptop key: %v %v %v", u.Role, limit, err)
	}
	if _, _, limit, err := s.SSHKeyFor("sam", ro.Fingerprint); err != nil || limit != Reader {
		t.Errorf("the reader key: %v %v", limit, err)
	}
	if _, _, _, err := s.SSHKeyFor("root", k.Fingerprint); err == nil {
		t.Error("sam's key signs root in")
	}
	// Demoted, the account's keys follow; expired or disabled, they stop.
	if _, err := s.UpdateUser("sam", Change{Role: ptr(Reader)}); err != nil {
		t.Fatal(err)
	}
	if _, u, _, _ := s.SSHKeyFor("sam", k.Fingerprint); u.Role != Reader {
		t.Errorf("after a demotion: %v", u.Role)
	}
	now = now.Add(2 * time.Hour)
	if _, _, _, err := s.SSHKeyFor("sam", ro.Fingerprint); err == nil {
		t.Error("an expired key")
	}
	if _, err := s.UpdateUser("sam", Change{Disabled: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.SSHKeyFor("sam", k.Fingerprint); err == nil {
		t.Error("a disabled account's key")
	}
	if err := s.RemoveSSHKey("sam", k.Fingerprint); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.User("sam"); len(u.SSHKeys) != 1 {
		t.Errorf("keys left: %+v", u.SSHKeys)
	}
}
