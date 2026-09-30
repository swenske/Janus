package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/swenske/Janus/internal/schematic"
)

// app is the site: the landing page and builder (a SPA), and the image
// factory API behind them.
type app struct {
	store     *store
	gh        *gitHub
	publicURL string // https://janus.sw-servers.net
	repo      string

	builds *buildLimiter
}

// buildLimiter bounds how many builds visitors can start: every build
// takes the self-hosted runner for several minutes.
type buildLimiter struct {
	mu        sync.Mutex
	requested map[string]time.Time   // build key -> when it was requested
	byIP      map[string][]time.Time // recent requests per client
	all       []time.Time
	perIP     int
	global    int
	window    time.Duration
	retryWait time.Duration // a requested build isn't requested again before this
}

func newBuildLimiter() *buildLimiter {
	return &buildLimiter{requested: map[string]time.Time{}, byIP: map[string][]time.Time{},
		perIP: 5, global: 30, window: time.Hour, retryWait: 90 * time.Minute}
}

func recent(ts []time.Time, window time.Duration) []time.Time {
	cut := time.Now().Add(-window)
	return slices.DeleteFunc(ts, func(t time.Time) bool { return t.Before(cut) })
}

// allow records a build request: start is true when a build has to be
// started, false when one already was (and isn't being retried).
func (b *buildLimiter) allow(key, ip string, retry bool) (start bool, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if t, ok := b.requested[key]; ok && time.Since(t) < b.retryWait && !retry {
		return false, nil
	}
	b.all = recent(b.all, b.window)
	b.byIP[ip] = recent(b.byIP[ip], b.window)
	if len(b.all) >= b.global {
		return false, errors.New("too many builds were requested in the last hour - try again later")
	}
	if len(b.byIP[ip]) >= b.perIP {
		return false, errors.New("you requested too many builds in the last hour - try again later")
	}
	now := time.Now()
	b.all = append(b.all, now)
	b.byIP[ip] = append(b.byIP[ip], now)
	b.requested[key] = now
	return true, nil
}

func (b *buildLimiter) wasRequested(key string) (time.Time, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.requested[key]
	return t, ok && time.Since(t) < b.retryWait
}

func (b *buildLimiter) forget(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.requested, key)
}

func (a *app) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/versions", a.handleVersions)
	mux.HandleFunc("GET /api/v1/versions/{version}/extensions", a.handleExtensions)
	mux.HandleFunc("GET /api/v1/platforms", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, platforms) })
	mux.HandleFunc("POST /api/v1/schematics", a.handlePostSchematic)
	mux.HandleFunc("GET /api/v1/schematics/{id}", a.handleGetSchematic)
	mux.HandleFunc("GET /api/v1/images/{id}/{version}/{arch}", a.handleImageStatus)
	mux.HandleFunc("POST /api/v1/images/{id}/{version}/{arch}", a.handleImageBuild)
	mux.HandleFunc("GET /api/v1/updates/{id}", a.handleUpdates)
	mux.HandleFunc("GET /image/{id}/{version}/{arch}/{file}", a.handleDownload)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func clientIP(r *http.Request) string {
	// Behind the HAProxy front end: the last X-Forwarded-For hop is the
	// one it added.
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[len(parts)-1])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// compareVersions orders CalVer tags: v2026.09.30 < v2026.09.30-2 < v2026.10.01.
func compareVersions(a, b string) int {
	pa, sa, _ := strings.Cut(strings.TrimPrefix(a, "v"), "-")
	pb, sb, _ := strings.Cut(strings.TrimPrefix(b, "v"), "-")
	if c := strings.Compare(pa, pb); c != 0 {
		return c
	}
	na, _ := strconv.Atoi(sa)
	nb, _ := strconv.Atoi(sb)
	return na - nb
}

func (a *app) handleVersions(w http.ResponseWriter, r *http.Request) {
	rels, err := a.gh.Releases(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rels)
}

func (a *app) handleExtensions(w http.ResponseWriter, r *http.Request) {
	version := r.PathValue("version")
	if !validVersion(version) {
		writeError(w, http.StatusBadRequest, "invalid version")
		return
	}
	c, err := a.gh.Catalog(r.Context(), version)
	if errors.Is(err, errNotFound) {
		writeError(w, http.StatusNotFound, "no such release")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if c == nil {
		c = &schematic.Catalog{Version: version, Extensions: []schematic.CatalogEntry{}}
	}
	writeJSON(w, http.StatusOK, c)
}

type schematicView struct {
	ID         string   `json:"id"`
	Extensions []string `json:"extensions"`
	YAML       string   `json:"yaml"`
	JSON       string   `json:"json"`
	Default    bool     `json:"default"`
}

func viewOf(sc *schematic.Schematic) schematicView {
	exts := sc.Extensions()
	if exts == nil {
		exts = []string{}
	}
	return schematicView{ID: sc.ID(), Extensions: exts, YAML: sc.YAML(), JSON: string(sc.Canonical()), Default: sc.ID() == schematic.DefaultID()}
}

// handlePostSchematic stores a schematic whose extensions some release
// offers, and returns its ID.
func (a *app) handlePostSchematic(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(io.LimitReader(r.Body, 16<<10))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	sc, err := schematic.Parse(data)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.checkKnownExtensions(r.Context(), sc); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := a.store.PutSchematic(sc); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, viewOf(sc))
}

// checkKnownExtensions refuses a schematic naming an extension no
// release offers - stored schematics stay meaningful.
func (a *app) checkKnownExtensions(ctx context.Context, sc *schematic.Schematic) error {
	exts := sc.Extensions()
	if len(exts) == 0 {
		return nil
	}
	rels, err := a.gh.Releases(ctx)
	if err != nil {
		return err
	}
	known := map[string]bool{}
	for _, rel := range rels {
		if !rel.Schematics {
			continue
		}
		c, err := a.gh.Catalog(ctx, rel.Version)
		if err != nil || c == nil {
			continue
		}
		for _, e := range c.Extensions {
			known[e.Name] = true
		}
	}
	for _, e := range exts {
		if !known[e] {
			return fmt.Errorf("no release offers the extension %q", e)
		}
	}
	return nil
}

func (a *app) handleGetSchematic(w http.ResponseWriter, r *http.Request) {
	sc, err := a.store.Schematic(r.PathValue("id"))
	if errors.Is(err, errNotFound) {
		writeError(w, http.StatusNotFound, "unknown schematic")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, viewOf(sc))
}

// imageStatus is a (schematic, version, arch) image set.
type imageStatus struct {
	Schematic string       `json:"schematic"`
	Version   string       `json:"version"`
	Arch      string       `json:"arch"`
	State     string       `json:"state"` // ready, building, failed, none, unavailable
	Message   string       `json:"message,omitempty"`
	RunURL    string       `json:"run_url,omitempty"`
	Files     []statusFile `json:"files,omitempty"`
}

type statusFile struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Size   int64  `json:"size,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
}

func buildKey(id, version, arch string) string { return id + " " + version + " " + arch }

// runName is the build workflow's run-name for a build.
func runName(id, version, arch string) string {
	return fmt.Sprintf("schematic %s %s %s", id, version, arch)
}

// status resolves where an image set stands.
func (a *app) status(ctx context.Context, id, version, arch string) (*imageStatus, *schematic.Schematic, int, error) {
	if !validVersion(version) || !validArch(arch) {
		return nil, nil, http.StatusBadRequest, errors.New("invalid version or architecture")
	}
	sc, err := a.store.Schematic(id)
	if errors.Is(err, errNotFound) {
		return nil, nil, http.StatusNotFound, errors.New("unknown schematic - create it first")
	}
	if err != nil {
		return nil, nil, http.StatusInternalServerError, err
	}
	rel, err := a.gh.Release(ctx, version)
	if errors.Is(err, errNotFound) {
		return nil, nil, http.StatusNotFound, errors.New("no such release")
	}
	if err != nil {
		return nil, nil, http.StatusBadGateway, err
	}
	st := &imageStatus{Schematic: id, Version: version, Arch: arch}

	// The default schematic is what the release itself published.
	if id == schematic.DefaultID() {
		st.State = "ready"
		for _, name := range a.filesFor(arch) {
			if url, ok := rel.Assets[name]; ok {
				st.Files = append(st.Files, statusFile{Name: name, URL: url})
			}
		}
		return st, sc, http.StatusOK, nil
	}
	if !rel.Schematics {
		st.State, st.Message = "unavailable", version+" predates custom schematics: only its default images exist"
		return st, sc, http.StatusOK, nil
	}
	c, err := a.gh.Catalog(ctx, version)
	if err != nil {
		return nil, nil, http.StatusBadGateway, err
	}
	if err := c.Check(sc, arch); err != nil {
		st.State, st.Message = "unavailable", err.Error()
		return st, sc, http.StatusOK, nil
	}
	m, err := a.store.Manifest(id, version, arch)
	if err != nil {
		return nil, nil, http.StatusInternalServerError, err
	}
	if m != nil {
		st.State, st.RunURL = "ready", m.RunURL
		for _, f := range m.Files {
			st.Files = append(st.Files, statusFile{Name: f.Name, URL: a.fileURL(id, version, arch, f.Name), Size: f.Size, SHA256: f.SHA256})
		}
		return st, sc, http.StatusOK, nil
	}
	st.State = "none"
	if _, ok := a.builds.wasRequested(buildKey(id, version, arch)); ok {
		st.State = "building"
		if run, err := a.gh.LatestRun(ctx, runName(id, version, arch)); err == nil && run != nil {
			st.RunURL = run.URL
			if run.Status == "completed" && run.Conclusion != "success" {
				st.State, st.Message = "failed", "the build "+run.Conclusion
			}
		}
	}
	return st, sc, http.StatusOK, nil
}

func (a *app) filesFor(arch string) []string {
	var out []string
	if arch == "amd64" {
		out = append(out, bundleFiles...)
	}
	for _, p := range platforms {
		if p.Arch == arch {
			out = append(out, p.File)
		}
	}
	return out
}

func (a *app) fileURL(id, version, arch, name string) string {
	return fmt.Sprintf("%s/image/%s/%s/%s/%s", a.publicURL, id, version, arch, name)
}

func (a *app) handleImageStatus(w http.ResponseWriter, r *http.Request) {
	st, _, code, err := a.status(r.Context(), r.PathValue("id"), r.PathValue("version"), r.PathValue("arch"))
	if err != nil {
		writeError(w, code, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// handleImageBuild starts building an image set that doesn't exist yet.
func (a *app) handleImageBuild(w http.ResponseWriter, r *http.Request) {
	id, version, arch := r.PathValue("id"), r.PathValue("version"), r.PathValue("arch")
	st, sc, code, err := a.status(r.Context(), id, version, arch)
	if err != nil {
		writeError(w, code, err.Error())
		return
	}
	if st.State == "ready" || st.State == "building" || st.State == "unavailable" {
		writeJSON(w, http.StatusOK, st)
		return
	}
	if err := a.startBuild(r.Context(), sc, version, arch, clientIP(r), st.State == "failed"); err != nil {
		writeError(w, http.StatusTooManyRequests, err.Error())
		return
	}
	st.State = "building"
	writeJSON(w, http.StatusAccepted, st)
}

func (a *app) startBuild(ctx context.Context, sc *schematic.Schematic, version, arch, ip string, retry bool) error {
	key := buildKey(sc.ID(), version, arch)
	start, err := a.builds.allow(key, ip, retry)
	if err != nil || !start {
		return err
	}
	err = a.gh.Dispatch(ctx, map[string]string{
		"version":      version,
		"arch":         arch,
		"schematic":    string(sc.Canonical()),
		"schematic_id": sc.ID(),
	})
	if err != nil {
		a.builds.forget(key)
		return err
	}
	log.Printf("build requested: %s %s %s (by %s)", sc.ID(), version, arch, ip)
	return nil
}

// update is what a node (or its Controller) needs to update: the newest
// release built from its schematic.
type update struct {
	Schematic  string `json:"schematic"`
	Version    string `json:"version"`
	ReleaseURL string `json:"release_url"`
	// BundleURL is UpgradeRequest.source.reference: the node fetches
	// rootfs.squashfs, rootfs.verity and uki-<slot>.efi under it.
	BundleURL string `json:"bundle_url,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
	State     string `json:"state"` // ready, building, failed, unavailable
	Message   string `json:"message,omitempty"`
	UpToDate  bool   `json:"up_to_date"`
}

func (a *app) handleUpdates(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	arch := r.URL.Query().Get("arch")
	if arch == "" {
		arch = "amd64"
	}
	if arch != "amd64" {
		writeError(w, http.StatusBadRequest, "updates are published for amd64 only")
		return
	}
	sc, err := a.store.Schematic(id)
	if errors.Is(err, errNotFound) {
		writeError(w, http.StatusNotFound, "unknown schematic - this site hasn't built images for it")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	rels, err := a.gh.Releases(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	var target *release
	for i := range rels { // newest first
		rel := &rels[i]
		if id == schematic.DefaultID() {
			if _, ok := rel.Assets["uki-b.efi"]; ok {
				target = rel
				break
			}
			continue
		}
		if !rel.Schematics {
			continue
		}
		c, err := a.gh.Catalog(r.Context(), rel.Version)
		if err == nil && c != nil && c.Check(sc, arch) == nil {
			target = rel
			break
		}
	}
	if target == nil {
		writeError(w, http.StatusNotFound, "no release offers this schematic's extensions")
		return
	}
	up := update{Schematic: id, Version: target.Version, ReleaseURL: target.URL}
	up.UpToDate = r.URL.Query().Get("from") == target.Version

	if id == schematic.DefaultID() {
		up.State = "ready"
		up.BundleURL = strings.TrimSuffix(target.Assets["uki-b.efi"], "/uki-b.efi")
		up.SHA256 = a.releaseSHA256(r.Context(), target)
		writeJSON(w, http.StatusOK, up)
		return
	}
	st, _, code, err := a.status(r.Context(), id, target.Version, arch)
	if err != nil {
		writeError(w, code, err.Error())
		return
	}
	up.State, up.Message = st.State, st.Message
	switch st.State {
	case "ready":
		up.BundleURL = fmt.Sprintf("%s/image/%s/%s/%s", a.publicURL, id, target.Version, arch)
		if m, _ := a.store.Manifest(id, target.Version, arch); m != nil {
			if f, ok := m.file("rootfs.squashfs"); ok {
				up.SHA256 = f.SHA256
			}
		}
	case "none", "failed":
		// A node asking for its update is reason enough to build it.
		if err := a.startBuild(r.Context(), sc, target.Version, arch, clientIP(r), st.State == "failed"); err != nil {
			up.Message = err.Error()
		} else {
			up.State = "building"
		}
	}
	writeJSON(w, http.StatusOK, up)
}

// releaseSHA256 reads a release's rootfs.squashfs.sha256 asset.
func (a *app) releaseSHA256(ctx context.Context, rel *release) string {
	url, ok := rel.Assets["rootfs.squashfs.sha256"]
	if !ok {
		return ""
	}
	resp, err := a.gh.do(ctx, http.MethodGet, url, nil, false)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	fields := strings.Fields(string(data))
	if resp.StatusCode != http.StatusOK || len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// handleDownload serves a built file, or redirects to the release asset
// for the default schematic.
func (a *app) handleDownload(w http.ResponseWriter, r *http.Request) {
	id, version, arch, file := r.PathValue("id"), r.PathValue("version"), r.PathValue("arch"), r.PathValue("file")
	if !schematic.ValidID(id) || !validVersion(version) || !validArch(arch) || !servedFile(arch, file) {
		http.NotFound(w, r)
		return
	}
	if id == schematic.DefaultID() {
		rel, err := a.gh.Release(r.Context(), version)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		url, ok := rel.Assets[file]
		if !ok {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, url, http.StatusFound)
		return
	}
	m, err := a.store.Manifest(id, version, arch)
	if err != nil || m == nil {
		http.Error(w, "not built yet", http.StatusNotFound)
		return
	}
	if _, ok := m.file(file); !ok && file != "manifest.json" {
		http.NotFound(w, r)
		return
	}
	if strings.HasSuffix(file, ".img") || strings.HasSuffix(file, ".iso") || strings.HasSuffix(file, ".qcow2") || strings.HasSuffix(file, ".vmdk") {
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", "janus-"+version+"-"+id[:8]+"-"+file))
	}
	http.ServeFile(w, r, filepath.Join(a.store.ImageDir(id, version, arch), file))
}
