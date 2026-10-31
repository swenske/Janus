package main

import (
	"os"
	"testing"

	"github.com/swenske/Janus/dashboard/backend/internal/identity"
	"github.com/swenske/Janus/dashboard/backend/internal/secrets"
)

func TestEnvOr(t *testing.T) {
	const key = "JANUS_CONTROLLER_TEST_ENV_OR"

	os.Unsetenv(key)
	if got := envOr(key, "fallback"); got != "fallback" {
		t.Errorf("envOr with unset var = %q, want %q", got, "fallback")
	}

	t.Setenv(key, "from-env")
	if got := envOr(key, "fallback"); got != "from-env" {
		t.Errorf("envOr with set var = %q, want %q", got, "from-env")
	}

	// Empty is a real, explicit value (distinct from unset) - matches
	// -advertise-address's own "" default meaning "nothing extra",
	// not "fall back to some other default".
	t.Setenv(key, "")
	if got := envOr(key, "fallback"); got != "" {
		t.Errorf("envOr with var explicitly set to empty = %q, want %q", got, "")
	}
}

// testIdentity is a Controller identity in dir, its CA sealed with dir's
// master key.
func testIdentity(t *testing.T, dir string) *identity.Identity {
	t.Helper()
	key, err := secrets.LoadOrCreate("", dir)
	if err != nil {
		t.Fatal(err)
	}
	id, err := identity.Open(dir, key, "")
	if err != nil {
		t.Fatal(err)
	}
	return id
}
