package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/swenske/Janus/internal/schematic"
)

// fakeGitHub serves two releases - one with schematic support - and
// records workflow dispatches.
type fakeGitHub struct {
	srv        *httptest.Server
	mu         sync.Mutex
	dispatches []map[string]string
	runs       []map[string]any
	catalog    string // v2026.10.02's schematic-catalog.json, if not the default one
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{}
	mux := http.NewServeMux()
	f.srv = httptest.NewServer(mux)
	asset := func(tag, name string) map[string]string {
		return map[string]string{"name": name, "browser_download_url": f.srv.URL + "/dl/" + tag + "/" + name}
	}
	mux.HandleFunc("GET /repos/swenske/Janus/releases", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"tag_name": "v2026.10.02", "name": "Janus v2026.10.02 (Alpha)", "html_url": "https://github.com/swenske/Janus/releases/tag/v2026.10.02",
				"assets": []map[string]string{asset("v2026.10.02", "schematic-catalog.json"), asset("v2026.10.02", "uki-b.efi"), asset("v2026.10.02", "rootfs.squashfs.sha256"), asset("v2026.10.02", "janus.qcow2")}},
			{"tag_name": "v2026.09.30-3", "html_url": "https://github.com/swenske/Janus/releases/tag/v2026.09.30-3",
				"assets": []map[string]string{asset("v2026.09.30-3", "uki-b.efi"), asset("v2026.09.30-3", "janus.iso")}},
			{"tag_name": "v2099.01.01", "draft": true},
		})
	})
	mux.HandleFunc("GET /dl/v2026.10.02/schematic-catalog.json", func(w http.ResponseWriter, _ *http.Request) {
		if f.catalog != "" {
			_, _ = io.WriteString(w, f.catalog)
			return
		}
		_, _ = io.WriteString(w, `{"version":"v2026.10.02","extensions":[
			{"name":"node-exporter","version":"1.12.1","description":"metrics","arches":["amd64","arm64"]},
			{"name":"qemu-guest-agent","version":"11.1.2","description":"agent","arches":["amd64"]}]}`)
	})
	mux.HandleFunc("GET /dl/v2026.10.02/rootfs.squashfs.sha256", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("c", 64)+"  rootfs.squashfs\n")
	})
	mux.HandleFunc("POST /repos/swenske/Janus/actions/workflows/schematic-build.yml/dispatches", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no token", http.StatusUnauthorized)
			return
		}
		var body struct {
			Ref    string            `json:"ref"`
			Inputs map[string]string `json:"inputs"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.dispatches = append(f.dispatches, body.Inputs)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /repos/swenske/Janus/actions/workflows/schematic-build.yml/runs", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"workflow_runs": f.runs})
	})
	t.Cleanup(f.srv.Close)
	return f
}

func newTestApp(t *testing.T) (*app, *fakeGitHub, *httptest.Server) {
	gh := newFakeGitHub(t)
	st, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := &app{store: st, gh: newGitHub(gh.srv.URL, "swenske/Janus", "tok", "schematic-build.yml"), publicURL: "https://site.test", builds: newBuildLimiter()}
	mux := http.NewServeMux()
	a.routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return a, gh, srv
}

func call(t *testing.T, method, url, body string, out any) int {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func TestVersionsAndCatalog(t *testing.T) {
	_, _, srv := newTestApp(t)
	var rels []release
	if code := call(t, "GET", srv.URL+"/api/v1/versions", "", &rels); code != 200 || len(rels) != 2 {
		t.Fatalf("versions: %d %+v", code, rels)
	}
	if !rels[0].Schematics || rels[1].Schematics {
		t.Errorf("schematic support: %+v", rels)
	}
	var cat schematic.Catalog
	call(t, "GET", srv.URL+"/api/v1/versions/v2026.10.02/extensions", "", &cat)
	if len(cat.Extensions) != 2 {
		t.Errorf("catalog %+v", cat)
	}
	call(t, "GET", srv.URL+"/api/v1/versions/v2026.09.30-3/extensions", "", &cat)
	if len(cat.Extensions) != 0 {
		t.Errorf("an old release has no extensions: %+v", cat)
	}
}

func TestSchematicLifecycle(t *testing.T) {
	a, gh, srv := newTestApp(t)
	var v schematicView
	if code := call(t, "POST", srv.URL+"/api/v1/schematics", `{"customization":{"extensions":["qemu-guest-agent","node-exporter"]}}`, &v); code != 201 {
		t.Fatalf("create: %d", code)
	}
	if v.Default || len(v.Extensions) != 2 || !strings.Contains(v.YAML, "- node-exporter") {
		t.Errorf("view %+v", v)
	}
	var got schematicView
	call(t, "GET", srv.URL+"/api/v1/schematics/"+v.ID, "", &got)
	if got.ID != v.ID {
		t.Errorf("get: %+v", got)
	}
	var e map[string]string
	if code := call(t, "POST", srv.URL+"/api/v1/schematics", `{"customization":{"extensions":["bird"]}}`, &e); code != 400 {
		t.Errorf("an extension no release offers: %d %v", code, e)
	}

	// Not built yet; the old release doesn't do custom schematics; arm64
	// lacks the guest agent.
	var st imageStatus
	call(t, "GET", srv.URL+"/api/v1/images/"+v.ID+"/v2026.10.02/amd64", "", &st)
	if st.State != "none" {
		t.Errorf("status %+v", st)
	}
	call(t, "GET", srv.URL+"/api/v1/images/"+v.ID+"/v2026.09.30-3/amd64", "", &st)
	if st.State != "unavailable" {
		t.Errorf("old release: %+v", st)
	}
	call(t, "GET", srv.URL+"/api/v1/images/"+v.ID+"/v2026.10.02/arm64", "", &st)
	if st.State != "unavailable" || !strings.Contains(st.Message, "qemu-guest-agent") {
		t.Errorf("arm64: %+v", st)
	}

	// Build: dispatched once, then "building" without a second dispatch.
	if code := call(t, "POST", srv.URL+"/api/v1/images/"+v.ID+"/v2026.10.02/amd64", "", &st); code != 202 || st.State != "building" {
		t.Fatalf("build: %d %+v", code, st)
	}
	call(t, "POST", srv.URL+"/api/v1/images/"+v.ID+"/v2026.10.02/amd64", "", &st)
	if len(gh.dispatches) != 1 || gh.dispatches[0]["schematic_id"] != v.ID || gh.dispatches[0]["version"] != "v2026.10.02" {
		t.Fatalf("dispatches %+v", gh.dispatches)
	}
	if sc, err := schematic.Parse([]byte(gh.dispatches[0]["schematic"])); err != nil || sc.ID() != v.ID {
		t.Errorf("dispatched schematic %q", gh.dispatches[0]["schematic"])
	}

	// A failed run shows, and can be retried.
	gh.mu.Lock()
	gh.runs = []map[string]any{{"display_title": runName(v.ID, "v2026.10.02", "amd64"), "status": "completed", "conclusion": "failure", "html_url": "https://run"}}
	gh.mu.Unlock()
	call(t, "GET", srv.URL+"/api/v1/images/"+v.ID+"/v2026.10.02/amd64", "", &st)
	if st.State != "failed" || st.RunURL != "https://run" {
		t.Errorf("failed run: %+v", st)
	}
	call(t, "POST", srv.URL+"/api/v1/images/"+v.ID+"/v2026.10.02/amd64", "", &st)
	if len(gh.dispatches) != 2 {
		t.Errorf("retry didn't dispatch: %d", len(gh.dispatches))
	}

	// The build uploads its files and manifest: ready, served.
	dir := a.store.ImageDir(v.ID, "v2026.10.02", "amd64")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "janus.qcow2"), []byte("QFI\xfb disk"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "rootfs.squashfs"), []byte("hsqs"), 0o644)
	m, _ := json.Marshal(manifest{Schematic: v.ID, Version: "v2026.10.02", Arch: "amd64",
		Files: []manifestFile{{Name: "janus.qcow2", Size: 9, SHA256: "aa"}, {Name: "rootfs.squashfs", Size: 4, SHA256: strings.Repeat("d", 64)}}})
	_ = os.WriteFile(filepath.Join(dir, "manifest.json"), m, 0o644)
	call(t, "GET", srv.URL+"/api/v1/images/"+v.ID+"/v2026.10.02/amd64", "", &st)
	if st.State != "ready" || len(st.Files) != 2 || st.Files[0].URL != "https://site.test/image/"+v.ID+"/v2026.10.02/amd64/janus.qcow2" {
		t.Fatalf("ready: %+v", st)
	}
	resp, err := http.Get(srv.URL + "/image/" + v.ID + "/v2026.10.02/amd64/janus.qcow2")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "QFI\xfb disk" || !strings.Contains(resp.Header.Get("Content-Disposition"), "janus-v2026.10.02-") {
		t.Errorf("download: %d %q %q", resp.StatusCode, body, resp.Header.Get("Content-Disposition"))
	}
	for _, bad := range []string{"../../../etc/passwd", "janus.iso", "pi4-disk.img"} {
		resp, _ := http.Get(srv.URL + "/image/" + v.ID + "/v2026.10.02/amd64/" + bad)
		if resp.StatusCode != 404 {
			t.Errorf("%s: %d", bad, resp.StatusCode)
		}
		resp.Body.Close()
	}

	// Updates for this schematic: the newest release offering it, ready.
	var up update
	call(t, "GET", srv.URL+"/api/v1/updates/"+v.ID+"?from=v2026.09.30-3", "", &up)
	if up.State != "ready" || up.Version != "v2026.10.02" || up.BundleURL != "https://site.test/image/"+v.ID+"/v2026.10.02/amd64" || up.SHA256 != strings.Repeat("d", 64) || up.UpToDate {
		t.Errorf("update %+v", up)
	}
}

func TestDefaultSchematic(t *testing.T) {
	_, gh, srv := newTestApp(t)
	id := schematic.DefaultID()
	var st imageStatus
	call(t, "GET", srv.URL+"/api/v1/images/"+id+"/v2026.09.30-3/amd64", "", &st)
	if st.State != "ready" || len(st.Files) != 2 {
		t.Errorf("default images come from the release: %+v", st)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(srv.URL + "/image/" + id + "/v2026.09.30-3/amd64/janus.iso")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 302 || !strings.HasSuffix(resp.Header.Get("Location"), "/dl/v2026.09.30-3/janus.iso") {
		t.Errorf("redirect: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	var up update
	call(t, "GET", srv.URL+"/api/v1/updates/"+id+"?from=v2026.10.02", "", &up)
	if up.Version != "v2026.10.02" || !up.UpToDate || up.SHA256 != strings.Repeat("c", 64) || !strings.HasSuffix(up.BundleURL, "/dl/v2026.10.02") {
		t.Errorf("default update %+v", up)
	}
	if len(gh.dispatches) != 0 {
		t.Error("the default schematic never needs a build")
	}
}

func TestUpdateTriggersBuild(t *testing.T) {
	a, gh, srv := newTestApp(t)
	sc, _ := schematic.Parse([]byte(`{"customization":{"extensions":["node-exporter"]}}`))
	id, _ := a.store.PutSchematic(sc)
	var up update
	call(t, "GET", srv.URL+"/api/v1/updates/"+id, "", &up)
	if up.State != "building" || up.BundleURL != "" || len(gh.dispatches) != 1 {
		t.Errorf("update %+v, %d dispatches", up, len(gh.dispatches))
	}
}

func TestBuildLimits(t *testing.T) {
	b := newBuildLimiter()
	b.perIP, b.global = 2, 3
	for i, want := range []bool{true, true, false} {
		start, err := b.allow("k"+string(rune('a'+i)), "1.1.1.1", false)
		if start != want || (err != nil) == want {
			t.Errorf("request %d: start=%v err=%v", i, start, err)
		}
	}
	if start, err := b.allow("ka", "1.1.1.1", false); start || err != nil {
		t.Errorf("a pending build requested again: %v %v", start, err)
	}
	if start, _ := b.allow("kz", "2.2.2.2", false); !start {
		t.Error("another client refused under the global limit")
	}
	if _, err := b.allow("ky", "3.3.3.3", false); err == nil {
		t.Error("the global limit didn't apply")
	}
}

func TestCompareVersions(t *testing.T) {
	ordered := []string{"v2026.09.29", "v2026.09.30", "v2026.09.30-2", "v2026.09.30-10", "v2026.10.01"}
	for i := 1; i < len(ordered); i++ {
		if compareVersions(ordered[i-1], ordered[i]) >= 0 {
			t.Errorf("%s should sort before %s", ordered[i-1], ordered[i])
		}
	}
}

func TestPrune(t *testing.T) {
	st, _ := newStore(t.TempDir())
	id := strings.Repeat("e", 64)
	for _, v := range []string{"v2026.09.29", "v2026.09.30-2", "v2026.09.30-10", "v2026.10.01"} {
		_ = os.MkdirAll(st.ImageDir(id, v, "amd64"), 0o755)
	}
	if err := st.Prune(2, compareVersions); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Join(st.dir, "images", id))
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if strings.Join(got, ",") != "v2026.09.30-10,v2026.10.01" {
		t.Errorf("kept %v", got)
	}
}

// renamedCatalog is v2026.10.02 offering node-exporter under a new name.
const renamedCatalog = `{"version":"v2026.10.02","extensions":[
	{"name":"prometheus-node-exporter","version":"1.12.1","description":"metrics","arches":["amd64","arm64"],"replaces":["node-exporter"]},
	{"name":"qemu-guest-agent","version":"11.1.2","description":"agent","arches":["amd64"]}]}`

func TestUpdateMigratesRenamedExtensions(t *testing.T) {
	a, gh, srv := newTestApp(t)
	gh.catalog = renamedCatalog
	old, _ := schematic.Parse([]byte(`{"customization":{"extensions":["node-exporter","qemu-guest-agent"]}}`))
	oldID, _ := a.store.PutSchematic(old)
	want, _ := schematic.Parse([]byte(`{"customization":{"extensions":["prometheus-node-exporter","qemu-guest-agent"]}}`))

	var up update
	if code := call(t, "GET", srv.URL+"/api/v1/updates/"+oldID+"?from=v2026.10.02", "", &up); code != http.StatusOK {
		t.Fatalf("updates: %d", code)
	}
	if up.Schematic != want.ID() || up.Renamed["node-exporter"] != "prometheus-node-exporter" || up.Version != "v2026.10.02" {
		t.Fatalf("update = %+v", up)
	}
	if up.UpToDate {
		t.Error("a rename isn't up to date, even on the same release")
	}
	if up.State != "building" || len(gh.dispatches) != 1 || !strings.Contains(gh.dispatches[0]["schematic"], "prometheus-node-exporter") {
		t.Fatalf("update %+v, dispatches %v", up, gh.dispatches)
	}
	if _, err := a.store.Schematic(want.ID()); err != nil {
		t.Fatalf("the migrated schematic isn't stored: %v", err)
	}
}

func TestPrebuildMigratesRenamedExtensions(t *testing.T) {
	a, gh, _ := newTestApp(t)
	gh.catalog = renamedCatalog
	old, _ := schematic.Parse([]byte(`{"customization":{"extensions":["node-exporter"]}}`))
	oldID, _ := a.store.PutSchematic(old)
	// In use: an image of it was built for an older release.
	dir := filepath.Join(a.store.dir, "images", oldID, "v2026.10.01-2")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	a.prebuild(t.Context())
	if len(gh.dispatches) != 1 || !strings.Contains(gh.dispatches[0]["schematic"], `"prometheus-node-exporter"`) {
		t.Fatalf("dispatches = %v", gh.dispatches)
	}
}
