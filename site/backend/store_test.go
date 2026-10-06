package main

import (
	"os"
	"strings"
	"testing"
)

// Each platform's guide is a page of the docs site (site/docs/
// structure.yaml): the builder links its permalink.
func TestPlatformDocsArePages(t *testing.T) {
	structure, err := os.ReadFile("../docs/structure.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range platforms {
		if p.Docs != "" && !strings.Contains(string(structure), "file: "+p.Docs+",") {
			t.Errorf("%s's guide %s isn't a page of the docs site", p.ID, p.Docs)
		}
	}
}
