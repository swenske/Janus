package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
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

func TestCheckRelease(t *testing.T) {
	fakeGitHub(t, list())
	// Newer than the newest release, or that release itself (a rerun);
	// an older one is a maintenance release.
	for v, ok := range map[string]bool{"v2026.10.14": true, "v2026.10.12-2": true, "v2026.10.12": true, "v2026.10.11-9": false, "main": false} {
		if _, err := runOut(t, "check-release", v); (err == nil) != ok {
			t.Errorf("check-release %s: %v", v, err)
		}
	}
}

// maintenanceList: v2026.11.10 is the newest release, v2026.11.04 the
// previous one - published 13 days before "now" (checkMaintenance's
// window); v2026.11.04-2 a pre-release that was never promoted.
func maintenanceList() []release {
	day := func(d int) time.Time { return time.Date(2026, 11, d, 12, 0, 0, 0, time.UTC) }
	return []release{
		{ID: 5, Tag: "v2026.11.12", Prerelease: true, PublishedAt: day(12)},
		{ID: 4, Tag: "v2026.11.10", PublishedAt: day(10)},
		{ID: 3, Tag: "v2026.11.04-2", Prerelease: true, PublishedAt: day(6)},
		{ID: 2, Tag: "v2026.11.04", PublishedAt: day(4)},
		{ID: 1, Tag: "v2026.10.31", PublishedAt: day(1)},
	}
}

func TestCheckMaintenance(t *testing.T) {
	prevNow, prevAncestor := now, ancestor
	t.Cleanup(func() { now, ancestor = prevNow, prevAncestor })
	now = func() time.Time { return time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC) }
	// HEAD is a branch made at base's tag (and the maintenance release's
	// own tag, once published).
	from := func(base ...string) {
		ancestor = func(tag, ref string) bool { return ref == "HEAD" && slices.Contains(base, tag) }
	}

	rels := maintenanceList()
	from("v2026.11.04", "v2026.10.31")
	for v, want := range map[string]string{
		"v2026.11.04-3": "",
		"v2026.11.04-9": "",
		"v2026.11.04-2": "is a pre-release",
		"v2026.11.04":   "takes a version of its day", // itself left out, as for a rerun: then v2026.10.31's
		"v2026.11.05":   "takes a version of its day",
		"v2026.11.11":   "takes a version of its day",
		"main":          "isn't a release version",
	} {
		warn, err := checkMaintenance(rels, v, "HEAD")
		if (want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), want)) {
			t.Errorf("check-release -maintenance %s: %v, want %q", v, err, want)
		}
		if warn != "" {
			t.Errorf("check-release -maintenance %s warns %q, inside the window", v, warn)
		}
	}

	// A rerun finds it published: still a maintenance release of
	// v2026.11.04, its own tag reachable from HEAD now.
	published := append([]release{{ID: 6, Tag: "v2026.11.04-3", PublishedAt: time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)}}, rels...)
	from("v2026.11.04-3", "v2026.11.04")
	if _, err := checkMaintenance(published, "v2026.11.04-3", "HEAD"); err != nil {
		t.Errorf("rerun: %v", err)
	}
	// Then another fix of the same release: the previous release is now
	// the first maintenance release.
	if _, err := checkMaintenance(published, "v2026.11.04-4", "HEAD"); err != nil {
		t.Errorf("a second maintenance release: %v", err)
	}

	// From the newest release, or one older than the previous: refused.
	from("v2026.11.10", "v2026.11.04")
	if _, err := checkMaintenance(rels, "v2026.11.10-2", "HEAD"); err == nil || !strings.Contains(err.Error(), "is the newest release") {
		t.Errorf("from the newest release: %v", err)
	}
	from("v2026.10.31")
	if _, err := checkMaintenance(rels, "v2026.10.31-2", "HEAD"); err == nil || !strings.Contains(err.Error(), "only those are supported") {
		t.Errorf("from an older release: %v", err)
	}

	// Past the window: published all the same, with a warning.
	from("v2026.11.04")
	now = func() time.Time { return time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC) }
	if warn, err := checkMaintenance(rels, "v2026.11.04-3", "HEAD"); err != nil || !strings.Contains(warn, "v2026.11.10 came out 15 days ago") {
		t.Errorf("past the window: %q, %v", warn, err)
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
