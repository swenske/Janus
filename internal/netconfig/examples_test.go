package netconfig

import (
	"os"
	"path/filepath"
	"testing"
)

// The network configurations the docs show (examples/network) are
// configurations a node accepts.
func TestDocsExamples(t *testing.T) {
	files, err := filepath.Glob("../../examples/network/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("no examples/network/*.json: %v", err)
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Parse(raw); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}
