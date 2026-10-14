package haproxy

import "testing"

func TestPinnedThreads(t *testing.T) {
	for cfg, want := range map[string]string{
		"global\n  chroot /var/empty\n":        "",
		"global\n  nbthread 4\n":               "nbthread",
		"global\n    cpu-map auto:1/1-4 0-3\n": "cpu-map",
		// Commented out, or in another section, it fixes nothing.
		"global\n  # nbthread 4\n":                            "",
		"global\n  maxconn 1000\ndefaults\n  nbthread 2\n":    "",
		"global\n  log stdout format raw local0 # nbthread\n": "",
	} {
		if got := PinnedThreads([]byte(cfg)); got != want {
			t.Errorf("PinnedThreads(%q) = %q, want %q", cfg, got, want)
		}
	}
}
