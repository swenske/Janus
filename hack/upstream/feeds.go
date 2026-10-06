package main

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// A feed lists the releases an upstream has published, as it writes their
// versions ("6.18.55", "v1.53"). Pre-releases may be among them: the
// component decides whether it follows them.
type feed interface {
	describe() string
	versions(ctx context.Context, f fetcher, pinned string) ([]string, error)
}

// kernelFeed reads kernel.org's releases.json: the latest release of every
// maintained branch, mainline and linux-next left out - only those of one
// moniker ("stable", "longterm") when set.
type kernelFeed struct{ moniker string }

func (k kernelFeed) describe() string {
	if k.moniker != "" {
		return "kernel.org/releases.json, " + k.moniker
	}
	return "kernel.org/releases.json"
}

func (k kernelFeed) versions(ctx context.Context, f fetcher, _ string) ([]string, error) {
	var doc struct {
		Releases []struct {
			Moniker string `json:"moniker"`
			Version string `json:"version"`
		} `json:"releases"`
	}
	if err := getJSON(ctx, f, "https://www.kernel.org/releases.json", &doc); err != nil {
		return nil, err
	}
	var out []string
	for _, r := range doc.Releases {
		if (k.moniker == "" && (r.Moniker == "stable" || r.Moniker == "longterm")) || r.Moniker == k.moniker {
			out = append(out, r.Version)
		}
	}
	return out, nil
}

// haproxyFeed reads the pinned branch's releases.json on haproxy.org: every
// release of that branch, with its sha256.
type haproxyFeed struct{}

func (haproxyFeed) describe() string { return "haproxy.org/download/<branch>/src/releases.json" }

type haproxyReleases struct {
	Branch   string `json:"branch"`
	Releases map[string]struct {
		File   string `json:"file"`
		SHA256 string `json:"sha256"`
	} `json:"releases"`
}

func haproxyBranchReleases(ctx context.Context, f fetcher, v string) (*haproxyReleases, error) {
	branch := mustVersion(v).branch(2)
	var doc haproxyReleases
	err := getJSON(ctx, f, "https://www.haproxy.org/download/"+branch+"/src/releases.json", &doc)
	return &doc, err
}

func (haproxyFeed) versions(ctx context.Context, f fetcher, pinned string) ([]string, error) {
	doc, err := haproxyBranchReleases(ctx, f, pinned)
	if err != nil {
		return nil, err
	}
	var out []string
	for v := range doc.Releases {
		out = append(out, v)
	}
	return out, nil
}

// githubReleases lists a GitHub repository's published releases by tag.
type githubReleases struct{ repo string }

func (g githubReleases) describe() string { return "github.com/" + g.repo + " releases" }

func (g githubReleases) versions(ctx context.Context, f fetcher, _ string) ([]string, error) {
	rels, err := listGitHubReleases(ctx, f, g.repo)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range rels {
		if !r.Draft {
			out = append(out, r.TagName)
		}
	}
	return out, nil
}

type ghRelease struct {
	TagName    string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Assets     []struct {
		Name   string `json:"name"`
		URL    string `json:"browser_download_url"`
		Digest string `json:"digest"`
	} `json:"assets"`
}

func listGitHubReleases(ctx context.Context, f fetcher, repo string) ([]ghRelease, error) {
	var rels []ghRelease
	err := getJSON(ctx, f, "https://api.github.com/repos/"+repo+"/releases?per_page=100", &rels)
	return rels, err
}

func gitHubRelease(ctx context.Context, f fetcher, repo, tag string) (*ghRelease, error) {
	var rel ghRelease
	err := getJSON(ctx, f, "https://api.github.com/repos/"+repo+"/releases/tags/"+tag, &rel)
	return &rel, err
}

// githubTags lists a GitHub repository's tags, for upstreams that tag
// releases without publishing GitHub releases.
type githubTags struct{ repo string }

func (g githubTags) describe() string { return "github.com/" + g.repo + " tags" }

func (g githubTags) versions(ctx context.Context, f fetcher, _ string) ([]string, error) {
	var tags []struct {
		Name string `json:"name"`
	}
	if err := getJSON(ctx, f, "https://api.github.com/repos/"+g.repo+"/tags?per_page=100", &tags); err != nil {
		return nil, err
	}
	out := make([]string, len(tags))
	for i, t := range tags {
		out[i] = t.Name
	}
	return out, nil
}

// htmlIndex reads versions off a download directory's listing: re's first
// group is the version.
type htmlIndex struct {
	url string
	re  *regexp.Regexp
}

func (h htmlIndex) describe() string { return h.url }

func (h htmlIndex) versions(ctx context.Context, f fetcher, _ string) ([]string, error) {
	page, err := f.get(ctx, h.url)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range h.re.FindAllStringSubmatch(string(page), -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no version matches %s", h.url, h.re)
	}
	return out, nil
}

// hashicorpFeed reads HashiCorp's releases API, open-source builds only.
type hashicorpFeed struct{ product string }

func (h hashicorpFeed) describe() string { return "api.releases.hashicorp.com/" + h.product }

func (h hashicorpFeed) versions(ctx context.Context, f fetcher, _ string) ([]string, error) {
	var rels []struct {
		Version      string `json:"version"`
		IsPrerelease bool   `json:"is_prerelease"`
		LicenseClass string `json:"license_class"`
	}
	url := "https://api.releases.hashicorp.com/v1/releases/" + h.product + "?limit=20&license_class=oss"
	if err := getJSON(ctx, f, url, &rels); err != nil {
		return nil, err
	}
	var out []string
	for _, r := range rels {
		if r.LicenseClass == "oss" && !strings.Contains(r.Version, "+") {
			out = append(out, r.Version)
		}
	}
	return out, nil
}

// githubCommit is the head of a branch, for an upstream pinned by commit
// because it has no releases at all.
type githubCommit struct{ repo, branch string }

func (g githubCommit) describe() string { return "github.com/" + g.repo + " " + g.branch }

func (g githubCommit) versions(ctx context.Context, f fetcher, _ string) ([]string, error) {
	var c struct {
		SHA string `json:"sha"`
	}
	if err := getJSON(ctx, f, "https://api.github.com/repos/"+g.repo+"/commits/"+g.branch, &c); err != nil {
		return nil, err
	}
	return []string{c.SHA}, nil
}
