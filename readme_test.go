package janus

import (
	"os"
	"regexp"
	"testing"
)

// The README's Go badge is static: shields.io's go-mod badge reads go.mod's
// `go` directive - the minimum language version, 1.26.0 - never the
// toolchain that builds Janus (#432). It and the contributor guide's
// mention must name go.mod's `toolchain`, so a toolchain bump that
// forgets them fails here.
func TestDocsNameTheGoToolchain(t *testing.T) {
	mod, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^toolchain go(\d+\.\d+\.\d+)$`).FindSubmatch(mod)
	if m == nil {
		t.Fatal("go.mod has no toolchain line")
	}
	toolchain := string(m[1])
	for _, c := range []struct{ file, pattern string }{
		{"README.md", `img\.shields\.io/badge/Go-(\d+\.\d+\.\d+)-`},
		{"docs/contributing/development.md", "`toolchain go(\\d+\\.\\d+\\.\\d+)`"},
	} {
		data, err := os.ReadFile(c.file)
		if err != nil {
			t.Fatal(err)
		}
		found := regexp.MustCompile(c.pattern).FindAllSubmatch(data, -1)
		if len(found) == 0 {
			t.Errorf("%s: no Go version matching %s", c.file, c.pattern)
		}
		for _, f := range found {
			if string(f[1]) != toolchain {
				t.Errorf("%s names Go %s, go.mod's toolchain is %s", c.file, f[1], toolchain)
			}
		}
	}
}
