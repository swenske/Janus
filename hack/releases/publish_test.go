package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// hub is a GitHub keeping releases, their assets and tags: what publish
// uses of its API.
type hub struct {
	t       *testing.T
	mu      sync.Mutex
	srv     *httptest.Server
	rels    []*release
	data    map[int64][]byte  // asset id -> bytes
	tags    map[string]string // tag -> commit
	latest  map[int64]string  // release id -> make_latest last asked
	nextID  int64
	fail    map[string]int // asset name -> uploads to answer with an error page
	changes []string       // every request that changes something
}

func newHub(t *testing.T) *hub {
	h := &hub{t: t, data: map[int64][]byte{}, tags: map[string]string{}, latest: map[int64]string{}, fail: map[string]int{}, nextID: 100}
	h.srv = httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.srv.Close)
	t.Setenv("GITHUB_API_URL", h.srv.URL)
	t.Setenv("GITHUB_REPOSITORY", "o/r")
	t.Setenv("GITHUB_TOKEN", "tok")
	prev := backoff
	backoff = 0
	t.Cleanup(func() { backoff = prev })
	return h
}

func (h *hub) id() int64 {
	h.nextID++
	return h.nextID
}

// add puts a release in, its assets with these bytes; a published one
// gets its tag.
func (h *hub) add(r release, assets map[string]string) *release {
	r.ID = h.id()
	for _, name := range sortedKeys(assets) {
		h.addAsset(&r, name, []byte(assets[name]), "uploaded")
	}
	if !r.Draft {
		h.tags[r.Tag] = r.Commitish
	}
	h.rels = append(h.rels, &r)
	return &r
}

func (h *hub) addAsset(r *release, name string, b []byte, state string) {
	sum := sha256.Sum256(b)
	a := asset{ID: h.id(), Name: name, State: state, Size: int64(len(b)), Digest: "sha256:" + hex.EncodeToString(sum[:])}
	h.data[a.ID] = b
	r.Assets = append(r.Assets, a)
}

func (h *hub) find(id string) *release {
	for _, r := range h.rels {
		if fmt.Sprint(r.ID) == id {
			return r
		}
	}
	return nil
}

func (h *hub) reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (h *hub) withURLs(r *release) release {
	c := *r
	c.UploadURL = fmt.Sprintf("%s/uploads/%d/assets{?name,label}", h.srv.URL, r.ID)
	c.HTMLURL = "https://github.test/o/r/releases/tag/" + r.Tag
	c.Assets = slices.Clone(r.Assets)
	return c
}

func (h *hub) serve(w http.ResponseWriter, req *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if req.Header.Get("Authorization") != "Bearer tok" {
		http.Error(w, "no token", http.StatusUnauthorized)
		return
	}
	if req.Method != http.MethodGet {
		h.changes = append(h.changes, req.Method+" "+req.URL.Path+"?"+req.URL.RawQuery)
	}
	parts := strings.Split(strings.TrimPrefix(req.URL.Path, "/"), "/")
	switch {
	case req.Method == http.MethodGet && req.URL.Path == "/repos/o/r/releases":
		var out []release
		if req.URL.Query().Get("page") == "1" {
			for i := len(h.rels) - 1; i >= 0; i-- {
				out = append(out, h.withURLs(h.rels[i]))
			}
		}
		h.reply(w, http.StatusOK, out)
	case req.Method == http.MethodPost && req.URL.Path == "/repos/o/r/releases":
		var spec map[string]any
		_ = json.NewDecoder(req.Body).Decode(&spec)
		r := &release{ID: h.id(), Tag: spec["tag_name"].(string), Commitish: spec["target_commitish"].(string), Name: spec["name"].(string), Draft: spec["draft"].(bool), Prerelease: spec["prerelease"].(bool)}
		if !r.Draft {
			h.t.Errorf("a release created published")
		}
		h.rels = append(h.rels, r)
		h.reply(w, http.StatusCreated, h.withURLs(r))
	case len(parts) == 5 && parts[3] == "releases" && (req.Method == http.MethodGet || req.Method == http.MethodPatch):
		r := h.find(parts[4])
		if r == nil {
			h.reply(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
			return
		}
		if req.Method == http.MethodPatch {
			var spec map[string]any
			_ = json.NewDecoder(req.Body).Decode(&spec)
			if v, ok := spec["name"].(string); ok {
				r.Name = v
			}
			if v, ok := spec["prerelease"].(bool); ok {
				r.Prerelease = v
			}
			if v, ok := spec["make_latest"].(string); ok {
				h.latest[r.ID] = v
			}
			if v, ok := spec["draft"].(bool); ok {
				if r.Draft && !v {
					if _, ok := h.tags[r.Tag]; !ok {
						h.tags[r.Tag] = r.Commitish
					}
				}
				r.Draft = v
			}
		}
		h.reply(w, http.StatusOK, h.withURLs(r))
	case len(parts) == 6 && parts[3] == "releases" && parts[4] == "assets" && req.Method == http.MethodDelete:
		for _, r := range h.rels {
			r.Assets = slices.DeleteFunc(r.Assets, func(a asset) bool { return fmt.Sprint(a.ID) == parts[5] })
		}
		w.WriteHeader(http.StatusNoContent)
	case len(parts) == 7 && parts[3] == "git" && parts[4] == "ref" && parts[5] == "tags":
		tag := parts[len(parts)-1]
		if c, ok := h.tags[tag]; ok {
			h.reply(w, http.StatusOK, map[string]any{"object": map[string]string{"sha": c, "type": "commit"}})
			return
		}
		h.reply(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
	case len(parts) == 3 && parts[0] == "uploads" && req.Method == http.MethodPost:
		r := h.find(parts[1])
		name := req.URL.Query().Get("name")
		b, _ := io.ReadAll(req.Body)
		if r == nil || req.Header.Get("Content-Type") != "application/octet-stream" || int64(len(b)) != req.ContentLength {
			h.reply(w, http.StatusBadRequest, map[string]string{"message": "bad upload"})
			return
		}
		if slices.ContainsFunc(r.Assets, func(a asset) bool { return a.Name == name }) {
			h.reply(w, http.StatusUnprocessableEntity, map[string]string{"message": "Validation Failed: already_exists"})
			return
		}
		if h.fail[name] > 0 { // half of it received, then an error page
			h.fail[name]--
			h.addAsset(r, name, b[:len(b)/2], "starter")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, "<html><body><h1>Unicorn!</h1></body></html>")
			return
		}
		h.addAsset(r, name, b, "uploaded")
		h.reply(w, http.StatusCreated, r.Assets[len(r.Assets)-1])
	default:
		h.t.Errorf("unexpected %s %s", req.Method, req.URL)
		http.Error(w, "unexpected", http.StatusNotFound)
	}
}

// assets are release r's assets' bytes by name, all uploaded.
func (h *hub) assets(r *release) map[string]string {
	h.t.Helper()
	m := map[string]string{}
	for _, a := range r.Assets {
		if a.State != "uploaded" {
			h.t.Errorf("%s: %s", a.Name, a.State)
		}
		m[a.Name] = string(h.data[a.ID])
	}
	return m
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// build writes an attempt's files: the bundle and an extension.
func build(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("the notes"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

const commit = "0d68fd7b2985852a7f63bbc8cb2a0b250172b995"

func publishArgs(dir string, prerelease bool) []string {
	return []string{"publish", "-commit", commit, "-name", "Janus v2026.10.30 (Alpha)", "-notes", filepath.Join(dir, "notes.md"),
		fmt.Sprintf("-prerelease=%t", prerelease), "v2026.10.30", filepath.Join(dir, "rootfs.*"), filepath.Join(dir, "extension-*.tar")}
}

var attempt1 = map[string]string{"rootfs.squashfs": "rootfs 1", "rootfs.verity": "verity 1", "extension-consul-amd64.tar": "consul", "extension-nftables-amd64.tar": "nft 1"}

func TestPublishNew(t *testing.T) {
	for _, pre := range []bool{true, false} {
		t.Run(fmt.Sprint("prerelease=", pre), func(t *testing.T) {
			h := newHub(t)
			h.add(release{Tag: "v2026.10.29-6", Commitish: "older"}, map[string]string{"rootfs.squashfs": "old"})
			// GitHub fails an upload now and then: tried again, the
			// unfinished asset removed first.
			h.fail["extension-consul-amd64.tar"] = 2
			out, err := runOut(t, publishArgs(build(t, attempt1), pre)...)
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			r := h.rels[1]
			if r.Draft || r.Prerelease != pre || r.Tag != "v2026.10.30" || r.Name != "Janus v2026.10.30 (Alpha)" || h.tags["v2026.10.30"] != commit {
				t.Errorf("release = %+v, tag on %q", r, h.tags["v2026.10.30"])
			}
			if got := h.assets(r); !maps.Equal(got, attempt1) {
				t.Errorf("assets = %v", got)
			}
			if want := map[bool]string{true: "false", false: "true"}[pre]; h.latest[r.ID] != want {
				t.Errorf("make_latest = %q, want %q", h.latest[r.ID], want)
			}
			if !strings.Contains(out, "extension-consul-amd64.tar: upload: 502 Bad Gateway: <html><body><h1>Unicorn!</h1></body></html> - trying again") {
				t.Errorf("output:\n%s", out)
			}
			// The release appears only whole: its assets were uploaded to
			// the draft, published last.
			last := h.changes[len(h.changes)-1]
			if !strings.HasPrefix(last, "PATCH /repos/o/r/releases/") {
				t.Errorf("last change %q, all:\n%s", last, strings.Join(h.changes, "\n"))
			}
		})
	}
}

// A rerun after an attempt failed while uploading: the draft it left is
// completed with this attempt's build, in whole.
func TestPublishCompletesTheDraft(t *testing.T) {
	h := newHub(t)
	d := h.add(release{Tag: "v2026.10.30", Commitish: commit, Draft: true, Prerelease: true}, map[string]string{
		"rootfs.squashfs": "rootfs 1", "rootfs.verity": "verity 1", "extension-nftables-amd64.tar": "nft 1", "stale.txt": "x",
	})
	h.addAsset(d, "extension-consul-amd64.tar", []byte("con"), "starter")
	attempt2 := map[string]string{"rootfs.squashfs": "rootfs 2", "rootfs.verity": "verity 2", "extension-consul-amd64.tar": "consul", "extension-nftables-amd64.tar": "nft 1"}
	out, err := runOut(t, publishArgs(build(t, attempt2), true)...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if d.Draft || len(h.rels) != 1 || h.tags["v2026.10.30"] != commit {
		t.Errorf("release = %+v (%d releases)", d, len(h.rels))
	}
	if got := h.assets(d); !maps.Equal(got, attempt2) {
		t.Errorf("assets = %v", got)
	}
	// What was already there with the same bytes isn't sent again.
	for _, c := range h.changes {
		if strings.Contains(c, "name=extension-nftables-amd64.tar") {
			t.Errorf("uploaded again: %s", c)
		}
	}
	for _, want := range []string{"completing the draft an earlier attempt left (5 assets)", "extension-nftables-amd64.tar: already there", "rootfs.squashfs: replacing the earlier attempt's", "stale.txt: removed"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
}

// A rerun after a later step failed: the published release is compared,
// never changed.
func TestPublishLeavesAPublishedRelease(t *testing.T) {
	for _, tc := range []struct {
		name      string
		published map[string]string
		fails     string
		warns     []string
	}{
		{name: "same bytes", published: attempt1},
		{name: "other bytes", published: map[string]string{"rootfs.squashfs": "rootfs 0", "rootfs.verity": "verity 1", "extension-consul-amd64.tar": "consul", "extension-nftables-amd64.tar": "nft 0"},
			warns: []string{"::warning::rootfs.squashfs: the published one (sha256:", "::warning::extension-nftables-amd64.tar:"}},
		{name: "an asset missing", published: map[string]string{"rootfs.squashfs": "rootfs 1", "rootfs.verity": "verity 1", "extension-nftables-amd64.tar": "nft 1"},
			fails: "v2026.10.30 is published without extension-consul-amd64.tar"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHub(t)
			r := h.add(release{Tag: "v2026.10.30", Commitish: commit, Prerelease: true}, tc.published)
			out, err := runOut(t, publishArgs(build(t, attempt1), true)...)
			if tc.fails == "" && err != nil || tc.fails != "" && (err == nil || !strings.Contains(err.Error(), tc.fails)) {
				t.Errorf("err = %v\n%s", err, out)
			}
			if len(h.changes) != 0 || !maps.Equal(h.assets(r), tc.published) {
				t.Errorf("changed: %v", h.changes)
			}
			for _, w := range tc.warns {
				if !strings.Contains(out, w) {
					t.Errorf("no %q in:\n%s", w, out)
				}
			}
		})
	}
}

func TestPublishRefuses(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(h *hub)
		args  func(dir string) []string
		fails string
	}{
		{name: "published from another commit", setup: func(h *hub) {
			h.add(release{Tag: "v2026.10.30", Commitish: "other"}, attempt1)
		}, fails: `already published, from commit "other"`},
		{name: "a draft for another commit", setup: func(h *hub) {
			h.add(release{Tag: "v2026.10.30", Commitish: "other", Draft: true}, nil)
		}, fails: "the draft of v2026.10.30 is for other"},
		{name: "two drafts", setup: func(h *hub) {
			h.add(release{Tag: "v2026.10.30", Commitish: commit, Draft: true}, nil)
			h.add(release{Tag: "v2026.10.30", Commitish: commit, Draft: true}, nil)
		}, fails: "several drafts"},
		{name: "a tag left on another commit", setup: func(h *hub) {
			h.tags["v2026.10.30"] = "other"
		}, fails: "tag v2026.10.30 exists on commit other"},
		{name: "a glob matching nothing", args: func(dir string) []string {
			return append(publishArgs(dir, true), filepath.Join(dir, "kernel-*"))
		}, fails: "kernel-*: no such file"},
		{name: "two files for one asset", args: func(dir string) []string {
			sub := filepath.Join(dir, "sub")
			_ = os.Mkdir(sub, 0o755)
			_ = os.WriteFile(filepath.Join(sub, "rootfs.verity"), []byte("v"), 0o644)
			return append(publishArgs(dir, true), filepath.Join(sub, "rootfs.verity"))
		}, fails: "would both be the asset rootfs.verity"},
		{name: "no commit", args: func(dir string) []string {
			return []string{"publish", "-name", "n", "-notes", "x", "v2026.10.30", "x"}
		}, fails: "usage"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHub(t)
			if tc.setup != nil {
				tc.setup(h)
			}
			dir := build(t, attempt1)
			args := publishArgs(dir, true)
			if tc.args != nil {
				args = tc.args(dir)
			}
			out, err := runOut(t, args...)
			if err == nil || !strings.Contains(err.Error(), tc.fails) {
				t.Errorf("err = %v, want %q\n%s", err, tc.fails, out)
			}
			if len(h.changes) != 0 {
				t.Errorf("changed: %v", h.changes)
			}
		})
	}
}
