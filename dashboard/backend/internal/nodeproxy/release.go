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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// ReleasesURL lists this project's releases, newest first -
// GitHub's documented ordering for this endpoint. Not /latest: that one
// skips pre-releases, which every release up to v2026.09.30-2 was.
// dashboardd's -releases-url replaces it (a mirror, or a test's fake).
var ReleasesURL = "https://api.github.com/repos/swenske/Janus/releases"

// releaseCacheTTL bounds how long a fetched result is reused before
// asking GitHub again - short enough that a fresh release shows up
// reasonably quickly, long enough that several nodes' pages open at
// once (or one page's own 5s auto-refresh loop) don't re-fetch on
// every single poll. GitHub's anonymous API rate limit (60/hour per
// source IP) is the real constraint this is sized against, not UX.
const releaseCacheTTL = 10 * time.Minute

// errorCacheTTL is how long a failed fetch is answered from the cache:
// long enough that every open page's refresh doesn't turn an outage into
// a request storm, short enough that a retry soon after a passing
// failure - a slow answer from GitHub - gets a fresh try. (It was
// releaseCacheTTL: one timeout failed every machine creation for ten
// minutes.)
var errorCacheTTL = 15 * time.Second

// ReleaseInfo is the newest release.
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

	// tags are every published release, newest first; security what the
	// ones with a security.json asset fix (SecurityUpdate).
	tags     []string
	security map[string]map[string]string
}

// SecurityUpdate is the most severe of the vulnerabilities the releases
// after version fix for target - "node" or "controller" - and the newest
// of those releases: from their security.json assets (hack/upstream,
// docs/upstreams.md), "critical", "high", "medium", "low" or "unknown"
// (unrated). Empty when they fix none, or when version isn't a published
// release (a development build: nothing to compare).
func (r *ReleaseInfo) SecurityUpdate(version, target string) (severity, release string) {
	if !slices.Contains(r.tags, version) {
		return "", ""
	}
	for _, tag := range r.tags {
		if tag == version {
			break
		}
		sev := r.security[tag][target]
		if sev == "" {
			continue
		}
		if release == "" {
			release = tag
		}
		if severity == "" || severityRank(sev) > severityRank(severity) {
			severity = sev
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
		Target string `json:"target"`
		Fixes  []struct {
			Severity string `json:"severity"`
		} `json:"fixes"`
	} `json:"updates"`
}

// securityByTag caches each release's security.json, read once: a
// published release doesn't change.
var securityByTag = struct {
	sync.Mutex
	m map[string]map[string]string
}{m: map[string]map[string]string{}}

// releaseSecurity is the most severe fix of a release's security.json for
// each target.
func releaseSecurity(ctx context.Context, tag, url string) (map[string]string, error) {
	securityByTag.Lock()
	sev, ok := securityByTag.m[tag]
	securityByTag.Unlock()
	if ok {
		return sev, nil
	}
	data, err := fetchAsset(ctx, url, 1<<20)
	if err != nil {
		return nil, err
	}
	var doc securityDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s's security.json: %w", tag, err)
	}
	sev = map[string]string{}
	for _, u := range doc.Updates {
		for _, f := range u.Fixes {
			s := f.Severity
			if s == "" {
				s = "unknown"
			}
			if cur, ok := sev[u.Target]; !ok || severityRank(s) > severityRank(cur) {
				sev[u.Target] = s
			}
		}
	}
	securityByTag.Lock()
	securityByTag.m[tag] = sev
	securityByTag.Unlock()
	return sev, nil
}

// LatestRelease is the newest release, cached like the per-node pages'.
func LatestRelease(ctx context.Context) (*ReleaseInfo, error) {
	return getLatestRelease(ctx)
}

var releaseCache struct {
	mu        sync.Mutex
	data      *ReleaseInfo
	err       error
	fetchedAt time.Time
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
// storm from every open per-node page's own refresh loop.
func getLatestRelease(ctx context.Context) (*ReleaseInfo, error) {
	releaseCache.mu.Lock()
	defer releaseCache.mu.Unlock()

	if releaseCache.data != nil && time.Since(releaseCache.fetchedAt) < releaseCacheTTL {
		return releaseCache.data, nil
	}
	if releaseCache.err != nil && time.Since(releaseCache.fetchedAt) < errorCacheTTL {
		return nil, releaseCache.err
	}

	fctx, cancel := sharedFetchContext(ctx)
	defer cancel()
	info, err := fetchLatestRelease(fctx)
	releaseCache.fetchedAt = time.Now()
	if err != nil {
		releaseCache.err = err
		releaseCache.data = nil
		return nil, err
	}
	releaseCache.data = info
	releaseCache.err = nil
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ReleasesURL, nil)
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

	var info *ReleaseInfo
	for _, rel := range releases {
		if rel.Draft {
			continue
		}
		if info != nil {
			info.tags = append(info.tags, rel.TagName)
			if err := info.readSecurity(ctx, rel); err != nil {
				return nil, err
			}
			continue
		}
		// rel.HTMLURL is ".../releases/tag/<tag>" - trimming the
		// "/tag/<tag>" suffix leaves ".../releases", so only
		// "/download/<tag>" (not "/releases/download/<tag>", which
		// would double up "releases") needs appending.
		bundleBaseURL := strings.TrimSuffix(rel.HTMLURL, "/tag/"+rel.TagName) + "/download/" + rel.TagName

		info = &ReleaseInfo{
			TagName:       rel.TagName,
			HTMLURL:       rel.HTMLURL,
			PublishedAt:   rel.PublishedAt,
			BundleBaseURL: bundleBaseURL,
			tags:          []string{rel.TagName},
			security:      map[string]map[string]string{},
		}
		for _, asset := range rel.Assets {
			switch asset.Name {
			case "rootfs.squashfs.sha256":
				info.SHA256, err = fetchAssetText(ctx, asset.BrowserDownloadURL)
			case "controller-image.txt":
				info.ControllerImage, err = fetchAssetText(ctx, asset.BrowserDownloadURL)
			}
			if err != nil {
				return nil, fmt.Errorf("fetch %s: %w", asset.Name, err)
			}
		}
		if err := info.readSecurity(ctx, rel); err != nil {
			return nil, err
		}
	}
	if info == nil {
		return nil, fmt.Errorf("no published releases found")
	}
	return info, nil
}

// readSecurity reads a release's security.json, when it has one.
func (r *ReleaseInfo) readSecurity(ctx context.Context, rel ghRelease) error {
	for _, asset := range rel.Assets {
		if asset.Name == "security.json" {
			sev, err := releaseSecurity(ctx, rel.TagName, asset.BrowserDownloadURL)
			if err != nil {
				return fmt.Errorf("fetch %s's security.json: %w", rel.TagName, err)
			}
			r.security[rel.TagName] = sev
		}
	}
	return nil
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
