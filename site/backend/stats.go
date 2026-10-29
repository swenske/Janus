package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/swenske/Janus/internal/schematic"
)

// downloadStats counts the images and updates the site serves, by day,
// for the builder's cards (GET /api/v1/stats/downloads). What, never
// who: a day, a schematic, a release and what was fetched - no address,
// no user agent, nothing about the client.
//
// Counted here: a platform's image (from the builder, or a Controller
// creating a machine) and a custom schematic's update - its
// rootfs.squashfs, fetched by a node or its Controller's relay. The
// default schematic's updates never reach the site (nodes and
// Controllers fetch them from the GitHub Release): they come from
// GitHub's own count of each release's rootfs.squashfs downloads,
// sampled - what it went up by since the last sample, counted on the
// day it was seen (sampleGitHub).
type downloadStats struct {
	path string
	now  func() time.Time

	mu       sync.Mutex
	data     statsFile
	dirty    bool
	computed map[int]*downloadSummary // by window, until summaryTTL
	at       map[int]time.Time
}

// statsFile is <data>/stats/downloads.json.
type statsFile struct {
	// Days: "2006-01-02" (UTC) -> "<schematic> <version> <kind>" -> count.
	Days map[string]map[string]int64 `json:"days"`
	// GitHub: a release's rootfs.squashfs download count when last
	// sampled, by version.
	GitHub map[string]int64 `json:"github"`
}

const (
	// kindUpdate is an update bundle's download; any other kind is the
	// platform (platform.ID) whose image was downloaded.
	kindUpdate = "update"
	// statsRetention is how long a day's counts are kept.
	statsRetention = 400 * 24 * time.Hour
	summaryTTL     = 5 * time.Minute
	dayLayout      = "2006-01-02"
)

func newDownloadStats(dataDir string) (*downloadStats, error) {
	dir := filepath.Join(dataDir, "stats")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &downloadStats{path: filepath.Join(dir, "downloads.json"), now: time.Now,
		data: statsFile{Days: map[string]map[string]int64{}, GitHub: map[string]int64{}}}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &s.data); err != nil {
		return nil, err
	}
	if s.data.Days == nil {
		s.data.Days = map[string]map[string]int64{}
	}
	if s.data.GitHub == nil {
		s.data.GitHub = map[string]int64{}
	}
	return s, nil
}

// downloadKind is what fetching file counts as: a platform's image, an
// update, or nothing ("") - the bundle's other files, the manifest.
func downloadKind(arch, file string) string {
	if file == "rootfs.squashfs" {
		return kindUpdate
	}
	for _, p := range platforms {
		if p.Arch == arch && p.File == file {
			return p.ID
		}
	}
	return ""
}

// countable reports whether a request is a download to count: a GET
// from the file's start - not a HEAD, nor a resumed transfer's later
// range.
func countable(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	rg := r.Header.Get("Range")
	return rg == "" || strings.HasPrefix(rg, "bytes=0-")
}

func statsKey(id, version, kind string) string { return id + " " + version + " " + kind }

func (s *downloadStats) add(day, key string, n int64) {
	if s.data.Days[day] == nil {
		s.data.Days[day] = map[string]int64{}
	}
	s.data.Days[day][key] += n
	s.dirty = true
}

// count records one download of kind (downloadKind) for a schematic's
// release.
func (s *downloadStats) count(id, version, kind string) {
	if s == nil || kind == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.add(s.now().UTC().Format(dayLayout), statsKey(id, version, kind), 1)
}

// sampleGitHub counts the default schematic's updates since the last
// sample, from GitHub's count of each release's rootfs.squashfs: a
// release seen for the first time only sets where its count stands, and
// so does a pre-release - testers' downloads aren't updates, and a
// promoted one counts from its promotion on.
func (s *downloadStats) sampleGitHub(rels []release) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	day := s.now().UTC().Format(dayLayout)
	for _, rel := range rels {
		n, ok := rel.Downloads["rootfs.squashfs"]
		if !ok {
			continue
		}
		last, seen := s.data.GitHub[rel.Version]
		if seen && n == last {
			continue
		}
		if seen && !rel.Prerelease && n > last {
			s.add(day, statsKey(schematic.DefaultID(), rel.Version, kindUpdate), n-last)
		}
		s.data.GitHub[rel.Version] = n
		s.dirty = true
	}
}

// flush writes the counts if they changed, dropping the days older than
// statsRetention - tmp, fsync, rename, so a power cut leaves the old
// file or the new one.
func (s *downloadStats) flush() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return nil
	}
	cut := s.now().UTC().Add(-statsRetention).Format(dayLayout)
	for day := range s.data.Days {
		if day < cut {
			delete(s.data.Days, day)
		}
	}
	data, err := json.Marshal(s.data)
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	if d, err := os.Open(filepath.Dir(s.path)); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	s.dirty = false
	return nil
}

// run writes the counts every minute until ctx ends, then a last time.
func (s *downloadStats) run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = s.flush()
			return
		case <-t.C:
			_ = s.flush()
		}
	}
}

// window sums the last days days' counts by key, and says the first day
// counted at all.
func (s *downloadStats) window(days int) (map[string]int64, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	start := s.now().UTC().AddDate(0, 0, -(days - 1)).Format(dayLayout)
	sums := map[string]int64{}
	first := ""
	for day, keys := range s.data.Days {
		if first == "" || day < first {
			first = day
		}
		if day < start {
			continue
		}
		for k, n := range keys {
			sums[k] += n
		}
	}
	if first < start {
		first = ""
	}
	return sums, first
}

func (s *downloadStats) cachedSummary(days int) *downloadSummary {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sum := s.computed[days]; sum != nil && s.now().Sub(s.at[days]) < summaryTTL {
		return sum
	}
	return nil
}

func (s *downloadStats) keepSummary(days int, sum *downloadSummary) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.computed == nil {
		s.computed, s.at = map[int]*downloadSummary{}, map[int]time.Time{}
	}
	s.computed[days], s.at[days] = sum, s.now()
}

// counts are a choice's downloads: platforms' images, and updates.
type counts struct {
	Images  int64 `json:"images"`
	Updates int64 `json:"updates"`
}

func bump(m map[string]counts, key, kind string, n int64) {
	c := m[key]
	if kind == kindUpdate {
		c.Updates += n
	} else {
		c.Images += n
	}
	m[key] = c
}

// downloadSummary is GET /api/v1/stats/downloads: the last Days days'
// downloads by choice - by platform, release, HAProxy branch and kernel
// track (as each release built them: an image following the default is
// counted on the branch it got), console and extension. Never by
// schematic.
type downloadSummary struct {
	Days int `json:"days"`
	// Since is the first day counted, when that's inside the window.
	Since      string            `json:"since,omitempty"`
	Total      counts            `json:"total"`
	Platforms  map[string]int64  `json:"platforms"`
	Versions   map[string]counts `json:"versions"`
	HAProxy    map[string]counts `json:"haproxy"`
	Kernel     map[string]counts `json:"kernel"`
	Console    map[string]counts `json:"console"`
	Extensions map[string]counts `json:"extensions"`
}

// choices are what a schematic's image of a release is made with.
type choices struct {
	haproxy, kernel, console string
	extensions               []string
}

// choicesOf resolves a schematic's choices for a release: false when the
// schematic isn't known here. HAProxy and kernel stay "" for a release
// whose catalog offers no choice.
func (a *app) choicesOf(ctx context.Context, id, version, arch string) (choices, bool) {
	sc, err := a.store.Schematic(id)
	if err != nil {
		return choices{}, false
	}
	ch := choices{console: sc.ConsoleChoice(), extensions: sc.Extensions()}
	if c, err := a.gh.Catalog(ctx, version); err == nil && c != nil {
		if res, err := c.Resolve(sc, arch); err == nil {
			ch.haproxy, ch.kernel = res.HAProxy.Name, res.Kernel.Name
		}
	}
	return ch, true
}

func kindArch(kind string) string {
	for _, p := range platforms {
		if p.ID == kind {
			return p.Arch
		}
	}
	return "amd64" // updates are published for amd64 only
}

func (a *app) downloadSummary(ctx context.Context, days int) *downloadSummary {
	if sum := a.stats.cachedSummary(days); sum != nil {
		return sum
	}
	sums, since := a.stats.window(days)
	sum := &downloadSummary{Days: days, Since: since, Platforms: map[string]int64{}, Versions: map[string]counts{},
		HAProxy: map[string]counts{}, Kernel: map[string]counts{}, Console: map[string]counts{}, Extensions: map[string]counts{}}
	keys := make([]string, 0, len(sums))
	for k := range sums {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	resolved := map[string]*choices{}
	for _, key := range keys {
		n := sums[key]
		parts := strings.SplitN(key, " ", 3)
		if len(parts) != 3 {
			continue
		}
		id, version, kind := parts[0], parts[1], parts[2]
		if kind == kindUpdate {
			sum.Total.Updates += n
		} else {
			sum.Total.Images += n
			sum.Platforms[kind] += n
		}
		bump(sum.Versions, version, kind, n)
		arch := kindArch(kind)
		rk := id + " " + version + " " + arch
		ch, done := resolved[rk]
		if !done {
			if c, ok := a.choicesOf(ctx, id, version, arch); ok {
				ch = &c
			}
			resolved[rk] = ch
		}
		if ch == nil {
			continue
		}
		if ch.haproxy != "" {
			bump(sum.HAProxy, ch.haproxy, kind, n)
		}
		if ch.kernel != "" {
			bump(sum.Kernel, ch.kernel, kind, n)
		}
		bump(sum.Console, ch.console, kind, n)
		for _, e := range ch.extensions {
			bump(sum.Extensions, e, kind, n)
		}
	}
	a.stats.keepSummary(days, sum)
	return sum
}

func (a *app) handleDownloadStats(w http.ResponseWriter, r *http.Request) {
	days := 30
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 365 {
			writeError(w, http.StatusBadRequest, "days is 1 to 365")
			return
		}
		days = n
	}
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, http.StatusOK, a.downloadSummary(r.Context(), days))
}
