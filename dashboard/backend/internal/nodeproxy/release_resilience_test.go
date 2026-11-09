package nodeproxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// freshAssetCaches empties the caches of releases' files (read once per
// process) for the test - so it can run again (-count) - and puts them
// back after.
func freshAssetCaches(t *testing.T) {
	t.Helper()
	securityByTag.Lock()
	prevSec := securityByTag.m
	securityByTag.m = map[string][]releaseFix{}
	securityByTag.Unlock()
	assetTexts.Lock()
	prevTexts := assetTexts.m
	assetTexts.m = map[string]string{}
	assetTexts.Unlock()
	t.Cleanup(func() {
		securityByTag.Lock()
		securityByTag.m = prevSec
		securityByTag.Unlock()
		assetTexts.Lock()
		assetTexts.m = prevTexts
		assetTexts.Unlock()
	})
}

// keepReleaseCache puts the release cache back as it was after the test.
func keepReleaseCache(t *testing.T) {
	t.Helper()
	freshAssetCaches(t)
	releaseCache.mu.Lock()
	prevURL, prevData, prevErr, prevAt, prevTried, prevFailed := releaseCache.url, releaseCache.data, releaseCache.err, releaseCache.fetchedAt, releaseCache.triedAt, releaseCache.failed
	releaseCache.url, releaseCache.data, releaseCache.err, releaseCache.fetchedAt, releaseCache.triedAt, releaseCache.failed = "", nil, nil, time.Time{}, time.Time{}, false
	releaseCache.mu.Unlock()
	prevPartial, prevStale, prevRetry, prevReleases := partialRetry, staleTTL, staleRetry, ReleasesURL
	t.Cleanup(func() {
		releaseCache.mu.Lock()
		releaseCache.url, releaseCache.data, releaseCache.err, releaseCache.fetchedAt, releaseCache.triedAt, releaseCache.failed = prevURL, prevData, prevErr, prevAt, prevTried, prevFailed
		releaseCache.mu.Unlock()
		partialRetry, staleTTL, staleRetry, ReleasesURL = prevPartial, prevStale, prevRetry, prevReleases
	})
}

// TestReleaseOfferedDespiteDownloads: GitHub's API answering, its
// release downloads not always (2026-11-09) - the newest release is
// offered all the same, its fixes read once they come, each small asset
// read once.
func TestReleaseOfferedDespiteDownloads(t *testing.T) {
	keepReleaseCache(t)
	var down atomic.Bool
	down.Store(true)
	var listed, sums, v3json atomic.Int32
	var gh *httptest.Server
	gh = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asset := func(name, path string) string {
			return `{"name":"` + name + `","browser_download_url":"` + gh.URL + path + `"}`
		}
		switch r.URL.Path {
		case "/releases":
			listed.Add(1)
			_, _ = io.WriteString(w, `[
				{"tag_name":"v2026.11.09","html_url":"https://github.invalid/r/releases/tag/v2026.11.09","assets":[`+
				asset("rootfs.squashfs.sha256", "/v3.sha")+`,`+asset("security.json", "/v3.json")+`]},
				{"tag_name":"v2026.11.08","html_url":"https://github.invalid/r/releases/tag/v2026.11.08","assets":[]}]`)
		case "/v3.sha":
			sums.Add(1)
			_, _ = io.WriteString(w, "abc  rootfs.squashfs\n")
		case "/v3.json":
			if down.Load() {
				http.Error(w, "gateway timeout", http.StatusGatewayTimeout)
				return
			}
			v3json.Add(1)
			_, _ = io.WriteString(w, `{"version":"v2026.11.09","updates":[{"name":"linux","target":"node","fixes":[{"id":"k","severity":"critical"}]}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(gh.Close)
	ReleasesURL, partialRetry = gh.URL+"/releases", 200*time.Millisecond

	rel, err := getLatestRelease(context.Background())
	if err != nil || rel.TagName != "v2026.11.09" || rel.SHA256 == "" {
		t.Fatalf("security.json not coming: %+v %v - want the release offered", rel, err)
	}
	if sev, _ := rel.SecurityUpdate("v2026.11.08", "node", NodeImage{}); sev != "" {
		t.Fatalf("a fix not read yet, and a badge: %q", sev)
	}
	// The downloads back: the same list until partialRetry, then read
	// again - its fixes in, the checksum not downloaded again.
	down.Store(false)
	if again, _ := getLatestRelease(context.Background()); again != rel || listed.Load() != 1 {
		t.Fatalf("fetched again before partialRetry (%d lists)", listed.Load())
	}
	time.Sleep(250 * time.Millisecond)
	releaseCache.mu.Lock()
	releaseCache.triedAt = time.Time{} // releaseRefreshMin (30 s) between two tries
	releaseCache.mu.Unlock()
	rel, err = getLatestRelease(context.Background())
	if err != nil || listed.Load() != 2 {
		t.Fatalf("after partialRetry: %v, %d lists", err, listed.Load())
	}
	if sev, _ := rel.SecurityUpdate("v2026.11.08", "node", NodeImage{}); sev != "critical" {
		t.Fatalf("its fix once read: %q", sev)
	}
	if sums.Load() != 1 {
		t.Errorf("the checksum downloaded %d times", sums.Load())
	}
	// Complete: kept releaseCacheTTL.
	time.Sleep(250 * time.Millisecond)
	if _, err := getLatestRelease(context.Background()); err != nil || listed.Load() != 2 || v3json.Load() != 1 {
		t.Fatalf("a complete list fetched again: %v, %d lists, %d security.json", err, listed.Load(), v3json.Load())
	}
}

// TestReleaseListKeptWhileGitHubFails: a failed fetch keeps the last
// list for staleTTL, tried again every staleRetry - not dropped after
// releaseCacheTTL.
func TestReleaseListKeptWhileGitHubFails(t *testing.T) {
	keepReleaseCache(t)
	var failing atomic.Bool
	failing.Store(true)
	var calls atomic.Int32
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		if failing.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, `[{"tag_name":"v2026.11.09","html_url":"https://github.invalid/r/releases/tag/v2026.11.09","assets":[]}]`)
	}))
	t.Cleanup(gh.Close)
	ReleasesURL, staleRetry = gh.URL, 200*time.Millisecond
	old := &ReleaseInfo{TagName: "v2026.11.08"}
	releaseCache.mu.Lock()
	releaseCache.url, releaseCache.data, releaseCache.fetchedAt = ReleasesURL, old, time.Now().Add(-time.Hour)
	releaseCache.mu.Unlock()

	if rel, err := getLatestRelease(context.Background()); err != nil || rel != old || calls.Load() != 1 {
		t.Fatalf("an hour-old list and GitHub failing: %+v %v, %d calls - want the list kept", rel, err, calls.Load())
	}
	if rel, _ := getLatestRelease(context.Background()); rel != old || calls.Load() != 1 {
		t.Fatalf("tried again before staleRetry: %d calls", calls.Load())
	}
	time.Sleep(250 * time.Millisecond)
	if rel, _ := getLatestRelease(context.Background()); rel != old || calls.Load() != 2 {
		t.Fatalf("after staleRetry: %d calls", calls.Load())
	}
	failing.Store(false)
	time.Sleep(250 * time.Millisecond)
	if rel, err := getLatestRelease(context.Background()); err != nil || rel.TagName != "v2026.11.09" {
		t.Fatalf("GitHub back: %+v %v", rel, err)
	}

	// Older than staleTTL: the error.
	failing.Store(true)
	releaseCache.mu.Lock()
	releaseCache.fetchedAt, releaseCache.triedAt = time.Now().Add(-7*time.Hour), time.Time{}
	releaseCache.mu.Unlock()
	if _, err := getLatestRelease(context.Background()); err == nil {
		t.Fatal("a list older than staleTTL kept")
	}
}

// TestReleaseFilesKeptAcrossARestart: what was read of each release's
// files is written to ReleaseCacheFile and read back by a Controller
// starting again - which then downloads none of them (#553).
func TestReleaseFilesKeptAcrossARestart(t *testing.T) {
	keepReleaseCache(t)
	var downloads atomic.Int32
	var gh *httptest.Server
	gh = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asset := func(name, path string) string {
			return `{"name":"` + name + `","browser_download_url":"` + gh.URL + path + `"}`
		}
		switch r.URL.Path {
		case "/releases":
			_, _ = io.WriteString(w, `[
				{"tag_name":"v2026.11.09","html_url":"https://github.invalid/r/releases/tag/v2026.11.09","assets":[`+
				asset("rootfs.squashfs.sha256", "/v3.sha")+`,`+asset("security.json", "/v3.json")+`]},
				{"tag_name":"v2026.11.08","html_url":"https://github.invalid/r/releases/tag/v2026.11.08","assets":[`+asset("security.json", "/v2.json")+`]}]`)
		case "/v3.sha":
			downloads.Add(1)
			_, _ = io.WriteString(w, "abc\n")
		case "/v3.json":
			downloads.Add(1)
			_, _ = io.WriteString(w, `{"updates":[{"name":"linux","target":"node","fixes":[{"id":"k","severity":"critical"}]}]}`)
		case "/v2.json":
			downloads.Add(1)
			_, _ = io.WriteString(w, `{"updates":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(gh.Close)
	prevFile := ReleaseCacheFile
	ReleaseCacheFile = filepath.Join(t.TempDir(), "release-cache.json")
	t.Cleanup(func() { ReleaseCacheFile = prevFile })
	ReleasesURL = gh.URL + "/releases"

	if _, err := fetchLatestRelease(context.Background()); err != nil || downloads.Load() != 3 {
		t.Fatalf("first fetch: %v, %d downloads", err, downloads.Load())
	}
	if _, err := os.Stat(ReleaseCacheFile); err != nil {
		t.Fatalf("nothing kept: %v", err)
	}

	// A restart: the caches empty, read back from the file.
	freshAssetCaches(t)
	if err := LoadReleaseCache(); err != nil {
		t.Fatal(err)
	}
	rel, err := fetchLatestRelease(context.Background())
	if err != nil || downloads.Load() != 3 {
		t.Fatalf("after a restart: %v, %d downloads - want none more", err, downloads.Load())
	}
	if sev, _ := rel.SecurityUpdate("v2026.11.08", "node", NodeImage{}); sev != "critical" || rel.SHA256 != "abc" {
		t.Fatalf("read back: severity %q, sha256 %q", sev, rel.SHA256)
	}
}

// TestSecuritiesReadAtOnce: the security.json not read yet are read
// together - 12 that each take 200 ms take well under their 2.4 s in a
// row (#553: ~60 of them, minutes when GitHub was slow).
func TestSecuritiesReadAtOnce(t *testing.T) {
	keepReleaseCache(t)
	var gh *httptest.Server
	var list strings.Builder
	gh = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/releases" {
			_, _ = io.WriteString(w, list.String())
			return
		}
		time.Sleep(200 * time.Millisecond)
		_, _ = io.WriteString(w, `{"updates":[]}`)
	}))
	t.Cleanup(gh.Close)
	list.WriteString("[")
	for i := 12; i >= 1; i-- {
		if i < 12 {
			list.WriteString(",")
		}
		tag := fmt.Sprintf("v2026.10.%02d", i)
		list.WriteString(`{"tag_name":"` + tag + `","html_url":"https://github.invalid/r/releases/tag/` + tag + `","assets":[{"name":"security.json","browser_download_url":"` + gh.URL + "/" + tag + `.json"}]}`)
	}
	list.WriteString("]")
	ReleasesURL = gh.URL + "/releases"
	start := time.Now()
	if _, err := fetchLatestRelease(context.Background()); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 1500*time.Millisecond {
		t.Fatalf("12 security.json took %s", took)
	}
}
