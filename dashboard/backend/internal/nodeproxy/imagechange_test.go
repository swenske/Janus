package nodeproxy

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/swenske/Janus/dashboard/backend/internal/store"
	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/schematic"
)

// fakeVersion plays a node's Version.
type fakeVersion struct {
	janusv1alpha1.UnimplementedSystemServiceServer
	v *janusv1alpha1.VersionResponse
}

func (f *fakeVersion) Version(context.Context, *emptypb.Empty) (*janusv1alpha1.VersionResponse, error) {
	return f.v, nil
}

// versionNode is a node whose janusd answers Version with v, for the
// routes that ask it.
func versionNode(t *testing.T, v *janusv1alpha1.VersionResponse) *store.Node {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	janusv1alpha1.RegisterSystemServiceServer(srv, &fakeVersion{v: v})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	node := &store.Node{ID: "version-" + t.Name() + "-" + v.GetVersion()}
	conns.Store(node.ID, conn)
	t.Cleanup(func() { closeNodeConn(node.ID) })
	return node
}

// fakeSite answers like site/backend: the release v2026.10.04 and the
// pre-releases v2026.10.04-3 and v2026.10.05, each with its own catalog
// (only-pre and bird-exporter are new in v2026.10.05); an update is the
// release, never a pre-release (GET /api/v1/updates); an image is built
// once asked (POST), ready at the next look.
type fakeSite struct {
	*httptest.Server
	mu      sync.Mutex
	images  map[string]string // "<schematic>/<version>" -> state
	builds  []string          // the versions built, in order
	updates int               // GET /api/v1/updates calls
}

const imageSHA = "1111111111111111111111111111111111111111111111111111111111111111"

func newFakeSite(t *testing.T) *fakeSite {
	t.Helper()
	f := &fakeSite{images: map[string]string{}}
	catalog := func(version string) string {
		exts := `{"name":"bird","version":"2.19.2","description":"BGP","arches":["amd64"]},{"name":"node-exporter","version":"1.9.1","description":"metrics","arches":["amd64"]}`
		if version == "v2026.10.05" {
			exts += `,{"name":"bird-exporter","version":"1.4.3","description":"BGP metrics","arches":["amd64"],"requires":["bird"]},{"name":"only-pre","version":"0.1","description":"new","arches":["amd64"]}`
		}
		return `{"version":"` + version + `","extensions":[` + exts + `],"haproxy":[{"name":"3.4","version":"3.4.7","default":true,"arches":["amd64"]}],"kernel":[{"name":"stable","version":"7.2.12","default":true,"arches":["amd64"]}]}`
	}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		path := r.URL.Path
		switch {
		case r.Method == http.MethodPost && path == "/api/v1/schematics":
			data, _ := io.ReadAll(r.Body)
			sc, err := schematic.Parse(data)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": sc.ID()})
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/api/v1/updates/"):
			f.updates++
			id := strings.TrimPrefix(path, "/api/v1/updates/")
			_, _ = io.WriteString(w, `{"schematic":"`+id+`","version":"v2026.10.04","release_url":"https://github.invalid/r/releases/tag/v2026.10.04",
				"bundle_url":"https://factory.invalid/image/`+id+`/v2026.10.04/amd64","sha256":"`+imageSHA+`","state":"ready"}`)
		case path == "/api/v1/versions":
			_, _ = io.WriteString(w, `[{"version":"v2026.10.05","schematics":true,"prerelease":true},
				{"version":"v2026.10.04-3","schematics":true,"prerelease":true},{"version":"v2026.10.04","schematics":true}]`)
		case strings.HasPrefix(path, "/api/v1/versions/") && strings.HasSuffix(path, "/extensions"):
			_, _ = io.WriteString(w, catalog(strings.TrimSuffix(strings.TrimPrefix(path, "/api/v1/versions/"), "/extensions")))
		case strings.HasPrefix(path, "/api/v1/images/") && strings.HasSuffix(path, "/amd64"):
			key := strings.TrimSuffix(strings.TrimPrefix(path, "/api/v1/images/"), "/amd64")
			state := f.images[key]
			switch {
			case r.Method == http.MethodPost && state == "":
				f.builds = append(f.builds, key[strings.Index(key, "/")+1:])
				f.images[key] = "building"
				state = "building"
			case state == "":
				state = "none"
			case state == "building": // ready at the next look
				f.images[key] = "ready"
				state = "ready"
			}
			if state != "ready" {
				_, _ = io.WriteString(w, `{"state":"`+state+`"}`)
				return
			}
			_, _ = io.WriteString(w, `{"state":"ready","files":[{"name":"rootfs.squashfs","url":"https://factory.invalid/image/`+key+`/amd64/rootfs.squashfs","size":3,"sha256":"`+imageSHA+`"}]}`)
		default:
			t.Errorf("the image factory was asked %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeSite) counts() (builds []string, updates int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.builds...), f.updates
}

// releasesWithPrereleases is GitHub's list: the release v2026.10.04, an
// older pre-release v2026.10.03-2 (never promoted), and two newer ones,
// v2026.10.04-3 and v2026.10.05.
func releasesWithPrereleases(t *testing.T) {
	t.Helper()
	fakeReleases(t, new(atomic.Int32), func(base string) string {
		rel := func(tag string, pre bool) string {
			p := "false"
			if pre {
				p = "true"
			}
			return `{"tag_name":"` + tag + `","html_url":"https://github.invalid/r/releases/tag/` + tag + `","published_at":"2026-10-05T08:00:00Z","prerelease":` + p +
				`,"assets":[{"name":"rootfs.squashfs.sha256","browser_download_url":"` + base + `/sha256"}]}`
		}
		return "[" + strings.Join([]string{rel("v2026.10.05", true), rel("v2026.10.04-3", true), rel("v2026.10.04", false), rel("v2026.10.03-2", true)}, ",") + "]"
	})
	seedRelease(t, nil) // the list fetched from the fake, the cache restored after
}

func birdImage(t *testing.T, exts ...string) *schematic.Schematic {
	t.Helper()
	sc := &schematic.Schematic{Customization: schematic.Customization{Extensions: exts}}
	if err := sc.Normalize(); err != nil {
		t.Fatal(err)
	}
	return sc
}

// TestPrereleaseTrack: a node running a pre-release stays on the
// pre-releases - its own while it's the newest, else the newest; any
// other node takes the release.
func TestPrereleaseTrack(t *testing.T) {
	rel := &ReleaseInfo{TagName: "v2026.10.04", tags: []string{"v2026.10.04"},
		prereleases: map[string]bool{"v2026.10.05": true, "v2026.10.04-3": true, "v2026.10.03-2": true},
		Prerelease:  &ReleaseInfo{TagName: "v2026.10.05"}}
	for v, want := range map[string]string{
		"v2026.10.05":            "v2026.10.05", // the pre-release it runs
		"v2026.10.04-3":          "v2026.10.05", // a newer one
		"v2026.10.03-2":          "v2026.10.05", // older than the release, a newer pre-release out
		"v2026.10.04":            "",            // the release
		"v2026.10.03":            "",            // an older release
		"v2026.10.05-1-g0123abc": "",            // a development build
		"dev":                    "",
	} {
		got := ""
		if p := rel.PrereleaseTrack(v); p != nil {
			got = p.TagName
		}
		if got != want {
			t.Errorf("PrereleaseTrack(%s) = %q, want %q", v, got, want)
		}
	}
	// No pre-release newer than the release: one a release came after
	// takes the release.
	rel.Prerelease = nil
	if p := rel.PrereleaseTrack("v2026.10.03-2"); p != nil {
		t.Errorf("PrereleaseTrack(v2026.10.03-2) with no newer pre-release = %s", p.TagName)
	}
}

// TestImageChangeKeepsThePrerelease (#405): changing the image of a node
// running a pre-release builds that pre-release - or a newer one - with
// the new schematic, from the catalog of that version; a node running the
// release still gets the release, its catalog the release's. The update
// check doesn't move.
func TestImageChangeKeepsThePrerelease(t *testing.T) {
	site := newFakeSite(t)
	resetFactory(t, site.URL)
	resetCatalog(t)
	releasesWithPrereleases(t)
	ctx := context.Background()
	bird := birdImage(t, "bird")

	for i, tc := range []struct {
		node, want string
		pre        bool
	}{
		{"v2026.10.05", "v2026.10.05", true},   // the pre-release it runs
		{"v2026.10.04-3", "v2026.10.05", true}, // a newer pre-release than the node's
		{"v2026.10.04", "v2026.10.04", false},  // a node on the release: the release
	} {
		v := nodeWith("node-exporter")
		v.Version = tc.node
		eu := updateFor(ctx, v, bird)
		if i == 0 {
			// The factory starts the pre-release's build; the page asks
			// again until it's ready.
			if eu.State != "building" || eu.Latest != tc.want || eu.BundleURL != "" || !eu.TargetPrerelease {
				t.Fatalf("on %s, first: %+v", tc.node, eu)
			}
			eu = updateFor(ctx, v, bird)
		}
		if eu.State != "ready" || eu.Latest != tc.want || eu.TargetPrerelease != tc.pre || eu.Source != "image-factory" || !eu.SchematicChange ||
			eu.BundleURL != "https://factory.invalid/image/"+bird.ID()+"/"+tc.want+"/amd64" || eu.SHA256 != imageSHA ||
			eu.SchematicID != bird.ID() || eu.Version != tc.node || eu.NodeSchematicID != v.SchematicId {
			t.Fatalf("on %s: %+v", tc.node, eu)
		}
		if newer := tc.node != tc.want; eu.UpdateAvailable != newer {
			t.Errorf("on %s: update_available %v", tc.node, eu.UpdateAvailable)
		}
		// The catalog shown is the one of the version built.
		cat, err := imageCatalog(ctx, tc.node)
		if err != nil || cat.Version != eu.Latest || cat.Prerelease != tc.pre {
			t.Fatalf("on %s: catalog %+v, %v - the build is %s", tc.node, cat, err, eu.Latest)
		}
		if _, only := (&schematic.Catalog{Extensions: cat.Extensions}).Lookup("only-pre"); only != tc.pre {
			t.Errorf("on %s: only-pre offered: %v", tc.node, only)
		}
	}
	builds, updates := site.counts()
	if strings.Join(builds, " ") != "v2026.10.05" || updates != 1 {
		t.Errorf("builds %q, update asked %d times - want the pre-release built once, the release's update asked once", builds, updates)
	}
	// The cached catalogs aren't marked: the newest release's is v2026.10.04's.
	if cat, _ := factoryCatalog(ctx); cat.Version != "v2026.10.04" || cat.Prerelease {
		t.Errorf("the newest release's catalog: %+v", cat)
	}

	// Back to the default image on the pre-release: its GitHub release.
	v := nodeWith("bird")
	v.Version = "v2026.10.05"
	eu := updateFor(ctx, v, &schematic.Schematic{})
	if eu.State != "ready" || eu.Source != "github" || eu.Latest != "v2026.10.05" || !eu.TargetPrerelease || !eu.Default || !eu.SchematicChange ||
		eu.BundleURL != "https://github.invalid/r/releases/download/v2026.10.05" || eu.SHA256 != strings.Repeat("ab", 32) {
		t.Fatalf("the default image on the pre-release: %+v", eu)
	}

	// Nothing else moves: the update check of that node is still the
	// release's (older: no update), no pre-release offered, no badge.
	uc := checkUpdate(ctx, v)
	if uc.Latest != "v2026.10.04" || uc.UpdateAvailable || !uc.RunningPrerelease || uc.Prerelease != nil || uc.SecurityUpdate != "" {
		t.Errorf("update check on the pre-release: %+v", uc)
	}
	// And on the release, the pre-release is still only offered apart.
	v.Version = "v2026.10.04"
	uc = checkUpdate(ctx, v)
	if uc.Latest != "v2026.10.04" || uc.UpdateAvailable || uc.RunningPrerelease || uc.Prerelease == nil || uc.Prerelease.Version != "v2026.10.05" {
		t.Errorf("update check on the release: %+v", uc)
	}
}

// TestImageChangeRoutes: the node page's catalog and preparation follow
// the node's version; an extension's requirement is checked against the
// catalog of the version built.
func TestImageChangeRoutes(t *testing.T) {
	site := newFakeSite(t)
	resetFactory(t, site.URL)
	resetCatalog(t)
	releasesWithPrereleases(t)

	call := func(node *store.Node, method, path, body string) (int, map[string]any, string) {
		t.Helper()
		mux := http.NewServeMux()
		registerUpdateRoutes(mux, node)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out, rec.Body.String()
	}
	onPre := nodeWith("node-exporter")
	onPre.Version = "v2026.10.05"
	pre := versionNode(t, onPre)
	onRel := nodeWith("node-exporter")
	onRel.Version = "v2026.10.04"
	rel := versionNode(t, onRel)

	if code, cat, body := call(pre, http.MethodGet, "/api/factory/catalog", ""); code != http.StatusOK || cat["version"] != "v2026.10.05" || cat["prerelease"] != true {
		t.Fatalf("catalog on the pre-release: %d %s", code, body)
	}
	if code, cat, body := call(rel, http.MethodGet, "/api/factory/catalog", ""); code != http.StatusOK || cat["version"] != "v2026.10.04" || cat["prerelease"] != nil {
		t.Fatalf("catalog on the release: %d %s", code, body)
	}

	code, eu, body := call(pre, http.MethodPost, "/api/factory/update", `{"extensions":["bird","node-exporter"]}`)
	if code != http.StatusOK || eu["latest"] != "v2026.10.05" || eu["target_prerelease"] != true || eu["state"] != "building" || eu["schematic_change"] != true {
		t.Fatalf("prepared on the pre-release: %d %s", code, body)
	}
	code, eu, body = call(rel, http.MethodPost, "/api/factory/update", `{"extensions":["bird","node-exporter"]}`)
	if code != http.StatusOK || eu["latest"] != "v2026.10.04" || eu["target_prerelease"] != nil || eu["state"] != "ready" {
		t.Fatalf("prepared on the release: %d %s", code, body)
	}

	// bird-exporter requires bird in the pre-release's catalog; the
	// release's doesn't know it (left to the factory).
	want := `extension "bird-exporter" requires the "bird" extension: choose it too`
	if code, _, body := call(pre, http.MethodPost, "/api/factory/update", `{"extensions":["bird-exporter"]}`); code != http.StatusBadRequest || strings.TrimSpace(body) != want {
		t.Fatalf("bird-exporter alone on the pre-release: %d %s", code, body)
	}
	if builds, _ := site.counts(); strings.Join(builds, " ") != "v2026.10.05" {
		t.Errorf("builds %q: a refused image was built", builds)
	}
}
