package main

import (
	"bytes"
	"flag"
	"os"
	"testing"
)

var update = flag.Bool("update", false, "rewrite docs/guide/janusctl-reference.md from the command tree, and the tui golden frames")

// The janusctl reference page is what the command tree says:
// go test ./cmd/janusctl -run TestReferenceDoc -update after a change.
func TestReferenceDoc(t *testing.T) {
	const file = "../../docs/guide/janusctl-reference.md"
	var b bytes.Buffer
	writeReference(&b)
	if *update {
		if err := os.WriteFile(file, b.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	have, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(have, b.Bytes()) {
		t.Fatalf("docs/guide/janusctl-reference.md isn't what cmd/janusctl/commands.go says: go test ./cmd/janusctl -run TestReferenceDoc -update")
	}
}
