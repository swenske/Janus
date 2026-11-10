package main

import "testing"

// TestSafeFilePart: a node-named hostname can't put a path separator or
// odd bytes into the default bundle filename (security review #28).
func TestSafeFilePart(t *testing.T) {
	for in, want := range map[string]string{
		"edge-1":         "edge-1",
		"lb.example.com": "lb.example.com",
		"../etc/passwd":  "etc_passwd",
		"a/b\x00c":       "a_b_c",
		"...":            "",
		"a b":            "a_b",
	} {
		if got := safeFilePart(in); got != want {
			t.Errorf("safeFilePart(%q) = %q, want %q", in, got, want)
		}
	}
}
