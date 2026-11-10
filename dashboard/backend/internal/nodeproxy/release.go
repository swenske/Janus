// Remote-update Phase 5, automatic detection (2026-09-29, now that real
// tagged GitHub Releases exist - remote-update Phase 4): tells the
// per-node page what the latest known Janus release is, so it can show
// whether a given node is running it and offer to pre-fill the
// "Update" panel (lifecycle.go) with that release's own download URL/
// checksum - closing the loop the user originally asked for ("le
// controller serait capable de détecter les nodes qui peuvent être mis
// à jour").
//
// Served from each per-node listener, not the main dashboard port
// (:8080) - this information isn't actually per-node at all, but the
// per-node page (static/index.html) can't reach the main port's own
// API without a cross-origin trust problem (see nodeproxy.go's own
// package doc comment for why every per-node page is deliberately its
// own origin) - so it's duplicated here instead, cheaply, behind a
// shared, process-wide cache (not per-listener) so having several
// nodes' pages open at once doesn't multiply real requests to GitHub's
// API.
package nodeproxy

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/swenske/Janus/dashboard/updater/updaterapi"
	"github.com/swenske/Janus/internal/calver"
)

// ReleasesURL lists this project's releases - by the date of the commit
// each one's tag names, which fetchLatestRelease sorts by version: a
// maintenance release (a fix of the previous release, tagged on its own
// branch after the newest - docs/contributing/releasing.md) comes first
// in GitHub's list, though it is older. Not /latest: the
// pre-releases are needed too (ReleaseInfo.Prerelease), and every release
// up to v2026.09.30-2 was one. dashboardd's -releases-url replaces it (a
// mirror, or a test's fake).
var ReleasesURL = "https://api.github.com/repos/swenske/Janus/releases"

// releaseCacheTTL bounds how long a fetched result is reused before
// asking GitHub again - short enough that a fresh release shows up
// reasonably quickly, long enough that several nodes' pages open at
// once (or one page's own 5s auto-refresh loop) don't re-fetch on
// every single poll. GitHub's anonymous API rate limit (60/hour per
// source IP) is the real constraint this is sized against, not UX.
const releaseCacheTTL = 10 * time.Minute

// releaseRefreshMin is how soon a refresh asked by hand (WithFreshReleases:
// a page's Refresh button) fetches the list again: GitHub allows 60 calls
// an hour without a token, shared by every page and the Controller's own
// checks.
const releaseRefreshMin = 30 * time.Second

// errorCacheTTL is how long a failed fetch is answered from the cache:
// long enough that every open page's refresh doesn't turn an outage into
// a request storm, short enough that a retry soon after a passing
// failure - a slow answer from GitHub - gets a fresh try. (It was
// releaseCacheTTL: one timeout failed every machine creation for ten
// minutes.)
var errorCacheTTL = 15 * time.Second

// GitHub's release downloads answered 504 now and then for hours
// (2026-11-08 and 09) while its API answered: every fetch read a few
// assets again, and one 504 among them dropped the whole list - the
// Controller offered no update the morning a release was promoted.
// So: a release's small assets are read once (assetTexts, securityByTag
// - a published release's files don't change); a release whose
// security.json doesn't come is offered all the same, the list fetched
// again partialRetry later to read it; and a failed fetch keeps the last
// list for staleTTL, tried again every staleRetry (GitHub's anonymous
// API allows 60 calls an hour).
var (
	partialRetry = 3 * time.Minute
	staleTTL     = 6 * time.Hour
	staleRetry   = 2 * time.Minute
)

// ReleaseInfo is the newest release - never a pre-release: those are for
// testing (docs/contributing/releasing.md), never offered as an update.
type ReleaseInfo struct {
	TagName       string `json:"tag_name"`
	HTMLURL       string `json:"html_url"`
	PublishedAt   string `json:"published_at"`
	BundleBaseURL string `json:"bundle_base_url"`
	SHA256        string `json:"sha256"`
	// ControllerImage is the release's Controller image pinned to its
	// digest ("swenske/janus-controller:vX@sha256:..."), from its
	// controller-image.txt asset - empty for releases before that asset.
	ControllerImage string `json:"controller_image,omitempty"`

	// Prerelease is the newest pre-release when it is newer than the
	// release: shown on the Update pages only, installed on purpose (no
	// badge, no "update available") - and what a node running a
	// pre-release changes its image to (PrereleaseTrack). Nil otherwise.
	Prerelease *ReleaseInfo `json:"prerelease,omitempty"`

	// MaintenanceOf is the release a maintenance release fixes, as its
	// name says ("… - maintenance of vX", image-build.yml): a release
	// older than the newest, on that release's day (docs/contributing/
	// releasing.md). Empty for any other release.
	MaintenanceOf string `json:"maintenance_of,omitempty"`

	// tags are every published release, newest first; security what the
	// ones with a security.json asset fix (SecurityUpdate) - pre-releases
	// are in neither, only in prereleases.
	tags        []string
	security    map[string][]releaseFix
	prereleases map[string]bool
	// maintenance are the maintenance releases, newest first
	// (MaintenanceFor).
	maintenance []*ReleaseInfo
	// partial: a release's security.json couldn't be read (partialRetry).
	partial bool
}

// maintenanceName is how a maintenance release's name says which release
// it fixes (image-build.yml's RELEASE_NAME).
var maintenanceName = regexp.MustCompile(` - maintenance of (v\d{4}\.\d{2}\.\d{2}(?:-\d+)?)\b`)

// MaintenanceFor is the maintenance release a node or Controller running
// version may install: the newest one of version's day, newer than it -
// a fix of that release, or of a maintenance release of it. Offered
// apart from the latest release, which stays the update (a node on the
// newest release is never offered one). Nil for a version that isn't a
// release (a development build) or has none.
func (r *ReleaseInfo) MaintenanceFor(version string) *ReleaseInfo {
	if !updaterapi.IsRelease(version) {
		return nil
	}
	for _, m := range r.maintenance {
		if newer, _ := updaterapi.Newer(m.TagName, version); newer && calver.SameDay(m.TagName, version) {
			return m
		}
	}
	return nil
}

// IsPrerelease reports whether version is a published pre-release - one
// that wasn't promoted to a release (yet).
func (r *ReleaseInfo) IsPrerelease(version string) bool { return r.prereleases[version] }

// PrereleaseFor is the pre-release to show a node or Controller running
// version: the newest one, when newer than both the release and version.
func (r *ReleaseInfo) PrereleaseFor(version string) *ReleaseInfo {
	if r.Prerelease == nil || !updateAvailable(r.Prerelease.TagName, version) {
		return nil
	}
	return r.Prerelease
}

// PrereleaseTrack is the pre-release a node running version stays on
// when its image changes (POST /api/factory/update): for a node running a
// pre-release, that one when it's the newest, else the newest
// (PrereleaseFor). Nil for a node on a release or a development build,
// and for one on a pre-release older than the release with no newer
// pre-release: those take the release, as before.
func (r *ReleaseInfo) PrereleaseTrack(version string) *ReleaseInfo {
	if !r.IsPrerelease(version) {
		return nil
	}
	if pre := r.PrereleaseFor(version); pre != nil {
		return pre
	}
	if r.Prerelease != nil && r.Prerelease.TagName == version {
		return r.Prerelease
	}
	return nil
}

// releaseFix is one fix of a release's security.json: whom it reaches
// (target "node" or "controller"; extension, when only nodes with it;
// component, variant and audience, when only nodes on a HAProxy branch or
// kernel track), how severe.
type releaseFix struct {
	// id names the fix ("JANUS-2026-007", "CVE-…"): the same in every
	// release's security.json that lists it.
	id                          string
	target, extension, severity string
	component, variant          string
	audience                    []string
}

// NodeImage is what a node's image is built with, as far as security
// fixes tell nodes apart: its extensions, and the HAProxy branch and
// kernel track its schematic pins ("" for each release's default -
// every node whose image doesn't say).
type NodeImage struct {
	Extensions      []string
	HAProxy, Kernel string
}

// reaches says whether the fix concerns a node built so: one to an
// extension, a node that has it; one to a HAProxy branch or kernel track,
// the nodes its audience names - those taking the default, or those
// pinned to that variant.
func (f releaseFix) reaches(img NodeImage) bool {
	if f.extension != "" && !slices.Contains(img.Extensions, f.extension) {
		return false
	}
	if f.component == "" || len(f.audience) == 0 {
		return true
	}
	pinned := img.HAProxy
	if f.component == "kernel" {
		pinned = img.Kernel
	}
	if pinned == "" {
		return slices.Contains(f.audience, "default")
	}
	return slices.Contains(f.audience, "pinned") && f.variant == pinned
}

// SecurityUpdate is the most severe of the vulnerabilities the releases
// after version fix for target - "node" (built as img says) or
// "controller" - and the newest of those releases: from their
// security.json assets (hack/upstream, docs/upstreams.md), "critical",
// "high", "medium", "low" or "unknown" (unrated). A fix to an extension
// counts only for a node that has it, one to a HAProxy branch or kernel
// track only for the nodes on it (releaseFix.reaches). Empty when they
// fix none, or when version isn't a published release or pre-release (a
// development build: nothing to compare). Pre-releases never count: their
// fixes reach the releases with the promotion. For a node on a pre-release,
// the releases newer than it - each security.json says what it fixes since
// the release before it, some of which the pre-release may already have.
// A fix the node's own release lists isn't missing: a maintenance release
// (a fix of an older release) carries a fix a newer release lists again.
func (r *ReleaseInfo) SecurityUpdate(version, target string, img NodeImage) (severity, release string) {
	released := slices.Contains(r.tags, version)
	if !released && !r.prereleases[version] {
		return "", ""
	}
	has := map[string]bool{}
	for _, f := range r.security[version] {
		if f.id != "" {
			has[f.id] = true
		}
	}
	for _, tag := range r.tags {
		if tag == version {
			break
		}
		if newer, known := updaterapi.Newer(tag, version); !released && (!known || !newer) {
			break
		}
		for _, f := range r.security[tag] {
			if f.target != target || !f.reaches(img) || has[f.id] {
				continue
			}
			if release == "" {
				release = tag
			}
			if severity == "" || severityRank(f.severity) > severityRank(severity) {
				severity = f.severity
			}
		}
	}
	return severity, release
}

func severityRank(s string) int {
	return max(slices.Index([]string{"unknown", "low", "medium", "high", "critical"}, s), 0)
}

// securityDoc is the part of a release's security.json read here.
type securityDoc struct {
	Updates []struct {
		Target    string   `json:"target"`
		Extension string   `json:"extension"`
		Component string   `json:"component"`
		Variant   string   `json:"variant"`
		Audience  []string `json:"audience"`
		Fixes     []struct {
			ID       string `json:"id"`
			Severity string `json:"severity"`
		} `json:"fixes"`
	} `json:"updates"`
}

// securityByTag caches each release's security.json, read once: a
// published release doesn't change.
var securityByTag = struct {
	sync.Mutex
	m map[string][]releaseFix
}{m: map[string][]releaseFix{}}

// releaseSecurity is the fixes of a release's security.json.
func releaseSecurity(ctx context.Context, tag, url string) ([]releaseFix, error) {
	securityByTag.Lock()
	fixes, ok := securityByTag.m[tag]
	securityByTag.Unlock()
	if ok {
		return fixes, nil
	}
	data, err := fetchAsset(ctx, url, 1<<20)
	if err != nil {
		return nil, err
	}
	var doc securityDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s's security.json: %w", tag, err)
	}
	fixes = []releaseFix{}
	for _, u := range doc.Updates {
		for _, f := range u.Fixes {
			fixes = append(fixes, releaseFix{id: f.ID, target: u.Target, extension: u.Extension, severity: cmp.Or(f.Severity, "unknown"),
				component: u.Component, variant: u.Variant, audience: u.Audience})
		}
	}
	securityByTag.Lock()
	securityByTag.m[tag] = fixes
	securityByTag.Unlock()
	releaseCacheDirty.Store(true)
	return fixes, nil
}

// LatestRelease is the newest release, cached like the per-node pages'.
func LatestRelease(ctx context.Context) (*ReleaseInfo, error) {
	return getLatestRelease(ctx)
}

type freshReleasesKey struct{}

// WithFreshReleases is ctx for a request asking to see a release just
// published - a page's Refresh button: the list is fetched again unless
// it's younger than releaseRefreshMin, not releaseCacheTTL.
func WithFreshReleases(ctx context.Context) context.Context {
	return context.WithValue(ctx, freshReleasesKey{}, true)
}

// RefreshAsked is r's context, WithFreshReleases when r asks for it
// (?refresh).
func RefreshAsked(r *http.Request) context.Context {
	if r.URL.Query().Has("refresh") {
		return WithFreshReleases(r.Context())
	}
	return r.Context()
}

// KnownPrereleases are the pre-releases the last release list fetched
// named, sorted - never waiting on GitHub (the node pages and the fleet's
// status keep that list fresh): nil while nothing was fetched yet, or a
// fetch is running.
func KnownPrereleases() []string {
	if !releaseCache.mu.TryLock() {
		return nil
	}
	defer releaseCache.mu.Unlock()
	if releaseCache.data == nil {
		return nil
	}
	return slices.Sorted(maps.Keys(releaseCache.data.prereleases))
}

var releaseCache struct {
	mu        sync.Mutex
	url       string // ReleasesURL when fetched
	data      *ReleaseInfo
	err       error
	fetchedAt time.Time // data's or err's
	triedAt   time.Time // the last fetch, its answer kept or not
	failed    bool      // the last fetch failed, data kept (staleTTL)
}

func registerReleaseRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/latest-release", func(w http.ResponseWriter, r *http.Request) {
		info, err := getLatestRelease(r.Context())
		if err != nil {
			http.Error(w, fmt.Sprintf("fetch latest release: %v", err), http.StatusBadGateway)
			return
		}
		writeJSONBody(w, http.StatusOK, info)
	})
}

// getLatestRelease returns the cached result if it's still fresh,
// otherwise fetches and caches a new one - a genuine upstream error
// (network failure, GitHub API error, no releases published yet) is
// cached too, briefly, so a real outage doesn't turn into a request
// storm from every open per-node page's own refresh loop. A refresh asked
// by hand (WithFreshReleases) fetches sooner, and when that fetch fails,
// the list still within releaseCacheTTL is kept.
func getLatestRelease(ctx context.Context) (*ReleaseInfo, error) {
	releaseCache.mu.Lock()
	defer releaseCache.mu.Unlock()

	maxAge := releaseCacheTTL
	if fresh, _ := ctx.Value(freshReleasesKey{}).(bool); fresh {
		maxAge = releaseRefreshMin
	}
	same := releaseCache.url == ReleasesURL
	retry := releaseRefreshMin
	if releaseCache.failed {
		retry = staleRetry
	}
	if same && releaseCache.data != nil && (time.Since(releaseCache.fetchedAt) < maxAge || time.Since(releaseCache.triedAt) < retry) {
		return releaseCache.data, nil
	}
	if same && releaseCache.err != nil && time.Since(releaseCache.fetchedAt) < errorCacheTTL {
		return nil, releaseCache.err
	}

	fctx, cancel := sharedFetchContext(ctx)
	defer cancel()
	info, err := fetchLatestRelease(fctx)
	now := time.Now()
	releaseCache.triedAt = now
	if err != nil {
		if same && releaseCache.data != nil && now.Sub(releaseCache.fetchedAt) < staleTTL {
			if !releaseCache.failed {
				log.Printf("releases: %v - the list read %s ago kept, tried again in %s", err, now.Sub(releaseCache.fetchedAt).Round(time.Second), staleRetry)
			}
			releaseCache.failed = true
			return releaseCache.data, nil
		}
		releaseCache.url, releaseCache.fetchedAt = ReleasesURL, now
		releaseCache.err, releaseCache.failed = err, false
		releaseCache.data = nil
		return nil, err
	}
	releaseCache.url, releaseCache.fetchedAt = ReleasesURL, now
	if info.partial {
		// Fetched again partialRetry from now, for what's missing.
		releaseCache.fetchedAt = now.Add(partialRetry - releaseCacheTTL)
	}
	releaseCache.data = info
	releaseCache.err, releaseCache.failed = nil, false
	return info, nil
}

// sharedFetchContext is the context of a fetch whose result is cached
// and shared by every node's page: detached from the request that
// happens to trigger it, so a browser going away (page change, closed
// tab) can't leave "context canceled" in the cache for everyone.
func sharedFetchContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
}

type ghRelease struct {
	TagName     string `json:"tag_name"`
	Name        string `json:"name"`
	HTMLURL     string `json:"html_url"`
	PublishedAt string `json:"published_at"`
	Prerelease  bool   `json:"prerelease"`
	Draft       bool   `json:"draft"`
	Assets      []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

// fetchLatestRelease calls GitHub for real - no auth (this project's
// releases are public), the same anonymous access every node's own
// fetchBundleFile already relies on to download a release's assets.
func fetchLatestRelease(ctx context.Context) (*ReleaseInfo, error) {
	// GitHub gives 30 releases a page by default: with pre-releases among
	// them, and several releases a day, a node a few weeks old would fall
	// off the list (no security badge for it).
	u, err := url.Parse(ReleasesURL)
	if err != nil {
		return nil, err
	}
	if q := u.Query(); !q.Has("per_page") {
		q.Set("per_page", "100")
		u.RawQuery = q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned %s", resp.Status)
	}

	var releases []ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return nil, fmt.Errorf("decode GitHub API response: %w", err)
	}
	// Newest version first, not GitHub's order (ReleasesURL): the first
	// release is the latest, the first pre-release the newest.
	calver.SortNewestFirst(releases, func(r ghRelease) string { return r.TagName })
	defer saveReleaseCache()

	var info *ReleaseInfo
	var pre *ghRelease // the newest pre-release
	prereleases := map[string]bool{}
	for _, rel := range releases {
		if rel.Draft {
			continue
		}
		if rel.Prerelease {
			prereleases[rel.TagName] = true
			if pre == nil {
				pre = &rel
			}
			continue
		}
		if info != nil {
			info.tags = append(info.tags, rel.TagName)
			if m := maintenanceName.FindStringSubmatch(rel.Name); m != nil {
				mi, err := releaseInfo(ctx, rel)
				if err != nil {
					log.Printf("releases: maintenance release %s: %v - read again in %s", rel.TagName, err, partialRetry)
					info.partial = true
					continue
				}
				mi.MaintenanceOf = m[1]
				info.maintenance = append(info.maintenance, mi)
			}
			continue
		}
		if info, err = releaseInfo(ctx, rel); err != nil {
			return nil, err
		}
		info.tags = []string{rel.TagName}
		info.security = map[string][]releaseFix{}
	}
	if info == nil {
		return nil, fmt.Errorf("no published releases found")
	}
	info.prereleases = prereleases
	if pre != nil && updateAvailable(pre.TagName, info.TagName) {
		if info.Prerelease, err = releaseInfo(ctx, *pre); err != nil {
			log.Printf("releases: pre-release %s: %v - read again in %s", pre.TagName, err, partialRetry)
			info.Prerelease, info.partial = nil, true
		}
	}
	// The security.json not read yet, last and newest first, with what
	// the fetch has left (#553; v2026.11.09-3's lab campaign: a first
	// start without release-cache.json, GitHub 4 s a request, spent the
	// whole fetch on ~60 of them and knew no release for 5 minutes). One
	// that doesn't come leaves the list partial - read again soon.
	fetchSecurities(ctx, releases)
	for _, rel := range releases {
		if !rel.Draft && !rel.Prerelease && slices.Contains(info.tags, rel.TagName) {
			info.readSecurity(rel)
		}
	}
	return info, nil
}

// releaseInfo is what a release offers: its bundle, the bundle's
// checksum, the Controller's image.
func releaseInfo(ctx context.Context, rel ghRelease) (*ReleaseInfo, error) {
	// rel.HTMLURL is ".../releases/tag/<tag>" - trimming the "/tag/<tag>"
	// suffix leaves ".../releases", so only "/download/<tag>" (not
	// "/releases/download/<tag>", which would double up "releases") needs
	// appending.
	info := &ReleaseInfo{
		TagName:       rel.TagName,
		HTMLURL:       rel.HTMLURL,
		PublishedAt:   rel.PublishedAt,
		BundleBaseURL: strings.TrimSuffix(rel.HTMLURL, "/tag/"+rel.TagName) + "/download/" + rel.TagName,
	}
	for _, asset := range rel.Assets {
		var err error
		switch asset.Name {
		case "rootfs.squashfs.sha256":
			info.SHA256, err = cachedAssetText(ctx, asset.BrowserDownloadURL)
		case "controller-image.txt":
			info.ControllerImage, err = cachedAssetText(ctx, asset.BrowserDownloadURL)
		}
		if err != nil {
			return nil, fmt.Errorf("fetch %s: %w", asset.Name, err)
		}
	}
	return info, nil
}

// readSecurity takes a release's security.json from securityByTag
// (fetchSecurities read it) - one that didn't come leaves the release's
// fixes out of this list (partial), the release offered all the same.
func (r *ReleaseInfo) readSecurity(rel ghRelease) {
	if securityURL(rel) == "" {
		return
	}
	securityByTag.Lock()
	fixes, ok := securityByTag.m[rel.TagName]
	securityByTag.Unlock()
	if !ok {
		r.partial = true
		return
	}
	r.security[rel.TagName] = fixes
}

// securityURL is a release's security.json, "" when it has none.
func securityURL(rel ghRelease) string {
	for _, asset := range rel.Assets {
		if asset.Name == "security.json" {
			return asset.BrowserDownloadURL
		}
	}
	return ""
}

// securityFetches bounds how many security.json are read at once, and
// securityFetchTimeout each one.
const (
	securityFetches      = 8
	securityFetchTimeout = 20 * time.Second
)

// fetchSecurities reads the security.json of every published release
// that has one and isn't in securityByTag yet, securityFetches at a time;
// one that doesn't come is said in the log and read again next time.
func fetchSecurities(ctx context.Context, releases []ghRelease) {
	sem := make(chan struct{}, securityFetches)
	var wg sync.WaitGroup
	for _, rel := range releases {
		u := securityURL(rel)
		if rel.Draft || rel.Prerelease || u == "" {
			continue
		}
		securityByTag.Lock()
		_, ok := securityByTag.m[rel.TagName]
		securityByTag.Unlock()
		if ok {
			continue
		}
		// The slot taken here, in the list's order: the newest
		// releases' first, when the time runs out.
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return
		}
		wg.Add(1)
		go func(tag, u string) {
			defer wg.Done()
			defer func() { <-sem }()
			fctx, cancel := context.WithTimeout(ctx, securityFetchTimeout)
			defer cancel()
			if _, err := releaseSecurity(fctx, tag, u); err != nil {
				log.Printf("releases: %s's security.json: %v - read again in %s", tag, err, partialRetry)
			}
		}(rel.TagName, u)
	}
	wg.Wait()
}

// assetTexts are the small text assets read off releases, by URL: read
// once, a published release's files don't change.
var assetTexts = struct {
	sync.Mutex
	m map[string]string
}{m: map[string]string{}}

// cachedAssetText is fetchAssetText, read once per URL.
func cachedAssetText(ctx context.Context, url string) (string, error) {
	assetTexts.Lock()
	text, ok := assetTexts.m[url]
	assetTexts.Unlock()
	if ok {
		return text, nil
	}
	text, err := fetchAssetText(ctx, url)
	if err != nil {
		return "", err
	}
	assetTexts.Lock()
	assetTexts.m[url] = text
	assetTexts.Unlock()
	releaseCacheDirty.Store(true)
	return text, nil
}

// fetchAssetText downloads a small text asset (the sha256 checksum
// file) and returns its trimmed content - not trusted from the GitHub
// API's own asset metadata (digest fields aren't reliably populated
// for every upload path), fetched directly instead, the same "verify,
// don't assume" discipline the release-bundle files themselves are
// held to.
func fetchAssetText(ctx context.Context, url string) (string, error) {
	data, err := fetchAsset(ctx, url, 4096)
	return strings.TrimSpace(string(data)), err
}

// fetchAsset downloads a release asset, at most limit bytes of it.
func fetchAsset(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: unexpected status %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}
