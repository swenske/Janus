package nocloud

import (
	"os"
	"path/filepath"
	"testing"
)

// The NoCloud user-data the docs show (examples/nocloud, in
// docs/private-cloud/first-boot.md) is user-data a node accepts.
func TestDocsExamples(t *testing.T) {
	files, err := filepath.Glob("../../examples/nocloud/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("no examples/nocloud/*.json: %v", err)
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := parseUserData(raw); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}
