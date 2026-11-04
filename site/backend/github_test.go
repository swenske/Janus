package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestReleasesByVersion: GitHub lists releases by the date of each tag's
// commit - a maintenance release (a fix of the previous release, tagged
// on its own branch after the newest) comes first. The site sorts them
// by version: the newest is still the latest, every update and pre-build
// takes it.
func TestReleasesByVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `[{"tag_name":"v2026.11.04-3"},{"tag_name":"v2026.11.12","prerelease":true},{"tag_name":"v2026.11.10"},{"tag_name":"v2026.11.04"}]`)
	}))
	t.Cleanup(srv.Close)
	rels, err := newGitHub(srv.URL, "swenske/Janus", "", "schematic-build.yml").Releases(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range rels {
		got = append(got, r.Version)
	}
	if strings.Join(got, " ") != "v2026.11.12 v2026.11.10 v2026.11.04-3 v2026.11.04" {
		t.Errorf("releases in the order %v", got)
	}
}
