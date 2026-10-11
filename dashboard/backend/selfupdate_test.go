package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/swenske/Janus/dashboard/backend/internal/nodeproxy"
	"github.com/swenske/Janus/dashboard/updater/updaterapi"
)

const (
	pinned    = "swenske/janus-controller:v2026.10.02@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	pinnedPre = "swenske/janus-controller:v2026.10.03@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
)

// fakeUpdater answers on a Unix socket like janus-controller-updater.
type fakeUpdater struct {
	mu       sync.Mutex
	ready    bool
	started  []string
	requests []updaterapi.UpdateRequest
}

func (f *fakeUpdater) serve(t *testing.T) string {
	sock := filepath.Join(t.TempDir(), "updater.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		st := updaterapi.Status{Version: "v2026.10.01", Ready: f.ready, Repository: "swenske/janus-controller", Project: "janus"}
		if !f.ready {
			st.Problems = []string{"the updater can't see /opt/janus/compose.yaml"}
		}
		_ = json.NewEncoder(w).Encode(st)
	})
	mux.HandleFunc("POST /v1/update", func(w http.ResponseWriter, r *http.Request) {
		var req updaterapi.UpdateRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.requests = append(f.requests, req)
		f.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(updaterapi.Job{ID: "1", ToVersion: req.Version, ToImage: req.Image, State: updaterapi.JobRunning})
	})
	mux.HandleFunc("POST /v1/started", func(w http.ResponseWriter, r *http.Request) {
		var req updaterapi.StartedRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.started = append(f.started, req.Version)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Close() })
	return sock
}

func TestControllerUpdate(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := "http://" + r.Host
		switch r.URL.Path {
		case "/releases":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"tag_name": "v2026.10.03", "html_url": "https://github.com/swenske/Janus/releases/tag/v2026.10.03", "published_at": "2026-10-03T08:00:00Z",
				"prerelease": true,
				"assets":     []map[string]string{{"name": "controller-image.txt", "browser_download_url": base + "/pre/controller-image.txt"}},
			}, {
				"tag_name": "v2026.10.02", "html_url": "https://github.com/swenske/Janus/releases/tag/v2026.10.02", "published_at": "2026-10-02T08:00:00Z",
				"assets": []map[string]string{{"name": "controller-image.txt", "browser_download_url": base + "/controller-image.txt"}},
			}})
		case "/controller-image.txt":
			_, _ = w.Write([]byte(pinned + "\n"))
		case "/pre/controller-image.txt":
			_, _ = w.Write([]byte(pinnedPre + "\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer gh.Close()
	prevURL, prevVersion := nodeproxy.ReleasesURL, version
	nodeproxy.ReleasesURL, version = gh.URL+"/releases", "v2026.10.01-3"
	t.Cleanup(func() { nodeproxy.ReleasesURL, version = prevURL, prevVersion })

	f := &fakeUpdater{}
	a := &app{selfUpdate: newSelfUpdate(f.serve(t))}
	get := func() controllerUpdateView {
		rec := httptest.NewRecorder()
		a.handleControllerUpdate(rec, httptest.NewRequest(http.MethodGet, "/api/controller/update", nil))
		var v controllerUpdateView
		if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
			t.Fatalf("GET: %d %s", rec.Code, rec.Body)
		}
		return v
	}
	postBody := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		a.handleControllerUpdate(rec, httptest.NewRequest(http.MethodPost, "/api/controller/update", strings.NewReader(body)))
		return rec
	}
	post := func(version string) *httptest.ResponseRecorder { return postBody(`{"version":"` + version + `"}`) }

	// The pre-release is offered apart, never as the update.
	v := get()
	if !v.UpdateAvailable || !v.VersionKnown || v.Latest.Version != "v2026.10.02" || v.Latest.Image != pinned {
		t.Fatalf("view = %+v, latest %+v", v, v.Latest)
	}
	if v.Prerelease == nil || v.Prerelease.Version != "v2026.10.03" || v.Prerelease.Image != pinnedPre || v.RunningPrerelease {
		t.Fatalf("pre-release = %+v (running one: %v)", v.Prerelease, v.RunningPrerelease)
	}
	if !v.Updater.Configured || !v.Updater.Reachable || v.Updater.Ready || len(v.Updater.Problems) != 1 {
		t.Fatalf("updater = %+v", v.Updater)
	}
	if rec := post("v2026.10.02"); rec.Code != http.StatusPreconditionFailed || !strings.Contains(rec.Body.String(), "can't see /opt/janus/compose.yaml") {
		t.Errorf("POST with the updater not ready: %d %s", rec.Code, rec.Body)
	}

	f.mu.Lock()
	f.ready = true
	f.mu.Unlock()
	if rec := post("v2026.10.01-9"); rec.Code != http.StatusConflict {
		t.Errorf("POST of another version: %d %s", rec.Code, rec.Body)
	}
	if rec := post("v2026.10.02"); rec.Code != http.StatusAccepted {
		t.Fatalf("POST: %d %s", rec.Code, rec.Body)
	}
	f.mu.Lock()
	reqs := f.requests
	f.mu.Unlock()
	want := updaterapi.UpdateRequest{Version: "v2026.10.02", Image: pinned, FromVersion: "v2026.10.01-3"}
	if len(reqs) != 1 || reqs[0] != want {
		t.Errorf("the updater got %+v, want %+v", reqs, want)
	}

	// The pre-release only with its confirmation, and the release never
	// as a pre-release.
	if rec := post("v2026.10.03"); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "pre-release") {
		t.Errorf("POST of the pre-release without confirming it: %d %s", rec.Code, rec.Body)
	}
	if rec := postBody(`{"version":"v2026.10.02","prerelease":true}`); rec.Code != http.StatusConflict {
		t.Errorf("POST of the release as a pre-release: %d %s", rec.Code, rec.Body)
	}
	if rec := postBody(`{"version":"v2026.10.03","prerelease":true}`); rec.Code != http.StatusAccepted {
		t.Fatalf("POST of the pre-release: %d %s", rec.Code, rec.Body)
	}
	f.mu.Lock()
	reqs = f.requests
	f.mu.Unlock()
	want = updaterapi.UpdateRequest{Version: "v2026.10.03", Image: pinnedPre, FromVersion: "v2026.10.01-3"}
	if len(reqs) != 2 || reqs[1] != want {
		t.Errorf("the updater got %+v, want %+v last", reqs, want)
	}

	a.selfUpdate.announce()
	f.mu.Lock()
	started := f.started
	f.mu.Unlock()
	if len(started) != 1 || started[0] != "v2026.10.01-3" {
		t.Errorf("check-ins = %q", started)
	}

	// Already on the newest release: nothing to install but the
	// pre-release.
	version = "v2026.10.02"
	if v := get(); v.UpdateAvailable || v.Prerelease == nil {
		t.Errorf("on the newest release: update %v, pre-release %+v", v.UpdateAvailable, v.Prerelease)
	}
	if rec := post("v2026.10.02"); rec.Code != http.StatusConflict {
		t.Errorf("POST on the newest release: %d %s", rec.Code, rec.Body)
	}

	// On the pre-release: no update, no pre-release left to offer - the
	// older release isn't one.
	version = "v2026.10.03"
	if v := get(); v.UpdateAvailable || v.Prerelease != nil || !v.RunningPrerelease {
		t.Errorf("on the pre-release: update %v, pre-release %+v, running one %v", v.UpdateAvailable, v.Prerelease, v.RunningPrerelease)
	}
}

func TestNoUpdater(t *testing.T) {
	s := newSelfUpdate(filepath.Join(t.TempDir(), "missing.sock"))
	if s.configured() {
		t.Fatal("configured without a socket")
	}
	s.announce() // returns at once
	if newSelfUpdate("").configured() {
		t.Fatal("configured with -updater-socket=''")
	}
}
