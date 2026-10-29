package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swenske/Janus/internal/schematic"
)

// builtOnSite stores a finished build of sc for v2026.10.02 on the
// site: the bundle and a disk image.
func builtOnSite(t *testing.T, a *app, sc *schematic.Schematic) string {
	t.Helper()
	id, _ := a.store.PutSchematic(sc)
	dir := a.store.ImageDir(id, "v2026.10.02", "amd64")
	_ = os.MkdirAll(dir, 0o755)
	var files []manifestFile
	for _, f := range []string{"rootfs.squashfs", "rootfs.squashfs.sha256", "rootfs.verity", "uki-a.efi", "uki-b.efi", "janus.qcow2"} {
		data := []byte("content of " + f)
		_ = os.WriteFile(filepath.Join(dir, f), data, 0o644)
		sum := sha256.Sum256(data)
		files = append(files, manifestFile{Name: f, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])})
	}
	m, _ := json.Marshal(manifest{Schematic: id, Version: "v2026.10.02", Arch: "amd64", Files: files})
	_ = os.WriteFile(filepath.Join(dir, "manifest.json"), m, 0o644)
	return id
}

func newMirror(t *testing.T, site string) (*mirror, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	st, _ := newStore(dir)
	stats, _ := newDownloadStats(dir)
	m := &mirror{store: st, stats: stats, site: site, token: "mtok", publicURL: "https://mirror.test", client: http.DefaultClient}
	mux := http.NewServeMux()
	m.routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return m, srv
}

func authGet(t *testing.T, url, token string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// The mirror copies the bundles of the site's newest releases - checked,
// the disk images left - answers the update questions as the site does,
// and its downloads are counted on the site's page, not its copying.
func TestMirror(t *testing.T) {
	a, _, site := newTestApp(t)
	a.mirrorToken = "mtok"
	sc, _ := schematic.Parse([]byte(`{"customization":{"extensions":["qemu-guest-agent"]}}`))
	id := builtOnSite(t, a, sc)

	if code, _ := authGet(t, site.URL+"/api/v1/mirror/index", ""); code != http.StatusUnauthorized {
		t.Errorf("index without the token: %d", code)
	}
	if code, _ := authGet(t, site.URL+"/api/v1/mirror/index", "nope"); code != http.StatusUnauthorized {
		t.Errorf("index with another token: %d", code)
	}
	code, body := authGet(t, site.URL+"/api/v1/mirror/index", "mtok")
	var idx mirrorIndex
	if code != http.StatusOK || json.Unmarshal([]byte(body), &idx) != nil {
		t.Fatalf("index: %d %s", code, body)
	}
	// v2026.10.03 is a pre-release, v2026.09.30-3 predates schematics.
	if len(idx.Releases) != 1 || idx.Releases[0].Version != "v2026.10.02" || len(idx.Builds) != 1 || len(idx.Builds[0].Manifest.Files) != 5 {
		t.Fatalf("index %+v", idx)
	}

	m, mirrorSrv := newMirror(t, site.URL)
	if err := m.sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	dir := m.store.ImageDir(id, "v2026.10.02", "amd64")
	if _, err := os.Stat(filepath.Join(dir, "janus.qcow2")); err == nil {
		t.Error("the mirror copied a disk image")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "uki-b.efi")); string(got) != "content of uki-b.efi" {
		t.Errorf("uki-b.efi: %q", got)
	}
	// Copying isn't downloading.
	var sum downloadSummary
	call(t, "GET", site.URL+"/api/v1/stats/downloads", "", &sum)
	if sum.Total.Updates != 0 {
		t.Errorf("the site counted the mirror's copy: %+v", sum.Total)
	}

	// What a Controller asks (nodeproxy's factoryUpdateResponse).
	var up struct {
		Schematic, Version, State string
		BundleURL                 string `json:"bundle_url"`
		SHA256                    string `json:"sha256"`
		ReleaseURL                string `json:"release_url"`
	}
	code, body = authGet(t, mirrorSrv.URL+"/api/v1/updates/"+id+"?arch=amd64&from=v2026.09.30-3", "")
	if code != http.StatusOK || json.Unmarshal([]byte(body), &up) != nil || up.State != "ready" || up.Version != "v2026.10.02" ||
		up.BundleURL != "https://mirror.test/image/"+id+"/v2026.10.02/amd64" || len(up.SHA256) != 64 || !strings.Contains(up.ReleaseURL, "v2026.10.02") {
		t.Fatalf("update: %d %s", code, body)
	}
	var st imageStatus
	call(t, "GET", mirrorSrv.URL+"/api/v1/images/"+id+"/v2026.10.02/amd64", "", &st)
	if st.State != "ready" || len(st.Files) != 5 {
		t.Errorf("images: %+v", st)
	}
	other := strings.Repeat("a", 64)
	if code, body := authGet(t, mirrorSrv.URL+"/api/v1/updates/"+other, ""); code != http.StatusNotFound || !strings.Contains(body, "this mirror has no update") {
		t.Errorf("an update it hasn't got: %d %s", code, body)
	}
	call(t, "GET", mirrorSrv.URL+"/api/v1/images/"+other+"/v2026.10.02/amd64", "", &st)
	if st.State != "unavailable" {
		t.Errorf("a build it hasn't got: %+v", st)
	}
	for _, p := range []string{"/", "/builder", "/api/v1/versions", "/image/" + id + "/v2026.10.02/amd64/janus.qcow2"} {
		if code, _ := authGet(t, mirrorSrv.URL+p, ""); code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
			t.Errorf("the mirror answers %s: %d", p, code)
		}
	}

	// A node downloads its update from the mirror: counted there, and on
	// the site's page once the mirror sends its counts.
	if code, body := authGet(t, mirrorSrv.URL+"/image/"+id+"/v2026.10.02/amd64/rootfs.squashfs", ""); code != http.StatusOK || body != "content of rootfs.squashfs" {
		t.Fatalf("download: %d %q", code, body)
	}
	if err := m.sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	a.stats.computed = nil
	call(t, "GET", site.URL+"/api/v1/stats/downloads", "", &sum)
	if sum.Total.Updates != 1 || sum.Extensions["qemu-guest-agent"].Updates != 1 {
		t.Errorf("the mirror's download on the site's page: %+v", sum)
	}

	// A build the site no longer lists goes.
	_ = os.RemoveAll(a.store.ImageDir(id, "v2026.10.02", "amd64"))
	if err := m.sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("a build the index dropped is still there: %v", err)
	}
	if ids, _ := m.store.SchematicIDs(); len(ids) != 0 {
		t.Errorf("schematics left: %v", ids)
	}
}

// A file that isn't what the manifest says is never served: the build
// stays out until a copy matches.
func TestMirrorChecksFiles(t *testing.T) {
	a, _, site := newTestApp(t)
	a.mirrorToken = "mtok"
	sc, _ := schematic.Parse([]byte(`{"customization":{"extensions":["qemu-guest-agent"]}}`))
	id := builtOnSite(t, a, sc)
	_ = os.WriteFile(filepath.Join(a.store.ImageDir(id, "v2026.10.02", "amd64"), "uki-a.efi"), []byte("tampered"), 0o644)
	m, mirrorSrv := newMirror(t, site.URL)
	if err := m.sync(context.Background()); err == nil || !strings.Contains(err.Error(), "uki-a.efi") {
		t.Fatalf("sync: %v", err)
	}
	if man, _ := m.store.Manifest(id, "v2026.10.02", "amd64"); man != nil {
		t.Error("a build with a bad file got its manifest")
	}
	if code, _ := authGet(t, mirrorSrv.URL+"/api/v1/updates/"+id, ""); code != http.StatusNotFound {
		t.Errorf("update offered from a bad copy: %d", code)
	}
}

// The site takes the mirror's counts only with its token.
func TestMirrorStatsNeedTheToken(t *testing.T) {
	a, _, site := newTestApp(t)
	a.mirrorToken = "mtok"
	body := `{"2026-10-29":{"` + strings.Repeat("a", 64) + ` v2026.10.02 update":5}}`
	req, _ := http.NewRequest(http.MethodPost, site.URL+"/api/v1/mirror/stats", strings.NewReader(body))
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("without the token: %d", resp.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodPost, site.URL+"/api/v1/mirror/stats", strings.NewReader(`{"2026-10-29":{"x":1}}`))
	req.Header.Set("Authorization", "Bearer mtok")
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a malformed count: %d", resp.StatusCode)
	}
}

// The schematics nodes run most are built first; those insights doesn't
// know keep their order, after.
func TestPriorityOrder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ptok" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"schematics":[{"id":"c","nodes":9},{"id":"a","nodes":3}]}`)
	}))
	defer srv.Close()
	p := &priority{url: srv.URL, token: "ptok", client: http.DefaultClient}
	if got := strings.Join(p.order(context.Background(), []string{"a", "b", "c", "d"}), ","); got != "c,a,b,d" {
		t.Errorf("order %s", got)
	}
	p.token = "wrong" // insights refusing: the last order stays
	if got := strings.Join(p.order(context.Background(), []string{"d", "c", "b", "a"}), ","); got != "c,a,d,b" {
		t.Errorf("order after a failure %s", got)
	}
	var none *priority
	if got := strings.Join(none.order(context.Background(), []string{"b", "a"}), ","); got != "b,a" {
		t.Errorf("no priority: %s", got)
	}
}
