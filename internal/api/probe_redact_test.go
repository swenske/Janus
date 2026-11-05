package api

import "testing"

// TestRedactTarget: a probe target's credentials are stripped before the
// event (security review 2026-11-05 #21).
func TestRedactTarget(t *testing.T) {
	for in, want := range map[string]string{
		"https://user:pass@host/x": "https://host/x",
		"http://user@host:8080/":   "http://host:8080/",
		"https://host/x":           "https://host/x",
		"host:443":                 "host:443",
		"10.0.0.1":                 "10.0.0.1",
		"probe.test":               "probe.test",
	} {
		if got := redactTarget(in); got != want {
			t.Errorf("redactTarget(%q) = %q, want %q", in, got, want)
		}
	}
}
