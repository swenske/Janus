package nodeproxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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

// keepReleaseCache puts the release cache back as it was after the test,
// once a fetch it left running is over.
func keepReleaseCache(t *testing.T) {
	t.Helper()
	freshAssetCaches(t)
	c := &releaseCache
	c.mu.Lock()
	prev := struct {
		url                         string
		data                        *ReleaseInfo
		err                         error
		list                        []ghRelease
		fetchedAt, retryAt, triedAt time.Time
		failed                      bool
		fetch                       *releaseFetch
	}{c.url, c.data, c.err, c.list, c.fetchedAt, c.retryAt, c.triedAt, c.failed, c.fetch}
	c.url, c.data, c.err, c.list, c.fetchedAt, c.retryAt, c.triedAt, c.failed, c.fetch = "", nil, nil, nil, time.Time{}, time.Time{}, time.Time{}, false, nil
	c.mu.Unlock()
	prevPartial, prevStale, prevRetry, prevReleases := partialRetry, staleTTL, staleRetry, ReleasesURL
	prevWait, prevBudget, prevGrace := releaseWait, releaseFetchBudget, releaseKnownGrace
	t.Cleanup(func() {
		waitReleaseFetch(t)
		c.mu.Lock()
		c.url, c.data, c.err, c.list, c.fetchedAt, c.retryAt, c.triedAt, c.failed, c.fetch = prev.url, prev.data, prev.err, prev.list, prev.fetchedAt, prev.retryAt, prev.triedAt, prev.failed, prev.fetch
		c.mu.Unlock()
		partialRetry, staleTTL, staleRetry, ReleasesURL = prevPartial, prevStale, prevRetry, prevReleases
		releaseWait, releaseFetchBudget, releaseKnownGrace = prevWait, prevBudget, prevGrace
	})
}

// waitReleaseFetch waits for the release fetch running in the background,
// if any, to be over.
func waitReleaseFetch(t *testing.T) {
	t.Helper()
	releaseCache.mu.Lock()
	f := releaseCache.fetch
	releaseCache.mu.Unlock()
	if f == nil {
		return
	}
	select {
	case <-f.done:
	case <-time.After(30 * time.Second):
		t.Error("the release fetch still running after 30 s")
	}
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
	// What's missing read from the same list: GitHub's API not asked
	// again (#627).
	rel, err = getLatestRelease(context.Background())
	if err != nil || listed.Load() != 1 {
		t.Fatalf("after partialRetry: %v, %d lists - want the list read then completed", err, listed.Load())
	}
	if sev, _ := rel.SecurityUpdate("v2026.11.08", "node", NodeImage{}); sev != "critical" || rel.partial {
		t.Fatalf("its fix once read: %q, partial %v", sev, rel.partial)
	}
	if sums.Load() != 1 {
		t.Errorf("the checksum downloaded %d times", sums.Load())
	}
	// Complete: kept releaseCacheTTL.
	time.Sleep(250 * time.Millisecond)
	if _, err := getLatestRelease(context.Background()); err != nil || listed.Load() != 1 || v3json.Load() != 1 {
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

	logged := captureLog(t)
	if rel, err := getLatestRelease(context.Background()); err != nil || rel != old || calls.Load() != 1 {
		t.Fatalf("an hour-old list and GitHub failing: %+v %v, %d calls - want the list kept", rel, err, calls.Load())
	}
	// Its real age (#627: a partial list's said 7 minutes more).
	if !strings.Contains(logged.String(), "the list read 1h0m0s ago kept") {
		t.Errorf("logged %q", logged.String())
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

// TestColdStartSlowGitHub: no release-cache.json, GitHub slow - the
// newest release and the pre-release come first, the security.json with
// what's left, newest first; out of time, the list is partial, not an
// error (v2026.11.09-3's lab campaign: the newest release unknown for 5
// minutes, the pre-release for 3).
func TestColdStartSlowGitHub(t *testing.T) {
	keepReleaseCache(t)
	var gh *httptest.Server
	var list strings.Builder
	gh = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/releases":
			_, _ = io.WriteString(w, list.String())
		case r.URL.Path == "/latest.sha256":
			_, _ = io.WriteString(w, "abc\n")
		case strings.HasSuffix(r.URL.Path, ".json"):
			select {
			case <-time.After(300 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
			_, _ = io.WriteString(w, `{"updates":[{"name":"linux","target":"node","fixes":[{"id":"`+strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), ".json")+`","severity":"high"}]}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(gh.Close)
	rel := func(tag string, pre bool) string {
		return fmt.Sprintf(`{"tag_name":%q,"prerelease":%v,"html_url":"https://github.invalid/r/releases/tag/%s","assets":[{"name":"security.json","browser_download_url":"%s/%s.json"}]}`, tag, pre, tag, gh.URL, tag)
	}
	// The newest release has its checksum to read too: read after the
	// security.json, it failed once the time had run out.
	list.WriteString("[" + rel("v2026.11.20", true) + `,{"tag_name":"v2026.11.19","html_url":"https://github.invalid/r/releases/tag/v2026.11.19","assets":[` +
		`{"name":"rootfs.squashfs.sha256","browser_download_url":"` + gh.URL + `/latest.sha256"},{"name":"security.json","browser_download_url":"` + gh.URL + `/v2026.11.19.json"}]}`)
	for i := 18; i >= 1; i-- {
		list.WriteString("," + rel(fmt.Sprintf("v2026.11.%02d", i), false))
	}
	list.WriteString("]")
	ReleasesURL = gh.URL + "/releases"

	// Time for one round of eight security.json, not three.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	info, err := fetchLatestRelease(ctx)
	if err != nil {
		t.Fatalf("out of time on the security.json: %v - want the list, partial", err)
	}
	if info.TagName != "v2026.11.19" || info.SHA256 != "abc" || info.Prerelease == nil || info.Prerelease.TagName != "v2026.11.20" || !info.partial {
		t.Fatalf("latest %q, pre-release %+v, partial %v", info.TagName, info.Prerelease, info.partial)
	}
	// The newest ones read first: the latest's own fixes are in.
	if _, ok := info.security["v2026.11.19"]; !ok {
		t.Fatalf("the newest release's security.json not read first: %v", slices.Collect(maps.Keys(info.security)))
	}
	if _, ok := info.security["v2026.11.01"]; ok {
		t.Fatal("the oldest read in that time - the order isn't newest first")
	}
}

// captureLog sends the log to a buffer for the test.
func captureLog(t *testing.T) *syncBuffer {
	t.Helper()
	var buf syncBuffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// slowGitHub is a fake GitHub whose list takes list to answer and each
// file each, the requests counted - the netem of v2026.11.10's lab
// campaign, scaled down: GitHub 4 s away, the list longer than a read's
// 30 s. The releases: a pre-release, the newest release, maintenance
// releases, older releases - all with a checksum, a Controller image and
// a security.json, the pre-release's and maintenance releases' read
// before any security.json.
type slowGitHub struct {
	srv          *httptest.Server
	lists, files atomic.Int32
	down         sync.Map // paths answering 504
	slow         sync.Map // paths taking this time.Duration, not each
}

func newSlowGitHub(t *testing.T, list, each time.Duration, maintenance, older int) *slowGitHub {
	t.Helper()
	g := &slowGitHub{}
	var body strings.Builder
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wait := each
		if r.URL.Path == "/releases" {
			g.lists.Add(1)
			wait = list
		} else {
			g.files.Add(1)
		}
		if d, ok := g.slow.Load(r.URL.Path); ok {
			wait = d.(time.Duration)
		}
		select {
		case <-time.After(wait):
		case <-r.Context().Done():
			return
		}
		if _, down := g.down.Load(r.URL.Path); down {
			http.Error(w, "gateway timeout", http.StatusGatewayTimeout)
			return
		}
		switch path := strings.TrimPrefix(r.URL.Path, "/"); {
		case path == "releases":
			_, _ = io.WriteString(w, body.String())
		case strings.HasSuffix(path, ".sha256"):
			_, _ = io.WriteString(w, "sum-"+strings.TrimSuffix(path, ".sha256")+"\n")
		case strings.HasSuffix(path, ".image"):
			_, _ = io.WriteString(w, "swenske/janus-controller:"+strings.TrimSuffix(path, ".image")+"\n")
		case strings.HasSuffix(path, ".json"):
			_, _ = io.WriteString(w, `{"updates":[{"name":"linux","target":"node","fixes":[{"id":"`+strings.TrimSuffix(path, ".json")+`","severity":"high"}]}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(g.srv.Close)
	rel := func(tag, name string, pre bool) string {
		asset := func(file, ext string) string {
			return fmt.Sprintf(`{"name":%q,"browser_download_url":"%s/%s%s"}`, file, g.srv.URL, tag, ext)
		}
		return fmt.Sprintf(`{"tag_name":%q,"name":%q,"prerelease":%v,"html_url":"https://github.invalid/r/releases/tag/%s","assets":[%s,%s,%s]}`,
			tag, name, pre, tag, asset("rootfs.squashfs.sha256", ".sha256"), asset("controller-image.txt", ".image"), asset("security.json", ".json"))
	}
	all := []string{rel("v2026.11.20", "v2026.11.20 (Alpha)", true), rel("v2026.11.19", "v2026.11.19 (Alpha)", false)}
	for i := range maintenance {
		// Maintenance of the releases before, tagged after the newest:
		// v2026.11.18-2 fixes v2026.11.18, and so on.
		day := 18 - i
		all = append(all, rel(fmt.Sprintf("v2026.11.%02d-2", day), fmt.Sprintf("v2026.11.%02d-2 (Alpha) - maintenance of v2026.11.%02d", day, day), false))
	}
	for i := range older {
		all = append(all, rel(fmt.Sprintf("v2026.10.%02d", 31-i), "", false))
	}
	body.WriteString("[" + strings.Join(all, ",") + "]")
	ReleasesURL = g.srv.URL + "/releases"
	return g
}

// TestColdStartGitHubFourSecondsAway: no release-cache.json, GitHub 4 s
// away (v2026.11.10's lab campaign, finding 1, here 1/20th of it): the
// list alone takes longer than a caller waits. The first caller is told
// it's still being read; the read goes on in the background, every
// caller sharing it - the list asked once; the newest release, the
// pre-release and the maintenance releases are known as soon as the list
// and one round of their files have come - read together, not one after
// the other -, the security.json following.
func TestColdStartGitHubFourSecondsAway(t *testing.T) {
	keepReleaseCache(t)
	const (
		rtt  = 200 * time.Millisecond // 4 s
		list = 2 * time.Second        // 40 s: more than a caller's 30 s
	)
	releaseWait, releaseKnownGrace, releaseFetchBudget = 1500*time.Millisecond, 100*time.Millisecond, 9*time.Second
	g := newSlowGitHub(t, list, rtt, 3, 30)

	start := time.Now()
	_, err := getLatestRelease(context.Background())
	if took := time.Since(start); err == nil || !strings.Contains(err.Error(), "still being read") || took > releaseWait+300*time.Millisecond {
		t.Fatalf("first caller: %v after %s - want told the list is still being read after %s", err, took, releaseWait)
	}
	// Callers asking every 50 ms, as pages and the fleet's status do.
	var rel *ReleaseInfo
	for rel == nil || rel.Prerelease == nil {
		if time.Since(start) > 5*time.Second {
			t.Fatalf("no pre-release after %s (%+v)", time.Since(start), rel)
		}
		rel, _ = getLatestRelease(context.Background())
		if rel == nil {
			time.Sleep(50 * time.Millisecond)
		}
	}
	known := time.Since(start)
	// Ten files (the newest's, the pre-release's, three maintenance
	// releases' two each): one after the other, 2 s after the list.
	if bound := list + 3*rtt + releaseKnownGrace + 300*time.Millisecond; known > bound {
		t.Errorf("the pre-release known after %s, more than %s - its files not read together", known, bound)
	}
	if rel.TagName != "v2026.11.19" || rel.SHA256 != "sum-v2026.11.19" || rel.ControllerImage != "swenske/janus-controller:v2026.11.19" ||
		rel.Prerelease.TagName != "v2026.11.20" || rel.Prerelease.ControllerImage != "swenske/janus-controller:v2026.11.20" {
		t.Fatalf("known: %+v, pre-release %+v", rel, rel.Prerelease)
	}
	if m := rel.MaintenanceFor("v2026.11.17"); m == nil || m.TagName != "v2026.11.17-2" || m.SHA256 != "sum-v2026.11.17-2" {
		t.Fatalf("maintenance of v2026.11.17: %+v", m)
	}
	t.Logf("GitHub 4 s away (scaled 1/20): the pre-release known after %s (the list %s)", known, list)

	// The security.json follow, in the same read: the list asked once.
	waitReleaseFetch(t)
	rel, err = getLatestRelease(context.Background())
	if err != nil || rel.partial {
		t.Fatalf("after the read: %v, partial %v", err, rel != nil && rel.partial)
	}
	if sev, from := rel.SecurityUpdate("v2026.10.31", "node", NodeImage{}); sev != "high" || from != "v2026.11.19" {
		t.Fatalf("security update for v2026.10.31: %q from %q", sev, from)
	}
	if n := g.lists.Load(); n != 1 {
		t.Errorf("the list asked %d times - want one read shared by every caller", n)
	}
	t.Logf("complete after %s, %d files", time.Since(start), g.files.Load())
}

// TestFirstAnswerBeforeTheSecurities: a cold start with GitHub 1 s away
// answered after the whole read, 31 s, spent to its end on security.json
// (v2026.11.10's lab campaign) - now as soon as the releases are known,
// releaseKnownGrace later, the security.json read in the background; the
// list answered then is never changed.
func TestFirstAnswerBeforeTheSecurities(t *testing.T) {
	keepReleaseCache(t)
	const rtt = 50 * time.Millisecond
	releaseKnownGrace = rtt
	g := newSlowGitHub(t, rtt, rtt, 1, 60)

	start := time.Now()
	rel, err := getLatestRelease(context.Background())
	took := time.Since(start)
	if err != nil || rel.Prerelease == nil || !rel.partial {
		t.Fatalf("first answer: %+v %v - want the releases, the security.json still coming", rel, err)
	}
	// 62 security.json, 8 at once: 400 ms more.
	if bound := rtt + 2*rtt + releaseKnownGrace + 150*time.Millisecond; took > bound {
		t.Errorf("first answer after %s, more than %s", took, bound)
	}
	fixes := len(rel.security)
	waitReleaseFetch(t)
	again, err := getLatestRelease(context.Background())
	if err != nil || again.partial || len(again.security) != 62 {
		t.Fatalf("after the read: %v, partial %v, %d security.json", err, again != nil && again.partial, len(again.security))
	}
	if len(rel.security) != fixes || again == rel {
		t.Error("the list answered first was changed by the read")
	}
	if g.lists.Load() != 1 {
		t.Errorf("%d lists", g.lists.Load())
	}
}

// TestPartialListCompletedWithoutTheList: the pre-release's Controller
// image missing the first read, GitHub's list failing after (v2026.11.10's
// lab campaign: with GitHub 4 s away the list missed every read's end,
// and the pre-release stayed missing 16 minutes) - completed from the
// list read then, not asked again.
func TestPartialListCompletedWithoutTheList(t *testing.T) {
	keepReleaseCache(t)
	partialRetry = 100 * time.Millisecond
	g := newSlowGitHub(t, 0, 0, 0, 2)
	g.down.Store("/v2026.11.20.image", true)
	rel, err := getLatestRelease(context.Background())
	if err != nil || rel.Prerelease != nil || !rel.partial {
		t.Fatalf("pre-release's image missing: %+v %v - want the list, partial, no pre-release", rel, err)
	}
	g.down.Delete("/v2026.11.20.image")
	g.down.Store("/releases", true)
	logged := captureLog(t)
	time.Sleep(150 * time.Millisecond)
	releaseCache.mu.Lock()
	releaseCache.triedAt = time.Time{} // releaseRefreshMin (30 s) between two tries
	releaseCache.mu.Unlock()
	rel, err = getLatestRelease(context.Background())
	if err != nil || rel.Prerelease == nil || rel.Prerelease.ControllerImage == "" || rel.partial {
		t.Fatalf("completed: %+v %v", rel, err)
	}
	if g.lists.Load() != 1 || strings.Contains(logged.String(), "kept") {
		t.Errorf("the list asked %d times, logged %q", g.lists.Load(), logged.String())
	}
	// Past releaseCacheTTL, the list read again: failing, the one known
	// kept, its real age said.
	releaseCache.mu.Lock()
	releaseCache.fetchedAt = releaseCache.fetchedAt.Add(-releaseCacheTTL - 30*time.Second)
	releaseCache.triedAt = time.Time{}
	releaseCache.mu.Unlock()
	if again, err := getLatestRelease(context.Background()); err != nil || again != rel {
		t.Fatalf("GitHub's list failing: %v - want the list kept", err)
	}
	if !strings.Contains(logged.String(), "the list read 10m30s ago kept") {
		t.Errorf("logged %q", logged.String())
	}
}

// TestCallersShareTheFetch: callers arriving while the list is read wait
// for that read - one request to GitHub -, and once a list is known, a
// caller is answered from it at once while a new read runs.
func TestCallersShareTheFetch(t *testing.T) {
	keepReleaseCache(t)
	g := newSlowGitHub(t, 300*time.Millisecond, 0, 0, 1)
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if rel, err := getLatestRelease(context.Background()); err != nil || rel.TagName != "v2026.11.19" {
				errs <- errors.Join(err, fmt.Errorf("got %+v", rel))
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if g.lists.Load() != 1 {
		t.Fatalf("%d lists for 10 callers", g.lists.Load())
	}
	// Stale: one caller starts a read (and waits for it); another,
	// arriving meanwhile, gets the list known at once.
	releaseCache.mu.Lock()
	known := releaseCache.data
	releaseCache.fetchedAt = releaseCache.fetchedAt.Add(-releaseCacheTTL)
	releaseCache.triedAt = time.Time{}
	releaseCache.mu.Unlock()
	go func() { _, _ = getLatestRelease(context.Background()) }()
	for {
		releaseCache.mu.Lock()
		running := releaseCache.fetch != nil
		releaseCache.mu.Unlock()
		if running {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	start := time.Now()
	if rel, err := getLatestRelease(context.Background()); err != nil || rel != known || time.Since(start) > 100*time.Millisecond {
		t.Fatalf("while a read runs: %v after %s - want the list known, at once", err, time.Since(start))
	}
	waitReleaseFetch(t)
	if g.lists.Load() != 2 {
		t.Errorf("%d lists", g.lists.Load())
	}
}

// TestAnswersTakeTheSecuritiesReadSoFar: an answer while the
// security.json are read has the ones read so far - newest first, the
// files a 🔒 badge depends on. Against GitHub 4 s away the answers lacked
// the badge until the 63rd file, 70 s after the one that mattered
// (#627's follow-up).
func TestAnswersTakeTheSecuritiesReadSoFar(t *testing.T) {
	keepReleaseCache(t)
	releaseKnownGrace = 100 * time.Millisecond
	g := newSlowGitHub(t, 0, 0, 0, 30)
	for i := range 30 {
		g.slow.Store(fmt.Sprintf("/v2026.10.%02d.json", 31-i), time.Second)
	}
	start := time.Now()
	rel, err := getLatestRelease(context.Background())
	if err != nil || !rel.partial || time.Since(start) > time.Second {
		t.Fatalf("first answer: %v, partial %v after %s - want the releases, the older security.json still coming", err, rel != nil && rel.partial, time.Since(start))
	}
	// The newest release's own security.json came in the first round.
	if sev, from := rel.SecurityUpdate("v2026.10.31", "node", NodeImage{}); sev != "high" || from != "v2026.11.19" {
		t.Fatalf("first answer, v2026.10.31: %q from %q - want the newest release's fix", sev, from)
	}
	again, _ := getLatestRelease(context.Background())
	if again == rel {
		t.Error("an answer while the read runs is the first one again")
	}
	waitReleaseFetch(t)
	final, err := getLatestRelease(context.Background())
	if err != nil || final.partial || len(final.security) != 31 {
		t.Fatalf("after the read: %v, partial %v, %d security.json", err, final != nil && final.partial, len(final.security))
	}
	if g.lists.Load() != 1 {
		t.Errorf("%d lists", g.lists.Load())
	}
}

// TestListReadAgainAfterAReset: the list's answer cut short - GitHub 4 s
// away reset its stream ~27 s in, twice in two measures (#627's
// follow-up) - is read again at once in the same fetch, not after
// errorCacheTTL and a caller; a status of GitHub's isn't (see
// TestReleaseErrorCachedBriefly).
func TestListReadAgainAfterAReset(t *testing.T) {
	keepReleaseCache(t)
	var lists atomic.Int32
	body := `[{"tag_name":"v2026.11.19","html_url":"https://github.invalid/r/releases/tag/v2026.11.19","assets":[]}]`
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if lists.Add(1) == 1 {
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			_, _ = io.WriteString(w, body[:20])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			panic(http.ErrAbortHandler) // the connection cut, the answer half sent
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(gh.Close)
	ReleasesURL = gh.URL
	logged := captureLog(t)
	rel, err := getLatestRelease(context.Background())
	if err != nil || rel.TagName != "v2026.11.19" || lists.Load() != 2 {
		t.Fatalf("%+v %v, %d lists - want the list read again at once", rel, err, lists.Load())
	}
	if !strings.Contains(logged.String(), "reading the list again") {
		t.Errorf("logged %q", logged.String())
	}
}
