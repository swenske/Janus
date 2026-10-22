package sso

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/auth"
	"github.com/swenske/Janus/dashboard/backend/internal/secrets"
)

func openStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	key, err := secrets.LoadOrCreate(filepath.Join(dir, "master.key"), dir)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func ptr[T any](v T) *T { return &v }

func TestStore(t *testing.T) {
	s, dir := openStore(t)
	p := keycloak(t)
	saved, err := s.Put(p, ptr("s3cret"))
	if err != nil {
		t.Fatal(err)
	}
	if saved.Version != 1 {
		t.Fatalf("version %d", saved.Version)
	}
	if got, _ := s.Secret("kc"); got != "s3cret" {
		t.Fatalf("secret %q", got)
	}
	data, err := os.ReadFile(filepath.Join(dir, settingsFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "s3cret") {
		t.Fatal("the secret is in sso.json in the clear")
	}
	if fi, _ := os.Stat(filepath.Join(dir, settingsFile)); fi.Mode().Perm() != 0o600 {
		t.Fatalf("sso.json is %v", fi.Mode().Perm())
	}
	// Absent keeps the secret; "" clears it.
	p.Name = "Our Keycloak"
	if saved, err = s.Put(p, nil); err != nil || saved.Version != 2 {
		t.Fatalf("put: %v, version %d", err, saved.Version)
	}
	if got, _ := s.Secret("kc"); got != "s3cret" {
		t.Fatalf("kept secret %q", got)
	}
	if v := s.List(); len(v) != 1 || !v[0].HasSecret || v[0].Name != "Our Keycloak" {
		t.Fatalf("list: %+v", v)
	}
	if _, err := s.Put(p, ptr("")); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Secret("kc"); got != "" {
		t.Fatalf("cleared secret %q", got)
	}
	// A public client keeps PKCE.
	p.DisablePKCE = true
	if _, err := s.Put(p, nil); err == nil || !strings.Contains(err.Error(), "keep PKCE on") {
		t.Fatalf("public client without PKCE: %v", err)
	}
	if got, _ := s.Get("kc"); got.DisablePKCE || got.Version != 3 {
		t.Fatalf("a refused change was kept: %+v", got)
	}
	// Read back by a new store: the same, secret openable.
	if _, err := s.Put(keycloak(t), ptr("again")); err != nil {
		t.Fatal(err)
	}
	key, _ := secrets.LoadOrCreate(filepath.Join(dir, "master.key"), dir)
	s2, err := Open(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s2.Secret("kc"); got != "again" {
		t.Fatalf("secret after reopening: %q", got)
	}
	// A sealed secret is bound to its provider: moved under another ID,
	// it doesn't open.
	s2.providers = append(s2.providers, stored{Provider: Provider{ID: "other"}, SecretSealed: s2.providers[0].SecretSealed})
	if _, err := s2.Secret("other"); err == nil {
		t.Fatal("a secret sealed for kc opened for other")
	}
	if err := s.Delete("kc"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get("kc"); ok {
		t.Fatal("deleted, still there")
	}
	if err := s.Delete("kc"); err != ErrNoProvider {
		t.Fatalf("delete twice: %v", err)
	}
}

func TestStoreButtonsAndLimits(t *testing.T) {
	s, _ := openStore(t)
	for i, name := range []string{"Zeta", "Alpha", "Mid"} {
		p := keycloak(t)
		p.ID, p.Name, p.Enabled = string(rune('a'+i)), name, name != "Mid"
		if _, err := s.Put(p, nil); err != nil {
			t.Fatal(err)
		}
	}
	b := s.Buttons()
	if len(b) != 2 || b[0].Name != "Alpha" || b[1].Name != "Zeta" {
		t.Fatalf("buttons: %+v", b)
	}
	for i := len(s.List()); i < MaxProviders; i++ {
		p := keycloak(t)
		p.ID = "p" + string(rune('a'+i))
		if _, err := s.Put(p, nil); err != nil {
			t.Fatal(err)
		}
	}
	p := keycloak(t)
	p.ID = "one-too-many"
	if _, err := s.Put(p, nil); err == nil {
		t.Fatal("more providers than MaxProviders")
	}
	// The store's copies aren't the caller's.
	got, _ := s.Get("a")
	got.Rules = append(got.Rules, Rule{Group: "x", Role: auth.Admin})
	got.Scopes[0] = "changed"
	if again, _ := s.Get("a"); len(again.Rules) != 0 || again.Scopes[0] != "openid" {
		t.Fatalf("the store's copy changed: %+v", again)
	}
}

func TestLogins(t *testing.T) {
	l := NewLogins()
	now := time.Now()
	l.now = func() time.Time { return now }
	key, err := l.Start(Login{Provider: "kc", State: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := l.Take(key); !ok || got.State != "s" {
		t.Fatalf("take: %+v %v", got, ok)
	}
	if _, ok := l.Take(key); ok {
		t.Fatal("taken twice")
	}
	key, _ = l.Start(Login{Provider: "kc"})
	now = now.Add(loginTTL)
	if _, ok := l.Take(key); ok {
		t.Fatal("taken after its time")
	}
	l.Finish(key, Done{Login: Login{Provider: "kc"}})
	if d, ok := l.TakeDone(key); !ok || d.Login.Provider != "kc" {
		t.Fatal("done not kept")
	}
	if _, ok := l.TakeDone(key); ok {
		t.Fatal("done taken twice")
	}
	l.Finish(key, Done{})
	now = now.Add(doneTTL)
	if _, ok := l.TakeDone(key); ok {
		t.Fatal("done taken after its time")
	}
	// Full: refused, until the old ones expire.
	for range maxLogins {
		if _, err := l.Start(Login{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := l.Start(Login{}); err != ErrBusy {
		t.Fatalf("full: %v", err)
	}
	now = now.Add(loginTTL)
	if _, err := l.Start(Login{}); err != nil {
		t.Fatalf("swept: %v", err)
	}
}
