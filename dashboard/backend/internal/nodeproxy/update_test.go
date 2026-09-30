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

func TestCheckUpdateDefaultSchematicUsesGitHub(t *testing.T) {
	resetFactory(t, "http://127.0.0.1:1") // must not be used
	releaseCache.mu.Lock()
	prevData, prevErr, prevAt := releaseCache.data, releaseCache.err, releaseCache.fetchedAt
	releaseCache.data = &latestReleaseInfo{TagName: "v1", BundleBaseURL: "https://github.invalid/download/v1", SHA256: "def"}
	releaseCache.err = nil
	releaseCache.fetchedAt = time.Now()
	releaseCache.mu.Unlock()
	t.Cleanup(func() {
		releaseCache.mu.Lock()
		releaseCache.data, releaseCache.err, releaseCache.fetchedAt = prevData, prevErr, prevAt
		releaseCache.mu.Unlock()
	})

	// A node older than schematics and VersionResponse.arch.
	uc := checkUpdate(context.Background(), &janusv1alpha1.VersionResponse{Version: "v1"})
	if uc.Source != "github" || uc.State != "ready" || uc.UpdateAvailable || !uc.Default || uc.Arch != "amd64" {
		t.Fatalf("update check = %+v", uc)
	}
	if uc.SchematicID != schematic.DefaultID() || uc.BundleURL != "https://github.invalid/download/v1" {
		t.Fatalf("update check = %+v", uc)
	}
}
