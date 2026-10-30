// Command releases says which of this project's tags are releases: git
// can't, GitHub's release list does. A pre-release is a release tag
// published with GitHub's prerelease flag, made a release later by
// clearing it - the same build (docs/contributing/releasing.md). Whatever
// follows "the previous release" (the security notes, the docs' latest
// channel, the upgrade-from-release test) must skip pre-releases.
//
// Usage:
//
//	go run ./hack/releases newest [-reachable-from REF]
//	    the newest release - never a pre-release or a draft; with
//	    -reachable-from, the newest whose tag is an ancestor of REF
//	go run ./hack/releases check-prerelease VERSION
//	    fails unless VERSION is newer than the newest release: an older
//	    pre-release could never become one
//	go run ./hack/releases info VERSION
//	    VERSION's GitHub release as JSON (id, name, prerelease, draft,
//	    the newest release and whether VERSION is newer, the assets with
//	    their URL and SHA-256), for the promotion workflow
//	go run ./hack/releases publish -commit SHA -name NAME -notes FILE [-prerelease] VERSION GLOB...
//	    publishes VERSION's GitHub release with the files GLOB names, a
//	    draft until every one is there, and never changes one already
//	    published (publish.go), for image-build's publishing job
//
// GITHUB_REPOSITORY (swenske/Janus by default), GITHUB_API_URL
// (https://api.github.com) and GITHUB_TOKEN (optional - publish needs
// one) as in a workflow.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/swenske/Janus/dashboard/updater/updaterapi"
)

type release struct {
	ID         int64   `json:"id"`
	Tag        string  `json:"tag_name"`
	Commitish  string  `json:"target_commitish"`
	Name       string  `json:"name"`
	Draft      bool    `json:"draft"`
	Prerelease bool    `json:"prerelease"`
	HTMLURL    string  `json:"html_url"`
	UploadURL  string  `json:"upload_url"` // "https://uploads.github.com/.../assets{?name,label}"
	Assets     []asset `json:"assets"`
}

type asset struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	State  string `json:"state"` // "uploaded", else an upload that didn't finish
	URL    string `json:"browser_download_url"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"` // "sha256:<hex>"
}

// client lists the releases of repo through GitHub's API at api.
type client struct {
	api, repo, token string
	http             *http.Client
}

func newClient() *client {
	c := &client{api: "https://api.github.com", repo: "swenske/Janus", token: os.Getenv("GITHUB_TOKEN"), http: &http.Client{Timeout: 30 * time.Second}}
	if v := os.Getenv("GITHUB_API_URL"); v != "" {
		c.api = strings.TrimRight(v, "/")
	}
	if v := os.Getenv("GITHUB_REPOSITORY"); v != "" {
		c.repo = v
	}
	return c
}

// backoff is the wait before a second try, doubled for a third (0 in
// tests).
var backoff = 2 * time.Second

func (c *client) get(ctx context.Context, path string, out any) error {
	return c.send(ctx, http.MethodGet, path, nil, out)
}

// send makes a request of the API with in as its JSON body (nil: none)
// and decodes the answer into out (nil: ignored). GitHub's API answers
// a 5xx now and then: tried again, but a POST only once - a second
// might create the release twice.
func (c *client) send(ctx context.Context, method, path string, in, out any) error {
	tries := 3
	if method == http.MethodPost {
		tries = 1
	}
	var err error
	for attempt := range tries {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * backoff)
		}
		var retry bool
		if retry, err = c.try(ctx, method, path, in, out); !retry {
			return err
		}
	}
	return err
}

func (c *client) try(ctx context.Context, method, path string, in, out any) (retry bool, err error) {
	var reqBody io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return false, err
		}
		reqBody = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.api+"/repos/"+c.repo+path, reqBody)
	if err != nil {
		return false, err
	}
	c.headers(req)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return true, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return true, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		err := fmt.Errorf("GitHub API %s %s: %s%s", method, path, resp.Status, apiMessage(body))
		if resp.StatusCode == http.StatusNotFound {
			err = fmt.Errorf("%w: %w", errNotFound, err)
		}
		return resp.StatusCode >= 500, err
	}
	if out == nil || len(body) == 0 {
		return false, nil
	}
	return false, json.Unmarshal(body, out)
}

func (c *client) headers(req *http.Request) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}

// apiMessage is what GitHub said of a refused request (its JSON
// "message", or the start of the page it answered), ": "-prefixed.
func apiMessage(body []byte) string {
	var m struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &m) == nil && m.Message != "" {
		return ": " + m.Message
	}
	if s := strings.Join(strings.Fields(string(body)), " "); s != "" {
		return ": " + s[:min(len(s), 200)]
	}
	return ""
}

var errNotFound = errors.New("not found")

// releases are every release, as GitHub lists them (newest first).
func (c *client) releases(ctx context.Context) ([]release, error) {
	var all []release
	for page := 1; page <= 20; page++ {
		var rels []release
		if err := c.get(ctx, fmt.Sprintf("/releases?per_page=100&page=%d", page), &rels); err != nil {
			return nil, err
		}
		all = append(all, rels...)
		if len(rels) < 100 {
			break
		}
	}
	return all, nil
}

// stable are the releases' tags - no draft, no pre-release - newest
// first by version, not by GitHub's order (creation).
func stable(rels []release) []string {
	var tags []string
	for _, r := range rels {
		if !r.Draft && !r.Prerelease && updaterapi.IsRelease(r.Tag) {
			tags = append(tags, r.Tag)
		}
	}
	slices.SortFunc(tags, func(a, b string) int {
		if newer, _ := updaterapi.Newer(a, b); newer {
			return -1
		}
		if newer, _ := updaterapi.Newer(b, a); newer {
			return 1
		}
		return 0
	})
	return tags
}

// ancestor reports whether tag is an ancestor of ref in this repository
// (a tag it doesn't have isn't).
var ancestor = func(tag, ref string) bool {
	return exec.Command("git", "merge-base", "--is-ancestor", "refs/tags/"+tag, ref).Run() == nil
}

// newest is the newest release, reachable from ref when ref isn't empty.
func newest(rels []release, ref string) (string, error) {
	for _, tag := range stable(rels) {
		if ref == "" || ancestor(tag, ref) {
			return tag, nil
		}
	}
	if ref != "" {
		return "", fmt.Errorf("no release is reachable from %s", ref)
	}
	return "", errors.New("no release published")
}

// checkPrerelease refuses a pre-release no newer than the newest release.
func checkPrerelease(rels []release, version string) error {
	if !updaterapi.IsRelease(version) {
		return fmt.Errorf("%s isn't a release version (vYYYY.MM.DD or vYYYY.MM.DD-N)", version)
	}
	latest, err := newest(rels, "")
	if err != nil {
		return nil // nothing to be newer than
	}
	if newer, _ := updaterapi.Newer(version, latest); !newer {
		return fmt.Errorf("%s isn't newer than the newest release, %s: a pre-release must be, to become a release later", version, latest)
	}
	return nil
}

// infoDoc is info's answer.
type infoDoc struct {
	ID            int64                `json:"id"`
	Version       string               `json:"version"`
	Name          string               `json:"name"`
	URL           string               `json:"url"`
	Draft         bool                 `json:"draft"`
	Prerelease    bool                 `json:"prerelease"`
	NewestRelease string               `json:"newest_release"`
	Newer         bool                 `json:"newer"` // than NewestRelease
	Assets        map[string]infoAsset `json:"assets"`
}

type infoAsset struct {
	URL    string `json:"url"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256,omitempty"`
}

func info(rels []release, version string) (*infoDoc, error) {
	i := slices.IndexFunc(rels, func(r release) bool { return r.Tag == version })
	if i < 0 {
		return nil, fmt.Errorf("no release %s", version)
	}
	r := rels[i]
	doc := &infoDoc{ID: r.ID, Version: r.Tag, Name: r.Name, URL: r.HTMLURL, Draft: r.Draft, Prerelease: r.Prerelease, Assets: map[string]infoAsset{}}
	if latest, err := newest(rels, ""); err == nil {
		doc.NewestRelease = latest
		doc.Newer, _ = updaterapi.Newer(version, latest)
	}
	for _, a := range r.Assets {
		sum, _ := strings.CutPrefix(a.Digest, "sha256:")
		doc.Assets[a.Name] = infoAsset{URL: a.URL, Size: a.Size, SHA256: sum}
	}
	return doc, nil
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "releases:", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	if args[0] == "publish" {
		return runPublish(args[1:], out)
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	ref := fs.String("reachable-from", "", "only a release whose tag is an ancestor of this ref")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	ctx := context.Background()
	rels, err := newClient().releases(ctx)
	if err != nil {
		return err
	}
	switch {
	case args[0] == "newest" && fs.NArg() == 0:
		tag, err := newest(rels, *ref)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, tag)
		return err
	case args[0] == "check-prerelease" && fs.NArg() == 1:
		return checkPrerelease(rels, fs.Arg(0))
	case args[0] == "info" && fs.NArg() == 1:
		doc, err := info(rels, fs.Arg(0))
		if err != nil {
			return err
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(doc)
	}
	return errors.New(usage)
}

const usage = "usage: releases newest [-reachable-from REF] | check-prerelease VERSION | info VERSION | publish -commit SHA -name NAME -notes FILE [-prerelease] VERSION GLOB [GLOB...]"
