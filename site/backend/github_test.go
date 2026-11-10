package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestReleaseNotFoundCached: a version GitHub has no release for is asked
// once, then answered from the negative cache - a loop polling an unknown
// version mustn't spend the token's quota (security review 2026-11-05 #27).
func TestReleaseNotFoundCached(t *testing.T) {
	var tagHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/releases/tags/"):
			tagHits++
			w.WriteHeader(http.StatusNotFound)
		case strings.HasSuffix(r.URL.Path, "/releases"):
			_, _ = io.WriteString(w, `[{"tag_name":"v2026.11.10"}]`)
		}
	}))
	t.Cleanup(srv.Close)
	gh := newGitHub(srv.URL, "swenske/Janus", "", "schematic-build.yml")
	for i := 0; i < 3; i++ {
		if _, err := gh.Release(t.Context(), "v2099.01.01"); !errors.Is(err, errNotFound) {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if tagHits != 1 {
		t.Errorf("GitHub was asked %d times for the missing version, want 1", tagHits)
	}
}

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
