package enroll

import (
	"strings"
	"testing"
	"time"
)

func TestEnroll(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct{ uses, days int }{"no use": {0, 1}, "too many": {MaxUses + 1, 1}, "no day": {1, 0}, "too long": {1, MaxDays + 1}} {
		if _, _, err := s.Create("x", "root", c.uses, c.days, nil); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, _, err := s.Create("x", "root", 1, 1, map[string]string{"Team": "web"}); err == nil {
		t.Error("a bad label accepted")
	}
	secret, tok, err := s.Create("rack-3", "root", 2, 7, map[string]string{"team": "web"})
	if err != nil || !Is(secret) || !strings.HasPrefix(secret, Prefix+tok.ID+"_") || tok.Hash == "" {
		t.Fatalf("%q %+v %v", secret, tok, err)
	}
	if _, ok := s.Claim(secret + "x"); ok {
		t.Error("a wrong secret claimed")
	}
	if _, ok := s.Claim("janus_" + tok.ID + "_x"); ok {
		t.Error("an API token claimed")
	}
	got, ok := s.Claim(secret)
	if !ok || got.Labels["team"] != "web" || got.Uses != 1 {
		t.Fatalf("first claim: %+v %v", got, ok)
	}
	s.Admitted(tok.ID, "node-a")
	// Kept across a restart.
	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if l := again.List(); len(l) != 1 || l[0].Uses != 1 || l[0].Nodes[0] != "node-a" {
		t.Errorf("reopened: %+v", l)
	}
	if _, ok := again.Claim(secret); !ok {
		t.Error("the second use refused")
	}
	if _, ok := again.Claim(secret); ok {
		t.Error("a third use, of two")
	}
	// Expired.
	secret2, tok2, _ := again.Create("old", "root", 5, 1, nil)
	again.now = func() time.Time { return time.Now().Add(25 * time.Hour) }
	if _, ok := again.Claim(secret2); ok {
		t.Error("an expired token claimed")
	}
	again.now = time.Now
	if err := again.Revoke(tok2.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := again.Claim(secret2); ok {
		t.Error("a revoked token claimed")
	}
}
