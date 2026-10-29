package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swenske/Janus/internal/schematic"
)

func get(t *testing.T, url string, header ...string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if len(header) == 2 {
		req.Header.Set(header[0], header[1])
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// What counts: a platform's image or an update fetched from its start -
// not a HEAD, a resumed range, the bundle's other files; the default
// schematic's images on their way to GitHub. The summary breaks them
// down by choice, as each release built them.
func TestDownloadCounting(t *testing.T) {
	a, gh, srv := newTestApp(t)
	gh.catalog = variantCatalog
	sc, _ := schematic.Parse([]byte(`{"customization":{"extensions":["qemu-guest-agent"],"haproxy":"3.2"}}`))
	id, _ := a.store.PutSchematic(sc)
	dir := a.store.ImageDir(id, "v2026.10.02", "amd64")
	_ = os.MkdirAll(dir, 0o755)
	var files []manifestFile
	for _, f := range []string{"janus.qcow2", "rootfs.squashfs", "uki-a.efi"} {
		_ = os.WriteFile(filepath.Join(dir, f), []byte(strings.Repeat("x", 200)), 0o644)
		files = append(files, manifestFile{Name: f, Size: 200})
	}
	m, _ := json.Marshal(manifest{Schematic: id, Version: "v2026.10.02", Arch: "amd64", Files: files})
	_ = os.WriteFile(filepath.Join(dir, "manifest.json"), m, 0o644)

	base := srv.URL + "/image/" + id + "/v2026.10.02/amd64/"
	for _, c := range []struct {
		file, header, value string
		code                int
	}{
		{"janus.qcow2", "", "", 200},
		{"janus.qcow2", "Range", "bytes=0-99", 206},
		{"janus.qcow2", "Range", "bytes=100-", 206}, // a resumed download: counted once already
		{"rootfs.squashfs", "", "", 200},
		{"uki-a.efi", "", "", 200},
		{"manifest.json", "", "", 200},
	} {
		h := []string{}
		if c.header != "" {
			h = []string{c.header, c.value}
		}
		if code := get(t, base+c.file, h...); code != c.code {
			t.Errorf("%s %s: %d", c.file, c.value, code)
		}
	}
	req, _ := http.NewRequest(http.MethodHead, base+"janus.qcow2", nil)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
	}
	def := srv.URL + "/image/" + schematic.DefaultID() + "/v2026.09.30-3/amd64/"
	if code := get(t, def+"janus.iso"); code != 302 {
		t.Errorf("default image: %d", code)
	}
	if code := get(t, def+"uki-b.efi"); code != 302 {
		t.Errorf("default bundle file: %d", code)
	}

	var sum downloadSummary
	if code := call(t, "GET", srv.URL+"/api/v1/stats/downloads", "", &sum); code != 200 {
		t.Fatalf("stats: %d", code)
	}
	want := downloadSummary{Days: 30, Since: time.Now().UTC().Format(dayLayout), Total: counts{Images: 3, Updates: 1},
		Platforms: map[string]int64{"proxmox": 2, "iso": 1},
		Versions:  map[string]counts{"v2026.10.02": {Images: 2, Updates: 1}, "v2026.09.30-3": {Images: 1}},
		// v2026.09.30-3 offers no choice of branch or track: its image is
		// counted on neither.
		HAProxy:    map[string]counts{"3.2": {Images: 2, Updates: 1}},
		Kernel:     map[string]counts{"stable": {Images: 2, Updates: 1}},
		Console:    map[string]counts{"serial": {Images: 3, Updates: 1}},
		Extensions: map[string]counts{"qemu-guest-agent": {Images: 2, Updates: 1}},
	}
	got, _ := json.Marshal(sum)
	exp, _ := json.Marshal(want)
	if string(got) != string(exp) {
		t.Errorf("summary\n got %s\nwant %s", got, exp)
	}
	// Nothing in it names a schematic.
	if strings.Contains(string(got), id) {
		t.Errorf("the summary names a schematic: %s", got)
	}
	var e map[string]string
	if code := call(t, "GET", srv.URL+"/api/v1/stats/downloads?days=400", "", &e); code != 400 {
		t.Errorf("days=400: %d %v", code, e)
	}
}

// The default schematic's images are linked through the site, to be
// counted; its update bundle straight to GitHub, as nodes fetch it.
func TestDefaultImagesLinkThroughTheSite(t *testing.T) {
	_, _, srv := newTestApp(t)
	var st imageStatus
	call(t, "GET", srv.URL+"/api/v1/images/"+schematic.DefaultID()+"/v2026.09.30-3/amd64", "", &st)
	urls := map[string]string{}
	for _, f := range st.Files {
		urls[f.Name] = f.URL
	}
	if urls["janus.iso"] != "https://site.test/image/"+schematic.DefaultID()+"/v2026.09.30-3/amd64/janus.iso" {
		t.Errorf("janus.iso: %s", urls["janus.iso"])
	}
	if !strings.HasSuffix(urls["uki-b.efi"], "/dl/v2026.09.30-3/uki-b.efi") {
		t.Errorf("uki-b.efi: %s", urls["uki-b.efi"])
	}
}

// GitHub's counts: a release first seen, or a pre-release, only sets
// where its count stands; a release's count going up is its default
// schematic's updates, on the day seen - a promoted pre-release's from
// its promotion on.
func TestSampleGitHub(t *testing.T) {
	s, err := newDownloadStats(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC) }
	rel := func(v string, pre bool, n int64) release {
		return release{Version: v, Prerelease: pre, Downloads: map[string]int64{"rootfs.squashfs": n, "janus.iso": 99}}
	}
	s.sampleGitHub([]release{rel("v2026.10.29", false, 10), rel("v2026.10.29-2", true, 5)})
	if sums, _ := s.window(1); len(sums) != 0 {
		t.Fatalf("first sample counted %v", sums)
	}
	s.sampleGitHub([]release{rel("v2026.10.29", false, 13), rel("v2026.10.29-2", true, 7)})
	s.sampleGitHub([]release{rel("v2026.10.29", false, 13), rel("v2026.10.29-2", false, 9)}) // promoted
	sums, _ := s.window(1)
	def := schematic.DefaultID()
	if len(sums) != 2 || sums[statsKey(def, "v2026.10.29", kindUpdate)] != 3 || sums[statsKey(def, "v2026.10.29-2", kindUpdate)] != 2 {
		t.Errorf("sums %v", sums)
	}
}

// The counts survive a restart; days older than statsRetention go.
func TestDownloadStatsPersist(t *testing.T) {
	dir := t.TempDir()
	s, _ := newDownloadStats(dir)
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	s.count("a", "v2026.10.29", "kvm")
	s.count("a", "v2026.10.29", "kvm")
	s.add("2025-01-01", statsKey("a", "v2026.01.01", "kvm"), 5)
	if err := s.flush(); err != nil {
		t.Fatal(err)
	}
	r, err := newDownloadStats(dir)
	if err != nil {
		t.Fatal(err)
	}
	r.now = s.now
	sums, since := r.window(365)
	if len(sums) != 1 || sums[statsKey("a", "v2026.10.29", "kvm")] != 2 || since != "2026-10-29" {
		t.Errorf("reloaded %v since %q", sums, since)
	}
	if _, ok := r.data.Days["2025-01-01"]; ok {
		t.Error("a day older than the retention was kept")
	}
}
