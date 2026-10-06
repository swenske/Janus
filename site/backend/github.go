package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/swenske/Janus/internal/schematic"
)

// gitHub is what the site needs from GitHub: the releases (versions),
// each release's extension catalog, and starting the schematic build
// workflow. Read calls are cached: the site answers every visitor, and
// GitHub's anonymous rate limit is 60 requests an hour.
type gitHub struct {
	api      string // https://api.github.com
	repo     string // swenske/Janus
	token    string // fine-grained token, Actions: read/write - only for dispatching and reading runs
	workflow string // schematic-build.yml
	client   *http.Client

	mu       sync.Mutex
	releases []release
	fetched  time.Time
	catalogs map[string]*schematic.Catalog // by version; nil entry: the release has none
	older    map[string]*release           // releases older than Releases' list, by version
}

type release struct {
	Version     string    `json:"version"`
	Name        string    `json:"name"`
	URL         string    `json:"url"`
	PublishedAt time.Time `json:"published_at"`
	Prerelease  bool      `json:"prerelease"`
	// Assets by name -> download URL.
	Assets map[string]string `json:"-"`
	// Schematics: the release publishes the inputs a custom schematic is
	// built from (schematic-catalog.json and friends).
	Schematics bool `json:"schematics"`
}

const releasesTTL = 10 * time.Minute

func newGitHub(api, repo, token, workflow string) *gitHub {
	return &gitHub{api: strings.TrimRight(api, "/"), repo: repo, token: token, workflow: workflow,
		client: &http.Client{Timeout: 30 * time.Second}, catalogs: map[string]*schematic.Catalog{}, older: map[string]*release{}}
}

func (g *gitHub) do(ctx context.Context, method, url string, body any, auth bool) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if auth && g.token != "" {
		req.Header.Set("Authorization", "Bearer "+g.token)
	}
	return g.client.Do(req)
}

// Releases returns the published releases, newest first.
func (g *gitHub) Releases(ctx context.Context) ([]release, error) {
	g.mu.Lock()
	if g.releases != nil && time.Since(g.fetched) < releasesTTL {
		r := g.releases
		g.mu.Unlock()
		return r, nil
	}
	g.mu.Unlock()

	resp, err := g.do(ctx, http.MethodGet, g.api+"/repos/"+g.repo+"/releases?per_page=30", nil, true)
	if err != nil {
		return g.stale(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return g.stale(fmt.Errorf("GitHub releases: %s", resp.Status))
	}
	var raw []rawRelease
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return g.stale(fmt.Errorf("GitHub releases: %w", err))
	}
	out := make([]release, 0, len(raw))
	for _, r := range raw {
		if r.Draft {
			continue
		}
		out = append(out, *r.release())
	}
	g.mu.Lock()
	g.releases, g.fetched = out, time.Now()
	g.mu.Unlock()
	return out, nil
}

// rawRelease is a release as GitHub's API gives it.
type rawRelease struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	HTMLURL     string    `json:"html_url"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	Assets      []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

func (r rawRelease) release() *release {
	rel := &release{Version: r.TagName, Name: r.Name, URL: r.HTMLURL, PublishedAt: r.PublishedAt, Prerelease: r.Prerelease, Assets: map[string]string{}}
	for _, a := range r.Assets {
		rel.Assets[a.Name] = a.URL
	}
	_, rel.Schematics = rel.Assets["schematic-catalog.json"]
	return rel
}

// stale serves the last known releases when GitHub fails, rather than
// taking the site down with it.
func (g *gitHub) stale(err error) ([]release, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.releases != nil {
		return g.releases, nil
	}
	return nil, err
}

// Release returns one release by version: from the newest ones
// (Releases), else asked for by its tag - the last release offering a
// retired HAProxy branch can be older than those.
func (g *gitHub) Release(ctx context.Context, version string) (*release, error) {
	rels, err := g.Releases(ctx)
	if err != nil {
		return nil, err
	}
	for i := range rels {
		if rels[i].Version == version {
			return &rels[i], nil
		}
	}
	g.mu.Lock()
	rel, ok := g.older[version]
	g.mu.Unlock()
	if ok {
		return rel, nil
	}
	resp, err := g.do(ctx, http.MethodGet, g.api+"/repos/"+g.repo+"/releases/tags/"+url.PathEscape(version), nil, true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub release %s: %s", version, resp.Status)
	}
	var r rawRelease
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("GitHub release %s: %w", version, err)
	}
	if r.Draft {
		return nil, errNotFound
	}
	rel = r.release()
	g.mu.Lock()
	g.older[version] = rel
	g.mu.Unlock()
	return rel, nil
}

var errNotFound = errors.New("not found")

// Catalog returns a release's extension catalog; nil for a release
// built before schematics existed (the default schematic only).
func (g *gitHub) Catalog(ctx context.Context, version string) (*schematic.Catalog, error) {
	g.mu.Lock()
	c, ok := g.catalogs[version]
	g.mu.Unlock()
	if ok {
		return c, nil
	}
	rel, err := g.Release(ctx, version)
	if err != nil {
		return nil, err
	}
	url, has := rel.Assets["schematic-catalog.json"]
	if has {
		resp, err := g.do(ctx, http.MethodGet, url, nil, false)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("catalog of %s: %s", version, resp.Status)
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			return nil, err
		}
		if c, err = schematic.ParseCatalog(data); err != nil {
			return nil, err
		}
	}
	// Releases are immutable: cached for good (nil included).
	g.mu.Lock()
	g.catalogs[version] = c
	g.mu.Unlock()
	return c, nil
}

// Dispatch starts the schematic build workflow.
func (g *gitHub) Dispatch(ctx context.Context, inputs map[string]string) error {
	if g.token == "" {
		return errors.New("no GitHub token configured: this site can't start builds")
	}
	url := fmt.Sprintf("%s/repos/%s/actions/workflows/%s/dispatches", g.api, g.repo, g.workflow)
	resp, err := g.do(ctx, http.MethodPost, url, map[string]any{"ref": "main", "inputs": inputs}, true)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("start the build: %s %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

// buildRun is a schematic build workflow run, matched by its run name
// ("schematic <id> <version> <arch>").
type buildRun struct {
	Status     string    `json:"status"`     // queued, in_progress, completed
	Conclusion string    `json:"conclusion"` // success, failure, cancelled...
	URL        string    `json:"html_url"`
	CreatedAt  time.Time `json:"created_at"`
}

// LatestRun returns the newest run of the build workflow named name.
func (g *gitHub) LatestRun(ctx context.Context, name string) (*buildRun, error) {
	if g.token == "" {
		return nil, nil
	}
	url := fmt.Sprintf("%s/repos/%s/actions/workflows/%s/runs?per_page=50", g.api, g.repo, g.workflow)
	resp, err := g.do(ctx, http.MethodGet, url, nil, true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("workflow runs: %s", resp.Status)
	}
	var body struct {
		Runs []struct {
			buildRun
			Name string `json:"display_title"`
		} `json:"workflow_runs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	for _, r := range body.Runs { // newest first
		if r.Name == name {
			run := r.buildRun
			return &run, nil
		}
	}
	return nil, nil
}
