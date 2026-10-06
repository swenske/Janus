package main

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"testing"
)

// The line orchestrators grep (docs/private-cloud/first-contact.md):
// its pattern is published, so it never changes.
func TestCAConsoleLine(t *testing.T) {
	der := []byte("a CA certificate's DER")
	sum := sha256.Sum256(der)
	line := caConsoleLine(der)
	m := regexp.MustCompile(`pki: this node's CA: SHA-256 ([0-9a-f]{64})$`).FindStringSubmatch(line)
	if m == nil || m[1] != hex.EncodeToString(sum[:]) {
		t.Fatalf("%q doesn't match the published pattern", line)
	}
}
