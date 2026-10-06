package nodeproxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/store"
	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/schematic"
)

// fakeFactory answers like site/backend: schematics are registered by
// content, updates are ready for "v2" once asked for.
func fakeFactory(t *testing.T, updateCalls *atomic.Int32, updateStatus int, updateBody string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/schematics":
			data, _ := io.ReadAll(r.Body)
			sc, err := schematic.Parse(data)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": sc.ID(), "extensions": sc.Extensions()})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/updates/"):
			updateCalls.Add(1)
			if r.URL.Query().Get("arch") != "amd64" || r.URL.Query().Get("from") != "v1" {
				t.Errorf("updates query = %s", r.URL.RawQuery)
			}
			w.WriteHeader(updateStatus)
			_, _ = io.WriteString(w, updateBody)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func nodeWith(exts ...string) *janusv1alpha1.VersionResponse {
	sc := &schematic.Schematic{Customization: schematic.Customization{Extensions: exts}}
	if err := sc.Normalize(); err != nil {
		panic(err)
	}
	v := &janusv1alpha1.VersionResponse{Version: "v1", Arch: "amd64", SchematicId: sc.ID()}
	for _, e := range exts {
		v.Extensions = append(v.Extensions, &janusv1alpha1.ExtensionInfo{Name: e})
	}
	return v
}

func resetFactory(t *testing.T, url string) {
	t.Helper()
	prev := ImageFactoryURL
	ImageFactoryURL = url
	factoryCache.mu.Lock()
	factoryCache.entries = nil
	factoryCache.mu.Unlock()
	t.Cleanup(func() { ImageFactoryURL = prev })
}

func TestCheckUpdateFromFactory(t *testing.T) {
	var calls atomic.Int32
	node := nodeWith("node-exporter")
	body := `{"schematic":"` + node.SchematicId + `","version":"v2","release_url":"https://example.invalid/r/v2","bundle_url":"https://factory.invalid/image/x/v2/amd64","sha256":"abc","state":"ready","up_to_date":false}`
	srv := fakeFactory(t, &calls, http.StatusOK, body)
	resetFactory(t, srv.URL)

	uc := checkUpdate(context.Background(), node)
	if uc.Source != "image-factory" || uc.State != "ready" || uc.Latest != "v2" || !uc.UpdateAvailable {
		t.Fatalf("update check = %+v", uc)
	}
	if uc.BundleURL != "https://factory.invalid/image/x/v2/amd64" || uc.SHA256 != "abc" {
		t.Fatalf("bundle = %q %q", uc.BundleURL, uc.SHA256)
	}
	if len(uc.Extensions) != 1 || uc.Extensions[0] != "node-exporter" || uc.Default {
		t.Fatalf("node = %+v", uc)
	}

	// Cached: a ready answer isn't asked for again right away.
	checkUpdate(context.Background(), node)
	if n := calls.Load(); n != 1 {
		t.Fatalf("the factory was asked %d times, want 1", n)
	}
}

func TestCheckUpdateFactoryError(t *testing.T) {
	var calls atomic.Int32
	srv := fakeFactory(t, &calls, http.StatusNotFound, `{"error":"no release offers this schematic's extensions"}`)
	resetFactory(t, srv.URL)

	uc := checkUpdate(context.Background(), nodeWith("qemu-guest-agent"))
	if uc.State != "unavailable" || !strings.Contains(uc.Message, "no release offers") {
		t.Fatalf("update check = %+v", uc)
	}
}

func TestCheckUpdateMismatchedSchematic(t *testing.T) {
	var calls atomic.Int32
	srv := fakeFactory(t, &calls, http.StatusOK, `{}`)
	resetFactory(t, srv.URL)

	node := nodeWith("node-exporter")
	node.Extensions = append(node.Extensions, &janusv1alpha1.ExtensionInfo{Name: "qemu-guest-agent"})
	uc := checkUpdate(context.Background(), node)
	if uc.State != "unavailable" || !strings.Contains(uc.Message, "don't make up its schematic") || calls.Load() != 0 {
		t.Fatalf("update check = %+v (factory calls %d)", uc, calls.Load())
	}
}

func TestCheckUpdateNoFactory(t *testing.T) {
	resetFactory(t, "")
	uc := checkUpdate(context.Background(), nodeWith("node-exporter"))
	if uc.State != "unavailable" || !strings.Contains(uc.Message, "-image-factory") {
		t.Fatalf("update check = %+v", uc)
	}
}

// seedRelease makes rel the latest GitHub release without asking GitHub.
func seedRelease(t *testing.T, rel *ReleaseInfo) {
	t.Helper()
	releaseCache.mu.Lock()
	prevData, prevErr, prevAt := releaseCache.data, releaseCache.err, releaseCache.fetchedAt
	releaseCache.data, releaseCache.err, releaseCache.fetchedAt = rel, nil, time.Now()
	releaseCache.mu.Unlock()
	t.Cleanup(func() {
		releaseCache.mu.Lock()
		releaseCache.data, releaseCache.err, releaseCache.fetchedAt = prevData, prevErr, prevAt
		releaseCache.mu.Unlock()
	})
}

func TestCheckUpdateDefaultSchematicUsesGitHub(t *testing.T) {
	resetFactory(t, "http://127.0.0.1:1") // must not be used
	seedRelease(t, &ReleaseInfo{TagName: "v1", BundleBaseURL: "https://github.invalid/download/v1", SHA256: "def"})

	// A node older than schematics and VersionResponse.arch.
	uc := checkUpdate(context.Background(), &janusv1alpha1.VersionResponse{Version: "v1"})
	if uc.Source != "github" || uc.State != "ready" || uc.UpdateAvailable || !uc.Default || uc.Arch != "amd64" {
		t.Fatalf("update check = %+v", uc)
	}
	if uc.SchematicID != schematic.DefaultID() || uc.BundleURL != "https://github.invalid/download/v1" {
		t.Fatalf("update check = %+v", uc)
	}
}

func TestUpdateForOtherExtensions(t *testing.T) {
	var calls atomic.Int32
	target := &schematic.Schematic{Customization: schematic.Customization{Extensions: []string{"qemu-guest-agent", "node-exporter"}}}
	if err := target.Normalize(); err != nil {
		t.Fatal(err)
	}
	body := `{"schematic":"` + target.ID() + `","version":"v2","release_url":"https://example.invalid/r/v2","bundle_url":"https://factory.invalid/image/t/v2/amd64","sha256":"abc","state":"ready"}`
	srv := fakeFactory(t, &calls, http.StatusOK, body)
	resetFactory(t, srv.URL)

	eu := updateFor(context.Background(), nodeWith(), target)
	if !eu.SchematicChange || eu.Default || eu.SchematicID != target.ID() || eu.NodeSchematicID != schematic.DefaultID() {
		t.Fatalf("update = %+v", eu)
	}
	if strings.Join(eu.Extensions, ",") != "node-exporter,qemu-guest-agent" || len(eu.NodeExtensions) != 0 {
		t.Fatalf("extensions = %v, node's %v", eu.Extensions, eu.NodeExtensions)
	}
	if eu.Source != "image-factory" || eu.State != "ready" || eu.BundleURL != "https://factory.invalid/image/t/v2/amd64" || eu.SHA256 != "abc" || eu.Version != "v1" {
		t.Fatalf("update = %+v", eu)
	}
	// The embedded check's fields come out at the top level.
	data, _ := json.Marshal(eu)
	var flat map[string]any
	if err := json.Unmarshal(data, &flat); err != nil || flat["bundle_base_url"] != eu.BundleURL || flat["schematic_change"] != true || flat["node_schematic_id"] != schematic.DefaultID() {
		t.Fatalf("JSON = %s", data)
	}
}

func TestUpdateForBuilding(t *testing.T) {
	var calls atomic.Int32
	srv := fakeFactory(t, &calls, http.StatusOK, `{"version":"v2","state":"building"}`)
	resetFactory(t, srv.URL)
	target := &schematic.Schematic{Customization: schematic.Customization{Extensions: []string{"bird"}}}
	eu := updateFor(context.Background(), nodeWith("node-exporter"), target)
	if eu.State != "building" || eu.BundleURL != "" || !eu.SchematicChange {
		t.Fatalf("update = %+v", eu)
	}
}

func TestUpdateForSameSchematic(t *testing.T) {
	var calls atomic.Int32
	srv := fakeFactory(t, &calls, http.StatusOK, `{"version":"v2","bundle_url":"https://factory.invalid/b","state":"ready"}`)
	resetFactory(t, srv.URL)
	target := &schematic.Schematic{Customization: schematic.Customization{Extensions: []string{"node-exporter"}}}
	eu := updateFor(context.Background(), nodeWith("node-exporter"), target)
	if eu.SchematicChange || eu.State != "ready" || !eu.UpdateAvailable {
		t.Fatalf("update = %+v", eu)
	}
}

func TestUpdateForNoExtensionsUsesGitHub(t *testing.T) {
	resetFactory(t, "http://127.0.0.1:1") // must not be used
	seedRelease(t, &ReleaseInfo{TagName: "v2", BundleBaseURL: "https://github.invalid/download/v2", SHA256: "def"})
	eu := updateFor(context.Background(), nodeWith("node-exporter", "qemu-guest-agent"), &schematic.Schematic{})
	if !eu.Default || !eu.SchematicChange || eu.Source != "github" || eu.BundleURL != "https://github.invalid/download/v2" || len(eu.Extensions) != 0 {
		t.Fatalf("update = %+v", eu)
	}
	if strings.Join(eu.NodeExtensions, ",") != "node-exporter,qemu-guest-agent" {
		t.Fatalf("node's extensions = %v", eu.NodeExtensions)
	}
}

// fakeCatalogFactory serves the versions list and the catalogs of
// site/backend, counting the requests.
func fakeCatalogFactory(t *testing.T, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.URL.Path {
		case "/api/v1/versions":
			_, _ = io.WriteString(w, `[{"version":"v3","schematics":false},{"version":"v2","schematics":true},{"version":"v1","schematics":true}]`)
		case "/api/v1/versions/v2/extensions":
			_, _ = io.WriteString(w, `{"version":"v2","extensions":[{"name":"node-exporter","version":"1.9.1","description":"Prometheus exporter","arches":["amd64","arm64"]},{"name":"qemu-guest-agent","version":"10.0.0","description":"QEMU guest agent","arches":["amd64"]}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func resetCatalog(t *testing.T) {
	t.Helper()
	catalogCache.mu.Lock()
	catalogCache.base, catalogCache.view, catalogCache.err = "", nil, nil
	catalogCache.mu.Unlock()
}

func TestFactoryCatalog(t *testing.T) {
	var calls atomic.Int32
	srv := fakeCatalogFactory(t, &calls)
	resetFactory(t, srv.URL)
	resetCatalog(t)

	view, err := factoryCatalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// v3 publishes no catalog (an older build pipeline): the newest one is v2's.
	if view.Version != "v2" || len(view.Extensions) != 2 || view.Extensions[1].Name != "qemu-guest-agent" || view.Extensions[1].Arches[0] != "amd64" || view.Factory != srv.URL {
		t.Fatalf("catalog = %+v", view)
	}
	if _, err := factoryCatalog(context.Background()); err != nil || calls.Load() != 2 {
		t.Fatalf("second call: %v, factory asked %d times, want 2 (cached)", err, calls.Load())
	}
}

func TestFactoryCatalogNoFactory(t *testing.T) {
	resetFactory(t, "")
	resetCatalog(t)
	if _, err := factoryCatalog(context.Background()); err == nil || !strings.Contains(err.Error(), "-image-factory") {
		t.Fatalf("err = %v", err)
	}
}

func TestFactoryRoutes(t *testing.T) {
	var calls atomic.Int32
	srv := fakeCatalogFactory(t, &calls)
	resetFactory(t, srv.URL)
	resetCatalog(t)
	mux := http.NewServeMux()
	registerUpdateRoutes(mux, &store.Node{})

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/factory/catalog", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"qemu-guest-agent"`) {
		t.Fatalf("catalog: %d %s", rec.Code, rec.Body)
	}

	// An invalid name is refused before the node or the factory is asked.
	before := calls.Load()
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/factory/update", strings.NewReader(`{"extensions":["Not Valid!"]}`)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "valid extension name") || calls.Load() != before {
		t.Fatalf("invalid name: %d %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/factory/update", strings.NewReader(`not json`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body: %d %s", rec.Code, rec.Body)
	}
}

// canceled is the context of a request whose browser already went away.
func canceled() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// A shared, cached fetch outlives the request that triggers it: a client
// going away mustn't leave "context canceled" in the cache for every node.
func TestCachedFetchesIgnoreCanceledRequests(t *testing.T) {
	t.Run("latest release", func(t *testing.T) {
		var gh *httptest.Server
		gh = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/releases":
				_, _ = io.WriteString(w, `[{"tag_name":"v2","html_url":"https://github.invalid/r/releases/tag/v2","assets":[{"name":"rootfs.squashfs.sha256","browser_download_url":"`+gh.URL+`/sha"}]}]`)
			case "/sha":
				_, _ = io.WriteString(w, "abc\n")
			default:
				http.NotFound(w, r)
			}
		}))
		t.Cleanup(gh.Close)
		prevURL := ReleasesURL
		ReleasesURL = gh.URL + "/releases"
		t.Cleanup(func() { ReleasesURL = prevURL })
		seedRelease(t, nil) // empty cache, restored afterwards
		releaseCache.mu.Lock()
		releaseCache.fetchedAt = time.Time{}
		releaseCache.mu.Unlock()

		rel, err := getLatestRelease(canceled())
		if err != nil || rel.TagName != "v2" || rel.SHA256 != "abc" {
			t.Fatalf("latest release = %+v, %v", rel, err)
		}
	})
	t.Run("factory catalog", func(t *testing.T) {
		var calls atomic.Int32
		resetFactory(t, fakeCatalogFactory(t, &calls).URL)
		resetCatalog(t)
		if view, err := factoryCatalog(canceled()); err != nil || view.Version != "v2" {
			t.Fatalf("catalog = %+v, %v", view, err)
		}
	})
	t.Run("factory update", func(t *testing.T) {
		var calls atomic.Int32
		srv := fakeFactory(t, &calls, http.StatusOK, `{"version":"v2","bundle_url":"https://factory.invalid/b","state":"ready"}`)
		resetFactory(t, srv.URL)
		if uc := checkUpdate(canceled(), nodeWith("node-exporter")); uc.State != "ready" {
			t.Fatalf("update check = %+v", uc)
		}
	})
}

func TestCheckUpdateRenamedExtension(t *testing.T) {
	var calls atomic.Int32
	renamed := &schematic.Schematic{Customization: schematic.Customization{Extensions: []string{"prometheus-node-exporter", "qemu-guest-agent"}}}
	if err := renamed.Normalize(); err != nil {
		t.Fatal(err)
	}
	// The same release, the extension renamed: still an update to install.
	body := `{"schematic":"` + renamed.ID() + `","version":"v1","bundle_url":"https://factory.invalid/image/r/v1/amd64","sha256":"abc","state":"ready","renamed":{"node-exporter":"prometheus-node-exporter"}}`
	srv := fakeFactory(t, &calls, http.StatusOK, body)
	resetFactory(t, srv.URL)

	uc := checkUpdate(context.Background(), nodeWith("node-exporter", "qemu-guest-agent"))
	if uc.TargetSchematicID != renamed.ID() || uc.Renamed["node-exporter"] != "prometheus-node-exporter" || !uc.UpdateAvailable {
		t.Fatalf("update check = %+v", uc)
	}
	if strings.Join(uc.TargetExtensions, ",") != "prometheus-node-exporter,qemu-guest-agent" || strings.Join(uc.Extensions, ",") != "node-exporter,qemu-guest-agent" {
		t.Fatalf("extensions %v -> %v", uc.Extensions, uc.TargetExtensions)
	}
}

// A failure to reach GitHub is answered from the cache only briefly: a
// retry soon after gets a fresh try (it used to wait ten minutes).
func TestReleaseErrorCachedBriefly(t *testing.T) {
	var calls atomic.Int32
	failing := atomic.Bool{}
	failing.Store(true)
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		if failing.Load() {
			http.Error(w, "slow down", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`[{"tag_name":"v2026.10.03-3","html_url":"https://github.invalid/r/releases/tag/v2026.10.03-3","assets":[]}]`))
	}))
	defer gh.Close()
	prevURL, prevTTL := ReleasesURL, errorCacheTTL
	ReleasesURL, errorCacheTTL = gh.URL, 200*time.Millisecond
	seedRelease(t, nil) // restores the cache afterwards
	releaseCache.mu.Lock()
	releaseCache.fetchedAt = time.Time{}
	releaseCache.mu.Unlock()
	t.Cleanup(func() { ReleasesURL, errorCacheTTL = prevURL, prevTTL })

	if _, err := getLatestRelease(context.Background()); err == nil {
		t.Fatal("no error from a failing GitHub")
	}
	failing.Store(false)
	if _, err := getLatestRelease(context.Background()); err == nil || calls.Load() != 1 {
		t.Fatalf("right after: %v, %d calls - want the cached error", err, calls.Load())
	}
	time.Sleep(250 * time.Millisecond)
	rel, err := getLatestRelease(context.Background())
	if err != nil || rel.TagName != "v2026.10.03-3" || calls.Load() != 2 {
		t.Fatalf("after errorCacheTTL: %+v %v, %d calls", rel, err, calls.Load())
	}
}

func TestUpdateAvailable(t *testing.T) {
	for _, tc := range []struct {
		latest, running string
		want            bool
	}{
		{"v2026.10.05-3", "v2026.10.05-2", true},
		{"v2026.10.05-3", "v2026.10.05-3", false},
		// The cached latest predates the release the node just installed.
		{"v2026.10.05-2", "v2026.10.05-3", false},
		// A build after a release isn't offered that release again.
		{"v2026.10.05-3", "v2026.10.05-3-4-g0123abc", false},
		{"v2026.10.06", "v2026.10.05-3-4-g0123abc-dirty", true},
		// Not CalVer: only "different" means anything.
		{"v2", "v1", true},
		{"v2026.10.05", "dev", true},
		{"", "v2026.10.05", false},
	} {
		if got := updateAvailable(tc.latest, tc.running); got != tc.want {
			t.Errorf("updateAvailable(%q, %q) = %v, want %v", tc.latest, tc.running, got, tc.want)
		}
	}
}

// A node whose image reports its schematic - a HAProxy branch, a kernel
// track - is checked for updates of that schematic, not of its
// extensions alone; changing its extensions keeps the branch.
func TestCheckUpdateVariantSchematic(t *testing.T) {
	sc, _ := schematic.Parse([]byte(`{"customization":{"extensions":["node-exporter"],"haproxy":"3.2"}}`))
	var posted atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/schematics":
			data, _ := io.ReadAll(r.Body)
			got, err := schematic.Parse(data)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			posted.Store(string(data))
			_ = json.NewEncoder(w).Encode(map[string]any{"id": got.ID()})
		case strings.HasPrefix(r.URL.Path, "/api/v1/updates/"):
			id := strings.TrimPrefix(r.URL.Path, "/api/v1/updates/")
			_, _ = io.WriteString(w, `{"schematic":"`+id+`","version":"v2","bundle_url":"https://factory.invalid/b","sha256":"abc","state":"ready"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	resetFactory(t, srv.URL)

	node := &janusv1alpha1.VersionResponse{Version: "v1", Arch: "amd64", SchematicId: sc.ID(), Schematic: string(sc.Canonical()),
		Extensions: []*janusv1alpha1.ExtensionInfo{{Name: "node-exporter"}}}
	uc := checkUpdate(context.Background(), node)
	if uc.State != "ready" || uc.HAProxy != "3.2" || posted.Load() != string(sc.Canonical()) {
		t.Fatalf("update check = %+v, posted %v", uc, posted.Load())
	}

	eu := updateFor(context.Background(), node, func() *schematic.Schematic {
		s := NodeSchematic(node).Clone()
		s.Customization.Extensions = nil
		return s
	}())
	want, _ := schematic.Parse([]byte(`{"customization":{"haproxy":"3.2"}}`))
	if eu.SchematicID != want.ID() || eu.HAProxy != "3.2" || !eu.SchematicChange || eu.Default {
		t.Fatalf("update = %+v", eu)
	}

	// A schematic that isn't the one the node booted is ignored.
	node.Schematic = `{"customization":{"haproxy":"3.0"}}`
	if got := NodeSchematic(node); got.HAProxyBranch() != "" || len(got.Extensions()) != 1 {
		t.Errorf("trusted a schematic that isn't the node's: %s", got.Canonical())
	}
}

// EndOfSupport: a node pinned to a branch learns when the releases stop
// offering it; a node taking the default never does.
func TestEndOfSupport(t *testing.T) {
	soon := time.Now().AddDate(0, 3, 0).Format(time.DateOnly)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/versions":
			_, _ = io.WriteString(w, `[{"version":"v3","schematics":true}]`)
		case "/api/v1/versions/v3/extensions":
			_, _ = io.WriteString(w, `{"version":"v3","extensions":[],
				"haproxy":[{"name":"3.4","version":"3.4.7","default":true,"eol":"2031-04-01","arches":["amd64"]},
				           {"name":"3.2","version":"3.2.26","eol":"`+soon+`","arches":["amd64"]}],
				"kernel":[{"name":"longterm","version":"6.18.56","default":true,"arches":["amd64"]}],
				"retired":[{"component":"haproxy","name":"3.0","last_release":"v2"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	resetFactory(t, srv.URL)
	catalogCache.mu.Lock()
	catalogCache.base = ""
	catalogCache.mu.Unlock()

	ctx := context.Background()
	if s := EndOfSupport(ctx, ""); s != nil {
		t.Errorf("the default branch: %+v", s)
	}
	if s := EndOfSupport(ctx, "3.4"); s == nil || s.Soon || s.EOL != "2031-04-01" {
		t.Errorf("3.4: %+v", s)
	}
	if s := EndOfSupport(ctx, "3.2"); s == nil || !s.Soon {
		t.Errorf("3.2: %+v", s)
	}
	if s := EndOfSupport(ctx, "3.0"); s == nil || !s.Retired || s.LastRelease != "v2" {
		t.Errorf("3.0: %+v", s)
	}
}
