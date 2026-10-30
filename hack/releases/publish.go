package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// publish makes VERSION's GitHub release, and never changes one already
// published (#433).
//
// The release stays a draft until every asset is there: nobody sees a
// release half uploaded. A run that failed meanwhile leaves the draft
// behind; the next attempt - a rerun of the publishing job, which
// rebuilds everything - completes it with its own files, all of them: a
// rebuild isn't byte for byte the same, and two attempts' bundles mixed
// would pair a rootfs with UKIs pinning another one's root hash.
//
// A published release's assets are what people downloaded and what
// Controllers cached the SHA-256 of: a later attempt (a rerun after a
// later step failed) compares them with its own build and says what
// differs, uploads nothing and leaves the release as it is.

// publishOpts is what publish is given.
type publishOpts struct {
	Version    string
	Commit     string // the release's commit: the tag is created on it
	Name       string
	Notes      string // the notes' file
	Prerelease bool
	Patterns   []string // the assets, as globs
}

// localFile is an asset to publish.
type localFile struct {
	Path, Name, SHA256 string
	Size               int64
}

// uploadAttempts are the tries an asset's upload gets: GitHub answers
// one with an error page now and then (v2026.10.30's
// extension-consul-amd64.tar).
const uploadAttempts = 4

// uploadHTTP has no overall timeout: an asset may take minutes - each
// try has its own deadline.
var uploadHTTP = &http.Client{}

func runPublish(args []string, out io.Writer) error {
	var o publishOpts
	fs := flag.NewFlagSet("publish", flag.ContinueOnError)
	fs.StringVar(&o.Commit, "commit", "", "the commit the release is made from (its tag goes there)")
	fs.StringVar(&o.Name, "name", "", "the release's name")
	fs.StringVar(&o.Notes, "notes", "", "the file with the release's notes")
	fs.BoolVar(&o.Prerelease, "prerelease", false, "a pre-release: never the latest")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 2 || o.Commit == "" || o.Name == "" || o.Notes == "" {
		return errors.New(usage)
	}
	o.Version, o.Patterns = fs.Arg(0), fs.Args()[1:]
	c := newClient()
	if c.token == "" {
		return errors.New("publish needs GITHUB_TOKEN")
	}
	return publish(context.Background(), c, o, out)
}

func publish(ctx context.Context, c *client, o publishOpts, out io.Writer) error {
	files, err := localFiles(o.Patterns)
	if err != nil {
		return err
	}
	rels, err := c.releases(ctx)
	if err != nil {
		return err
	}
	var draft *release
	for i, r := range rels {
		switch {
		case r.Tag != o.Version:
		case !r.Draft:
			return checkPublished(ctx, c, r, o, files, out)
		case draft != nil:
			return fmt.Errorf("%s has several drafts: delete them on GitHub, then run this again", o.Version)
		default:
			draft = &rels[i]
		}
	}
	// A tag left without its release (deleted) would carry the new one:
	// publishing a draft creates its tag only when there's none.
	tagged, err := c.tagCommit(ctx, o.Version)
	if err != nil {
		return err
	}
	if tagged != "" && tagged != o.Commit {
		return fmt.Errorf("tag %s exists on commit %s, not %s: delete it, or publish another version", o.Version, tagged, o.Commit)
	}
	notes, err := os.ReadFile(o.Notes)
	if err != nil {
		return err
	}
	spec := map[string]any{
		"tag_name":         o.Version,
		"target_commitish": o.Commit,
		"name":             o.Name,
		"body":             string(notes),
		"draft":            true,
		"prerelease":       o.Prerelease,
	}
	var r release
	if draft != nil {
		if draft.Commitish != o.Commit {
			return fmt.Errorf("the draft of %s is for %s, not %s: delete it on GitHub if it's stale", o.Version, draft.Commitish, o.Commit)
		}
		fmt.Fprintf(out, "%s: completing the draft an earlier attempt left (%d assets)\n", o.Version, len(draft.Assets))
		err = c.send(ctx, http.MethodPatch, fmt.Sprintf("/releases/%d", draft.ID), spec, &r)
	} else {
		fmt.Fprintf(out, "%s: creating the release as a draft\n", o.Version)
		err = c.send(ctx, http.MethodPost, "/releases", spec, &r)
	}
	if err != nil {
		return err
	}
	if err := syncAssets(ctx, c, r, files, out); err != nil {
		return err
	}
	if err := checkAssets(ctx, c, r.ID, files); err != nil {
		return err
	}
	latest := "true"
	if o.Prerelease {
		latest = "false"
	}
	if err := c.send(ctx, http.MethodPatch, fmt.Sprintf("/releases/%d", r.ID), map[string]any{"draft": false, "make_latest": latest}, &r); err != nil {
		return err
	}
	if r.Draft {
		return fmt.Errorf("%s is still a draft after publishing it", o.Version)
	}
	if tagged, err := c.tagCommit(ctx, o.Version); err != nil || tagged != o.Commit {
		return fmt.Errorf("%s published, but its tag names %q, not %s (%v)", o.Version, tagged, o.Commit, err)
	}
	fmt.Fprintf(out, "%s: published, %d assets - %s\n", o.Version, len(files), r.HTMLURL)
	return nil
}

// checkPublished compares a published release with this build, and
// changes nothing.
func checkPublished(ctx context.Context, c *client, r release, o publishOpts, files []localFile, out io.Writer) error {
	tagged, err := c.tagCommit(ctx, o.Version)
	if err != nil {
		return err
	}
	if tagged != o.Commit {
		return fmt.Errorf("%s is already published, from commit %q, not %s: a published release never changes - publish another version", o.Version, tagged, o.Commit)
	}
	have := byName(r.Assets)
	var missing []string
	for _, f := range files {
		a, ok := have[f.Name]
		switch {
		case !ok || a.State != "uploaded":
			missing = append(missing, f.Name)
		case a.Digest == "":
			fmt.Fprintf(out, "::warning::%s: GitHub gives no digest of the published one - not compared\n", f.Name)
		case a.Digest != "sha256:"+f.SHA256:
			fmt.Fprintf(out, "::warning::%s: the published one (%s) isn't what this attempt built (sha256:%s) - it stays as published\n", f.Name, a.Digest, f.SHA256)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s is published without %s: nothing is added to a published release, from another build than the rest (docs/contributing/releasing.md, \"When publishing fails\")", o.Version, strings.Join(missing, ", "))
	}
	fmt.Fprintf(out, "%s: already published from this commit, with every asset - left as it is\n", o.Version)
	return nil
}

// syncAssets makes the draft r's assets files: one already there with
// the same bytes stays, one with other bytes is replaced, one this
// attempt doesn't publish goes.
func syncAssets(ctx context.Context, c *client, r release, files []localFile, out io.Writer) error {
	have := byName(r.Assets)
	for _, f := range files {
		if a, ok := have[f.Name]; ok {
			if a.State == "uploaded" && a.Digest == "sha256:"+f.SHA256 {
				fmt.Fprintf(out, "%s: already there\n", f.Name)
				continue
			}
			fmt.Fprintf(out, "%s: replacing the earlier attempt's\n", f.Name)
			if err := c.deleteAsset(ctx, a.ID); err != nil {
				return err
			}
		}
		if err := c.upload(ctx, r, f, out); err != nil {
			return err
		}
	}
	want := map[string]bool{}
	for _, f := range files {
		want[f.Name] = true
	}
	for _, a := range r.Assets {
		if !want[a.Name] {
			fmt.Fprintf(out, "%s: removed - this attempt doesn't publish it\n", a.Name)
			if err := c.deleteAsset(ctx, a.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkAssets reads release id back: exactly files, each with its bytes.
func checkAssets(ctx context.Context, c *client, id int64, files []localFile) error {
	var r release
	if err := c.get(ctx, fmt.Sprintf("/releases/%d", id), &r); err != nil {
		return err
	}
	have := byName(r.Assets)
	for _, f := range files {
		a, ok := have[f.Name]
		switch {
		case !ok || a.State != "uploaded":
			return fmt.Errorf("%s: not on the release after its upload", f.Name)
		case a.Digest != "" && a.Digest != "sha256:"+f.SHA256, a.Digest == "" && a.Size != f.Size:
			return fmt.Errorf("%s: the release has other bytes (%s, %d bytes) than sha256:%s", f.Name, a.Digest, a.Size, f.SHA256)
		}
	}
	if len(have) != len(files) {
		return fmt.Errorf("the release has %d assets, not %d", len(have), len(files))
	}
	return nil
}

// upload adds f to release r, trying again after a failure.
func (c *client) upload(ctx context.Context, r release, f localFile, out io.Writer) error {
	base, _, _ := strings.Cut(r.UploadURL, "{")
	var err error
	for attempt := range uploadAttempts {
		if attempt > 0 {
			fmt.Fprintf(out, "%s: %v - trying again\n", f.Name, err)
			time.Sleep(time.Duration(attempt) * backoff)
			// A failed upload may leave its asset behind, unfinished:
			// GitHub refuses another of the same name.
			if err := c.dropAsset(ctx, r.ID, f.Name); err != nil {
				return err
			}
		}
		if err = c.uploadOnce(ctx, base, f); err == nil {
			fmt.Fprintf(out, "%s: uploaded, %d bytes\n", f.Name, f.Size)
			return nil
		}
	}
	return fmt.Errorf("%s: %w", f.Name, err)
}

func (c *client) uploadOnce(ctx context.Context, base string, f localFile) error {
	file, err := os.Open(f.Path)
	if err != nil {
		return err
	}
	defer file.Close()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"?name="+url.QueryEscape(f.Name), file)
	if err != nil {
		return err
	}
	req.ContentLength = f.Size
	c.headers(req)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := uploadHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("upload: %s%s", resp.Status, apiMessage(body))
	}
	var a asset
	if err := json.Unmarshal(body, &a); err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	if a.Digest != "" && a.Digest != "sha256:"+f.SHA256 {
		return fmt.Errorf("upload: GitHub received other bytes (%s)", a.Digest)
	}
	return nil
}

// dropAsset removes release id's asset named name, if there is one.
func (c *client) dropAsset(ctx context.Context, id int64, name string) error {
	var r release
	if err := c.get(ctx, fmt.Sprintf("/releases/%d", id), &r); err != nil {
		return err
	}
	if a, ok := byName(r.Assets)[name]; ok {
		return c.deleteAsset(ctx, a.ID)
	}
	return nil
}

func (c *client) deleteAsset(ctx context.Context, id int64) error {
	return c.send(ctx, http.MethodDelete, fmt.Sprintf("/releases/assets/%d", id), nil, nil)
}

// tagCommit is the commit tag names, "" when there's no such tag.
func (c *client) tagCommit(ctx context.Context, tag string) (string, error) {
	var ref struct {
		Object struct {
			SHA  string `json:"sha"`
			Type string `json:"type"`
		} `json:"object"`
	}
	if err := c.get(ctx, "/git/ref/tags/"+url.PathEscape(tag), &ref); err != nil {
		if errors.Is(err, errNotFound) {
			return "", nil
		}
		return "", err
	}
	if ref.Object.Type == "tag" { // an annotated tag: the commit it names
		var t struct {
			Object struct {
				SHA string `json:"sha"`
			} `json:"object"`
		}
		if err := c.get(ctx, "/git/tags/"+ref.Object.SHA, &t); err != nil {
			return "", err
		}
		return t.Object.SHA, nil
	}
	return ref.Object.SHA, nil
}

func byName(assets []asset) map[string]asset {
	m := make(map[string]asset, len(assets))
	for _, a := range assets {
		m[a.Name] = a
	}
	return m
}

// localFiles are the files patterns name, each pattern at least one, in
// the patterns' order.
func localFiles(patterns []string) ([]localFile, error) {
	var files []localFile
	seen := map[string]string{} // asset name -> path
	for _, p := range patterns {
		paths, err := filepath.Glob(p)
		if err != nil {
			return nil, err
		}
		if len(paths) == 0 {
			return nil, fmt.Errorf("%s: no such file", p)
		}
		slices.Sort(paths)
		for _, path := range paths {
			name := filepath.Base(path)
			if prev, ok := seen[name]; ok {
				if prev == path {
					continue
				}
				return nil, fmt.Errorf("%s and %s would both be the asset %s", prev, path, name)
			}
			seen[name] = path
			f, err := hashFile(path)
			if err != nil {
				return nil, err
			}
			files = append(files, f)
		}
	}
	return files, nil
}

func hashFile(path string) (localFile, error) {
	file, err := os.Open(path)
	if err != nil {
		return localFile{}, err
	}
	defer file.Close()
	h := sha256.New()
	n, err := io.Copy(h, file)
	if err != nil {
		return localFile{}, fmt.Errorf("%s: %w", path, err)
	}
	return localFile{Path: path, Name: filepath.Base(path), SHA256: hex.EncodeToString(h.Sum(nil)), Size: n}, nil
}
