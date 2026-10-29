package nodeproxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/swenske/Janus/internal/schematic"
)

func TestMirrorFor(t *testing.T) {
	for _, c := range []struct{ factory, flag, want string }{
		{DefaultImageFactory, "auto", DefaultImageFactoryMirror},
		{DefaultImageFactory + "/", "auto", DefaultImageFactoryMirror},
		{"https://factory.example", "auto", ""}, // another factory: its own mirror, if any, is named
		{DefaultImageFactory, "", ""},
		{"https://factory.example", "https://mirror.example/", "https://mirror.example"},
	} {
		if got := MirrorFor(c.factory, c.flag); got != c.want {
			t.Errorf("MirrorFor(%q, %q) = %q, want %q", c.factory, c.flag, got, c.want)
		}
	}
}

// fakeMirror answers like the site in mirror mode: GETs only - it never
// registers a schematic nor builds one.
func fakeMirror(t *testing.T, calls *atomic.Int32, status int, updateBody, imageBody string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet {
			t.Errorf("the mirror got a %s %s", r.Method, r.URL.Path)
			http.Error(w, "read only", http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(status)
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v1/updates/"):
			_, _ = io.WriteString(w, updateBody)
		case strings.HasPrefix(r.URL.Path, "/api/v1/images/"):
			_, _ = io.WriteString(w, imageBody)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// resetMirror also seeds the release list: checkUpdate would fetch
// GitHub's, and keep real releases' security.json for the next tests.
func resetMirror(t *testing.T, url string) {
	t.Helper()
	seedRelease(t, &ReleaseInfo{TagName: "v2"})
	prev, prevTry := ImageFactoryMirrorURL, factoryTryTimeout
	ImageFactoryMirrorURL, factoryTryTimeout = url, 300*time.Millisecond
	t.Cleanup(func() { ImageFactoryMirrorURL, factoryTryTimeout = prev, prevTry })
}

// A factory that can't be reached - 5xx, no answer in time, nothing
// listening - leaves the update to its mirror, said so.
func TestCheckUpdateFromMirror(t *testing.T) {
	node := nodeWith("node-exporter")
	body := `{"schematic":"` + node.SchematicId + `","version":"v2","bundle_url":"https://mirror.invalid/image/x/v2/amd64","sha256":"abc","state":"ready"}`
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	for name, factory := range map[string]http.HandlerFunc{
		"503": func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "no server", http.StatusServiceUnavailable)
		},
		"timeout": func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(time.Second):
			}
		},
		"closed": nil,
	} {
		t.Run(name, func(t *testing.T) {
			url := closed.URL
			if factory != nil {
				srv := httptest.NewServer(factory)
				t.Cleanup(srv.Close)
				url = srv.URL
			}
			var calls atomic.Int32
			resetFactory(t, url)
			resetMirror(t, fakeMirror(t, &calls, http.StatusOK, body, "").URL)
			uc := checkUpdate(context.Background(), node)
			if uc.Via != "mirror" || uc.State != "ready" || uc.BundleURL != "https://mirror.invalid/image/x/v2/amd64" || !uc.UpdateAvailable {
				t.Fatalf("update check = %+v", uc)
			}
		})
	}
}

// The factory's own answer - a refusal included - is the answer: the
// mirror isn't asked.
func TestCheckUpdateFactoryAnswerSkipsMirror(t *testing.T) {
	var calls, mirrored atomic.Int32
	resetFactory(t, fakeFactory(t, &calls, http.StatusNotFound, `{"error":"no release offers this schematic's extensions"}`).URL)
	resetMirror(t, fakeMirror(t, &mirrored, http.StatusOK, `{}`, "").URL)
	uc := checkUpdate(context.Background(), nodeWith("qemu-guest-agent"))
	if uc.State != "unavailable" || uc.Via != "" || !strings.Contains(uc.Message, "no release offers") || mirrored.Load() != 0 {
		t.Fatalf("update check = %+v (mirror asked %d times)", uc, mirrored.Load())
	}
}

// Both out: the message says what each said.
func TestCheckUpdateFactoryAndMirrorDown(t *testing.T) {
	var mirrored atomic.Int32
	factory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}))
	t.Cleanup(factory.Close)
	resetFactory(t, factory.URL)
	resetMirror(t, fakeMirror(t, &mirrored, http.StatusNotFound, `{"error":"this mirror has no update for this schematic"}`, "").URL)
	uc := checkUpdate(context.Background(), nodeWith("node-exporter"))
	if uc.State != "unavailable" || !strings.Contains(uc.Message, "image factory: 502") || !strings.Contains(uc.Message, "the image factory's mirror: this mirror has no update") {
		t.Fatalf("update check = %+v", uc)
	}
}

// Machines and clusters resolve their update bundle the same way; a
// disk image (a new machine's) never comes from the mirror.
func TestResolveBundleFromMirror(t *testing.T) {
	factory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no server", http.StatusServiceUnavailable)
	}))
	t.Cleanup(factory.Close)
	resetFactory(t, factory.URL)
	var mirrored atomic.Int32
	image := `{"state":"ready","files":[{"name":"rootfs.squashfs","url":"https://mirror.invalid/image/x/v2/amd64/rootfs.squashfs","size":4,"sha256":"` + strings.Repeat("d", 64) + `"}]}`
	resetMirror(t, fakeMirror(t, &mirrored, http.StatusOK, "", image).URL)
	sc := &schematic.Schematic{Customization: schematic.Customization{Extensions: []string{"node-exporter"}}}

	b, _, err := ResolveBundle(context.Background(), "v2", sc)
	if err != nil || b.BaseURL != "https://mirror.invalid/image/x/v2/amd64" || b.SHA256 != strings.Repeat("d", 64) {
		t.Fatalf("bundle = %+v, %v", b, err)
	}
	n := mirrored.Load()
	if _, err := ResolveVMImage(context.Background(), "v2", sc, "janus-kvm.qcow2"); err == nil {
		t.Error("a disk image resolved while the factory is down")
	}
	if mirrored.Load() != n {
		t.Error("the mirror was asked for a disk image")
	}
}
