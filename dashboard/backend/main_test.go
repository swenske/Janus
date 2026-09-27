package main

import (
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
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

func TestLoadOrCreateDashboardIdentityAdvertiseAddressSAN(t *testing.T) {
	dir := t.TempDir()

	cert, err := loadOrCreateDashboardIdentity(dir, "controller.example.com, 203.0.113.5", "", "")
	if err != nil {
		t.Fatalf("loadOrCreateDashboardIdentity: %v", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse generated certificate: %v", err)
	}

	if !slicesContain(leaf.DNSNames, "controller.example.com") {
		t.Errorf("DNSNames = %v, want it to contain %q", leaf.DNSNames, "controller.example.com")
	}
	foundIP := false
	for _, ip := range leaf.IPAddresses {
		if ip.String() == "203.0.113.5" {
			foundIP = true
		}
	}
	if !foundIP {
		t.Errorf("IPAddresses = %v, want it to contain 203.0.113.5", leaf.IPAddresses)
	}
	// Persisted: a second call against the same dir must load the
	// identical cert back, not regenerate - see the function's own doc
	// comment on why (a fresh identity every restart would mean a new
	// browser trust warning every time for no reason).
	certPath := filepath.Join(dir, "dashboard-identity.crt")
	if _, err := os.Stat(certPath); err != nil {
		t.Fatalf("expected %s to exist after first call: %v", certPath, err)
	}
	cert2, err := loadOrCreateDashboardIdentity(dir, "a-different-hostname-that-must-be-ignored.example", "", "")
	if err != nil {
		t.Fatalf("loadOrCreateDashboardIdentity (second call): %v", err)
	}
	if string(cert2.Certificate[0]) != string(cert.Certificate[0]) {
		t.Error("second call against the same dir returned a different certificate - identity should be cached, not regenerated")
	}
}

func TestLoadOrCreateDashboardIdentityTLSCertRequiresBoth(t *testing.T) {
	if _, err := loadOrCreateDashboardIdentity(t.TempDir(), "", "cert-only.pem", ""); err == nil {
		t.Error("expected an error with -tls-cert set but not -tls-key")
	}
	if _, err := loadOrCreateDashboardIdentity(t.TempDir(), "", "", "key-only.pem"); err == nil {
		t.Error("expected an error with -tls-key set but not -tls-cert")
	}
}

func slicesContain(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
