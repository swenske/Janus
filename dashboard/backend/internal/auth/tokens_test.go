package auth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTokens(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenTokens(dir)
	if err != nil {
		t.Fatal(err)
	}
	secret, tok, err := s.Create("terraform", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, TokenPrefix+tok.ID+"_") {
		t.Errorf("token %q doesn't name its record %s", secret, tok.ID)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "api-tokens.json"))
	if strings.Contains(string(raw), secret[len(TokenPrefix)+len(tok.ID)+1:]) {
		t.Error("the secret is stored")
	}
	if fi, _ := os.Stat(filepath.Join(dir, "api-tokens.json")); fi.Mode().Perm() != 0o600 {
		t.Errorf("api-tokens.json is %v", fi.Mode().Perm())
	}

	if got, ok := s.Verify(secret); !ok || got.Name != "terraform" || got.LastUsedAt == nil {
		t.Fatalf("Verify(valid) = %+v, %v", got, ok)
	}
	for _, bad := range []string{"", "nope", TokenPrefix + tok.ID + "_wrong", TokenPrefix + "unknown_" + secret, secret + "x", strings.ToUpper(secret)} {
		if _, ok := s.Verify(bad); ok {
			t.Errorf("Verify(%q) accepted", bad)
		}
	}

	// Kept across a restart; revoked at once.
	s2, err := OpenTokens(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s2.Verify(secret); !ok {
		t.Fatal("token lost across a reopen")
	}
	if err := s2.Revoke(tok.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := s2.Verify(secret); ok {
		t.Error("revoked token accepted")
	}
	if s3, _ := OpenTokens(dir); len(s3.List()) != 0 {
		t.Error("revocation not saved")
	}
}

func TestTokenExpiry(t *testing.T) {
	s, err := OpenTokens(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	secret, _, err := s.Create("short", time.Nanosecond)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	if _, ok := s.Verify(secret); ok {
		t.Error("expired token accepted")
	}
	if _, _, err := s.Create(" ", 0); err == nil {
		t.Error("empty name accepted")
	}
}
