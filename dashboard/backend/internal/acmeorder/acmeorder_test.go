package acmeorder

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swenske/Janus/internal/acme"
)

// plainSealer "seals" by prefixing: enough to see what's sealed and what
// isn't.
type plainSealer struct{}

func (plainSealer) Seal(p []byte, purpose string) ([]byte, error) {
	return append([]byte("sealed:"+purpose+":"), p...), nil
}

func (plainSealer) Open(s []byte, purpose string) ([]byte, error) {
	return []byte(strings.TrimPrefix(string(s), "sealed:"+purpose+":")), nil
}

// TestStore: an account key made once and kept; settings sealed, their
// secrets kept when sent empty, wrong ones refused; orders kept and
// forgotten.
func TestStore(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, plainSealer{})
	if err != nil {
		t.Fatal(err)
	}
	thumb := s.Thumbprint()
	if len(thumb) != 43 {
		t.Fatalf("thumbprint %q", thumb)
	}
	if want, _ := acme.AccountThumbprint(s.AccountKey()); want != thumb {
		t.Fatal("thumbprint of another key")
	}
	set := Settings{Email: " ops@example.com ", AcceptTerms: true, DNSProviders: []DNSProvider{{Name: "cf", Type: "cloudflare", Settings: map[string]string{"CF_DNS_API_TOKEN": "s3cret"}}}}
	if _, err := s.Set(set); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	if !strings.Contains(string(raw), "sealed") {
		t.Errorf("state.json isn't sealed: %s", raw)
	}
	// Read again: the same key, the settings.
	s2, err := Open(dir, plainSealer{})
	if err != nil {
		t.Fatal(err)
	}
	if s2.Thumbprint() != thumb || s2.Settings().Email != "ops@example.com" {
		t.Errorf("read back: %q %+v", s2.Thumbprint(), s2.Settings())
	}
	red := s2.Settings().Redacted()
	if red.DNSProviders[0].Settings["CF_DNS_API_TOKEN"] != "" {
		t.Error("a secret in the redacted settings")
	}
	if _, err := s2.Set(red); err != nil {
		t.Fatal(err)
	}
	if got := s2.Settings().DNSProviders[0].Settings["CF_DNS_API_TOKEN"]; got != "s3cret" {
		t.Errorf("a secret sent empty wasn't kept: %q", got)
	}
	bad := red
	bad.DNSProviders = append(bad.DNSProviders, DNSProvider{Name: "x", Type: "nope", Settings: map[string]string{"A": "b"}})
	if _, err := s2.Set(bad); err == nil || !strings.Contains(err.Error(), "unknown type") {
		t.Errorf("an unknown provider type: %v", err)
	}
	if len(s2.Settings().DNSProviders) != 1 {
		t.Error("a refused change changed the settings")
	}

	var o Order
	now := time.Now()
	o.Failed("rate limited", now)
	if o.Failures != 1 || !o.NextAttempt.Equal(now.Add(5*time.Minute)) {
		t.Errorf("failed: %+v", o)
	}
	if err := s2.SetOrder("n1", "site", o); err != nil {
		t.Fatal(err)
	}
	if err := s2.SetOrder("n1", "old", Order{}); err != nil {
		t.Fatal(err)
	}
	if err := s2.Forget("n1", []string{"site"}); err != nil {
		t.Fatal(err)
	}
	s3, err := Open(dir, plainSealer{})
	if err != nil {
		t.Fatal(err)
	}
	if got := s3.Order("n1", "site"); got.LastError != "rate limited" {
		t.Errorf("order read back: %+v", got)
	}
	if got := s3.Order("n1", "old"); got.LastAttempt != (time.Time{}) || len(s3.Nodes()) != 1 {
		t.Errorf("a forgotten order: %+v %v", got, s3.Nodes())
	}
	o.Succeeded("ABCD", now.Add(90*24*time.Hour), now)
	if o.Failures != 0 || o.LastError != "" || !o.NextAttempt.IsZero() || o.Serial != "ABCD" {
		t.Errorf("succeeded: %+v", o)
	}
}

func TestAllowOrder(t *testing.T) {
	s := Settings{NamePolicy: []NameRule{
		{Suffixes: []string{"site.test"}},
		{Selector: map[string]string{"team": "web"}, Suffixes: []string{"web.example.com"}},
	}}

	// Default directories: Let's Encrypt is allowed, a custom CA isn't.
	if err := s.AllowOrder(nil, []string{"site.test", "www.site.test"}, "letsencrypt"); err != nil {
		t.Errorf("LE prod + covered names: %v", err)
	}
	if err := s.AllowOrder(nil, []string{"site.test"}, "https://ca.evil.test/dir"); err == nil {
		t.Error("a CA directory outside the default allowlist was allowed")
	}

	// A name only a labelled node may get.
	if err := s.AllowOrder(nil, []string{"web.example.com"}, "letsencrypt"); err == nil {
		t.Error("a label-scoped name was allowed for an unlabelled node")
	}
	if err := s.AllowOrder(map[string]string{"team": "web"}, []string{"*.web.example.com"}, "letsencrypt"); err != nil {
		t.Errorf("a wildcard under a covered suffix for the right node: %v", err)
	}
	if err := s.AllowOrder(nil, []string{"other.test"}, "letsencrypt"); err == nil {
		t.Error("a name under no suffix was allowed")
	}

	// An explicit directory allowlist replaces the default.
	s.Directories = []string{"https://ca.example.test/dir"}
	if err := s.AllowOrder(nil, []string{"site.test"}, "https://ca.example.test/dir"); err != nil {
		t.Errorf("an explicitly allowed directory: %v", err)
	}
	if err := s.AllowOrder(nil, []string{"site.test"}, "letsencrypt"); err == nil {
		t.Error("Let's Encrypt was allowed although the allowlist names only a custom CA")
	}

	// Fail closed: no policy, nothing ordered.
	if err := (Settings{}).AllowOrder(nil, []string{"site.test"}, "letsencrypt"); err == nil {
		t.Error("with no name policy a name was allowed - not fail-closed")
	}

	// Validate catches a bad suffix and a bad directory.
	bad := Settings{NamePolicy: []NameRule{{Suffixes: []string{"not a domain"}}}, Directories: []string{"ftp://x"}}
	if errs := bad.Validate(); len(errs) < 2 {
		t.Errorf("Validate missed the bad suffix and directory: %v", errs)
	}
}
