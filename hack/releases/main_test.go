package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeGitHub lists rels, 100 a page, newest first like GitHub.
func fakeGitHub(t *testing.T, rels []release) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/releases" || r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
			return
		}
		var page int
		_, _ = fmt.Sscan(r.URL.Query().Get("page"), &page)
		lo, hi := min((page-1)*100, len(rels)), min(page*100, len(rels))
		_ = json.NewEncoder(w).Encode(rels[lo:hi])
	}))
	t.Cleanup(srv.Close)
	t.Setenv("GITHUB_API_URL", srv.URL)
	t.Setenv("GITHUB_REPOSITORY", "o/r")
	t.Setenv("GITHUB_TOKEN", "tok")
}

func list() []release {
	rels := []release{
		{ID: 9, Tag: "v2026.10.13", Prerelease: true, Assets: []asset{{Name: "security.json", URL: "https://x/security.json", Size: 2, Digest: "sha256:" + strings.Repeat("a", 64)}}},
		{ID: 8, Tag: "v2026.10.12-2", Draft: true},
		{ID: 7, Tag: "v2026.10.12-2-dev"}, // not a release version
		{ID: 6, Tag: "v2026.10.12"},
		{ID: 5, Tag: "v2026.10.11-3", Prerelease: true},
	}
	// Enough older releases for a second page, the newest of them listed
	// last (GitHub orders by creation, not by version).
	for d := 1; d <= 120; d++ {
		rels = append(rels, release{ID: int64(100 + d), Tag: fmt.Sprintf("v2026.%02d.%02d", 1+d/28, 1+d%28)})
	}
	return append(rels, release{ID: 1, Tag: "v2026.10.11-2"})
}

func runOut(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := run(args, &out)
	return strings.TrimSpace(out.String()), err
}

func TestNewest(t *testing.T) {
	fakeGitHub(t, list())
	if got, err := runOut(t, "newest"); err != nil || got != "v2026.10.12" {
		t.Errorf("newest = %q, %v", got, err)
	}
	// Reachable from a ref: the newest tag that is an ancestor of it.
	prev := ancestor
	ancestor = func(tag, ref string) bool { return ref == "HEAD" && tag != "v2026.10.12" }
	t.Cleanup(func() { ancestor = prev })
	if got, err := runOut(t, "newest", "-reachable-from", "HEAD"); err != nil || got != "v2026.10.11-2" {
		t.Errorf("newest reachable = %q, %v", got, err)
	}
	if _, err := runOut(t, "newest", "-reachable-from", "elsewhere"); err == nil {
		t.Error("no release reachable: no error")
	}
}

func TestCheckPrerelease(t *testing.T) {
	fakeGitHub(t, list())
	for v, ok := range map[string]bool{"v2026.10.13": true, "v2026.10.12-2": true, "v2026.10.12": false, "v2026.10.11-9": false, "main": false} {
		if _, err := runOut(t, "check-prerelease", v); (err == nil) != ok {
			t.Errorf("check-prerelease %s: %v", v, err)
		}
	}
}

func TestInfo(t *testing.T) {
	fakeGitHub(t, list())
	out, err := runOut(t, "info", "v2026.10.13")
	if err != nil {
		t.Fatal(err)
	}
	var doc infoDoc
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.ID != 9 || !doc.Prerelease || doc.NewestRelease != "v2026.10.12" || !doc.Newer || doc.Assets["security.json"].SHA256 != strings.Repeat("a", 64) {
		t.Errorf("info = %+v", doc)
	}
	if _, err := runOut(t, "info", "v2020.01.01"); err == nil {
		t.Error("an unknown version: no error")
	}
}
