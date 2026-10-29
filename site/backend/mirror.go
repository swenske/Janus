package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/swenske/Janus/internal/schematic"
)

// The image factory's mirror (docs/image-factory.md "When the factory
// can't be reached: its mirror"): janus-site run with -mirror-of keeps a
// read-only copy of the update bundles this site built for its two
// newest releases, and answers what a Controller asks when the site
// can't be reached - GET /api/v1/updates/{id}, GET /api/v1/images/...,
// the bundles' files. It builds nothing, knows nothing of GitHub: what
// the site's index says, it copies; what isn't there, it hasn't got.
//
// The site's side: GET /api/v1/mirror/index (what to copy) and POST
// /api/v1/mirror/stats (the mirror's download counts), both behind
// -mirror-token-file's bearer token - and its downloads with that token
// aren't counted.

// mirrorIndex is GET /api/v1/mirror/index.
type mirrorIndex struct {
	// Releases are the two newest releases (pre-releases never), newest
	// first.
	Releases []mirrorRelease `json:"releases"`
	Builds   []mirrorBuild   `json:"builds"`
}

type mirrorRelease struct {
	Version string `json:"version"`
	URL     string `json:"url"`
}

// mirrorBuild is a schematic's update bundle for one release.
type mirrorBuild struct {
	// Schematic is the schematic, canonical JSON (its SHA-256 is its ID).
	Schematic string `json:"schematic"`
	// Manifest lists the bundle's files only.
	Manifest manifest `json:"manifest"`
}

// mirrorReleases is how many releases the mirror holds.
const mirrorReleases = 2

func bearer(r *http.Request, token string) bool {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return token != "" && ok && subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

// bundleOnly is m with its bundle's files alone - false when it lacks
// one a node needs.
func bundleOnly(m *manifest) (manifest, bool) {
	out := *m
	out.Files = nil
	for _, f := range m.Files {
		if slices.Contains(bundleFiles, f.Name) {
			out.Files = append(out.Files, f)
		}
	}
	for _, need := range []string{"rootfs.squashfs", "rootfs.verity", "uki-a.efi", "uki-b.efi"} {
		if _, ok := out.file(need); !ok {
			return out, false
		}
	}
	return out, true
}

func (a *app) handleMirrorIndex(w http.ResponseWriter, r *http.Request) {
	if !bearer(r, a.mirrorToken) {
		writeError(w, http.StatusUnauthorized, "the mirror's token")
		return
	}
	rels, err := a.gh.Releases(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	idx := mirrorIndex{Releases: []mirrorRelease{}, Builds: []mirrorBuild{}}
	for _, rel := range rels { // newest first
		if !rel.Prerelease && rel.Schematics && len(idx.Releases) < mirrorReleases {
			idx.Releases = append(idx.Releases, mirrorRelease{Version: rel.Version, URL: rel.URL})
		}
	}
	ids, err := a.store.SchematicIDs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, rel := range idx.Releases {
		for _, id := range ids {
			m, err := a.store.Manifest(id, rel.Version, "amd64")
			if err != nil || m == nil {
				continue
			}
			bm, ok := bundleOnly(m)
			sc, err := a.store.Schematic(id)
			if !ok || err != nil {
				continue
			}
			idx.Builds = append(idx.Builds, mirrorBuild{Schematic: string(sc.Canonical()), Manifest: bm})
		}
	}
	writeJSON(w, http.StatusOK, idx)
}

// handleMirrorStats takes the mirror's download counts (statsFile.Days):
// they replace what it sent before.
func (a *app) handleMirrorStats(w http.ResponseWriter, r *http.Request) {
	if !bearer(r, a.mirrorToken) {
		writeError(w, http.StatusUnauthorized, "the mirror's token")
		return
	}
	var days map[string]map[string]int64
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&days); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	for day, keys := range days {
		if _, err := time.Parse(dayLayout, day); err != nil {
			writeError(w, http.StatusBadRequest, "not a day: "+day)
			return
		}
		for k, n := range keys {
			if parts := strings.SplitN(k, " ", 3); len(parts) != 3 || !schematic.ValidID(parts[0]) || !validVersion(parts[1]) || n < 0 {
				writeError(w, http.StatusBadRequest, "not a count: "+k)
				return
			}
		}
	}
	a.stats.setMirror(days)
	w.WriteHeader(http.StatusNoContent)
}

// mirror is janus-site run with -mirror-of.
type mirror struct {
	store     *store
	stats     *downloadStats
	site      string // the site it copies, e.g. http://172.16.1.90:8080
	token     string
	publicURL string // https://janus-mirror.sw-servers.net
	client    *http.Client

	mu    sync.Mutex
	index *mirrorIndex // the last index synced
}

func (m *mirror) indexPath() string { return filepath.Join(m.store.dir, "mirror-index.json") }

func (m *mirror) load() {
	data, err := os.ReadFile(m.indexPath())
	if err != nil {
		return
	}
	var idx mirrorIndex
	if json.Unmarshal(data, &idx) == nil {
		m.index = &idx
	}
}

func (m *mirror) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/updates/{id}", m.handleUpdates)
	mux.HandleFunc("GET /api/v1/images/{id}/{version}/{arch}", m.handleImageStatus)
	mux.HandleFunc("GET /image/{id}/{version}/{arch}/{file}", m.handleDownload)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
}

func (m *mirror) releases() []mirrorRelease {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.index == nil {
		return nil
	}
	return m.index.Releases
}

// handleUpdates answers as the site does: the newest release the mirror
// holds a build of the schematic for.
func (m *mirror) handleUpdates(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if arch := r.URL.Query().Get("arch"); arch != "" && arch != "amd64" {
		writeError(w, http.StatusBadRequest, "updates are published for amd64 only")
		return
	}
	if !schematic.ValidID(id) {
		writeError(w, http.StatusNotFound, "not a schematic ID")
		return
	}
	for _, rel := range m.releases() { // newest first
		man, err := m.store.Manifest(id, rel.Version, "amd64")
		if err != nil || man == nil {
			continue
		}
		up := update{Schematic: id, Version: rel.Version, ReleaseURL: rel.URL, State: "ready",
			BundleURL: fmt.Sprintf("%s/image/%s/%s/amd64", m.publicURL, id, rel.Version), UpToDate: r.URL.Query().Get("from") == rel.Version}
		if f, ok := man.file("rootfs.squashfs"); ok {
			up.SHA256 = f.SHA256
		}
		writeJSON(w, http.StatusOK, up)
		return
	}
	writeError(w, http.StatusNotFound, "this mirror has no update for this schematic: it holds the bundles the image factory had built for its newest releases")
}

func (m *mirror) handleImageStatus(w http.ResponseWriter, r *http.Request) {
	id, version, arch := r.PathValue("id"), r.PathValue("version"), r.PathValue("arch")
	if !schematic.ValidID(id) || !validVersion(version) || !validArch(arch) {
		writeError(w, http.StatusBadRequest, "invalid schematic, version or architecture")
		return
	}
	st := imageStatus{Schematic: id, Version: version, Arch: arch}
	man, err := m.store.Manifest(id, version, arch)
	if err != nil || man == nil {
		st.State, st.Message = "unavailable", "this mirror has no build of this schematic for "+version+": it holds the update bundles the image factory had built for its newest releases"
		writeJSON(w, http.StatusOK, st)
		return
	}
	st.State = "ready"
	for _, f := range man.Files {
		st.Files = append(st.Files, statusFile{Name: f.Name, URL: fmt.Sprintf("%s/image/%s/%s/%s/%s", m.publicURL, id, version, arch, f.Name), Size: f.Size, SHA256: f.SHA256})
	}
	writeJSON(w, http.StatusOK, st)
}

func (m *mirror) handleDownload(w http.ResponseWriter, r *http.Request) {
	id, version, arch, file := r.PathValue("id"), r.PathValue("version"), r.PathValue("arch"), r.PathValue("file")
	if !schematic.ValidID(id) || !validVersion(version) || arch != "amd64" || !slices.Contains(bundleFiles, file) {
		http.NotFound(w, r)
		return
	}
	man, err := m.store.Manifest(id, version, arch)
	if err != nil || man == nil {
		http.NotFound(w, r)
		return
	}
	if _, ok := man.file(file); !ok {
		http.NotFound(w, r)
		return
	}
	if countable(r) {
		m.stats.count(id, version, downloadKind(arch, file))
	}
	http.ServeFile(w, r, filepath.Join(m.store.ImageDir(id, version, arch), file))
}

func (m *mirror) get(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.site+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+m.token)
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("%s: %s", path, resp.Status)
	}
	return resp, nil
}

// sync copies what the site's index lists and drops the rest, then
// sends the site the mirror's download counts.
func (m *mirror) sync(ctx context.Context) error {
	resp, err := m.get(ctx, "/api/v1/mirror/index")
	if err != nil {
		return err
	}
	var idx mirrorIndex
	err = json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&idx)
	resp.Body.Close()
	if err != nil {
		return fmt.Errorf("index: %w", err)
	}
	kept := map[string]bool{} // "<id> <version>"
	var errs []error
	for _, b := range idx.Builds {
		key, err := m.fetch(ctx, b)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		kept[key] = true
	}
	// What the index no longer lists goes - and what failed to copy
	// isn't served half-copied (fetch removes its manifest first).
	if err := m.prune(kept); err != nil {
		errs = append(errs, err)
	}
	data, err := json.Marshal(idx)
	if err == nil {
		tmp := m.indexPath() + ".tmp"
		if err = os.WriteFile(tmp, data, 0o644); err == nil {
			err = os.Rename(tmp, m.indexPath())
		}
	}
	if err != nil {
		errs = append(errs, err)
	}
	m.mu.Lock()
	m.index = &idx
	m.mu.Unlock()
	if err := m.pushStats(ctx); err != nil {
		errs = append(errs, fmt.Errorf("download counts: %w", err))
	}
	return errors.Join(errs...)
}

// fetch copies one build: the bundle's files, each checked against the
// manifest's size and SHA-256, the schematic, then the manifest - last,
// the build's completion marker here as on the site.
func (m *mirror) fetch(ctx context.Context, b mirrorBuild) (string, error) {
	sc, err := schematic.Parse([]byte(b.Schematic))
	if err != nil {
		return "", fmt.Errorf("index: %w", err)
	}
	id, man := sc.ID(), b.Manifest
	if man.Schematic != id || !validVersion(man.Version) || man.Arch != "amd64" {
		return "", fmt.Errorf("index: a build of %s that isn't one (%s %s %s)", short(id), short(man.Schematic), man.Version, man.Arch)
	}
	key := id + " " + man.Version
	if have, _ := m.store.Manifest(id, man.Version, "amd64"); have != nil && slices.Equal(have.Files, man.Files) {
		return key, nil
	}
	dir := m.store.ImageDir(id, man.Version, "amd64")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	_ = os.Remove(filepath.Join(dir, "manifest.json"))
	for _, f := range man.Files {
		if !slices.Contains(bundleFiles, f.Name) {
			return "", fmt.Errorf("index: %s isn't a bundle file", f.Name)
		}
		if err := m.fetchFile(ctx, id, man.Version, dir, f); err != nil {
			return "", fmt.Errorf("%s %s %s: %w", short(id), man.Version, f.Name, err)
		}
	}
	if _, err := m.store.PutSchematic(sc); err != nil {
		return "", err
	}
	data, err := json.Marshal(man)
	if err != nil {
		return "", err
	}
	tmp := filepath.Join(dir, "manifest.json.tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, filepath.Join(dir, "manifest.json")); err != nil {
		return "", err
	}
	log.Printf("mirror: copied %s %s", short(id), man.Version)
	return key, nil
}

func (m *mirror) fetchFile(ctx context.Context, id, version, dir string, f manifestFile) error {
	path := filepath.Join(dir, f.Name)
	if fileMatches(path, f) {
		return nil
	}
	resp, err := m.get(ctx, fmt.Sprintf("/image/%s/%s/amd64/%s", id, version, f.Name))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	tmp := path + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(resp.Body, f.Size+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && (n != f.Size || hex.EncodeToString(h.Sum(nil)) != f.SHA256) {
		err = fmt.Errorf("got %d bytes, sha256 %s - the manifest says %d, %s", n, hex.EncodeToString(h.Sum(nil)), f.Size, f.SHA256)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// fileMatches reports whether path already is f (a previous sync's).
func fileMatches(path string, f manifestFile) bool {
	in, err := os.Open(path)
	if err != nil {
		return false
	}
	defer in.Close()
	h := sha256.New()
	n, err := io.Copy(h, in)
	return err == nil && n == f.Size && hex.EncodeToString(h.Sum(nil)) == f.SHA256
}

// prune drops every build kept doesn't name, and the schematics left
// with none.
func (m *mirror) prune(kept map[string]bool) error {
	base := filepath.Join(m.store.dir, "images")
	ids, err := os.ReadDir(base)
	if err != nil {
		return err
	}
	used := map[string]bool{}
	for _, id := range ids {
		versions, err := os.ReadDir(filepath.Join(base, id.Name()))
		if err != nil {
			continue
		}
		left := 0
		for _, v := range versions {
			if kept[id.Name()+" "+v.Name()] {
				left++
				continue
			}
			if err := os.RemoveAll(filepath.Join(base, id.Name(), v.Name())); err != nil {
				return err
			}
			log.Printf("mirror: dropped %s %s", short(id.Name()), v.Name())
		}
		if left == 0 {
			_ = os.Remove(filepath.Join(base, id.Name()))
		} else {
			used[id.Name()] = true
		}
	}
	all, err := m.store.SchematicIDs()
	if err != nil {
		return err
	}
	for _, id := range all {
		if !used[id] {
			_ = os.Remove(filepath.Join(m.store.dir, "schematics", id+".json"))
		}
	}
	return nil
}

func (m *mirror) pushStats(ctx context.Context) error {
	data, err := json.Marshal(m.stats.days())
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.site+"/api/v1/mirror/stats", strings.NewReader(string(data)))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+m.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return errors.New(resp.Status)
	}
	return nil
}

// run syncs now, then every 15 minutes until ctx ends.
func (m *mirror) run(ctx context.Context) {
	for {
		sctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		if err := m.sync(sctx); err != nil {
			log.Printf("mirror: %v", err)
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-time.After(15 * time.Minute):
		}
	}
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
