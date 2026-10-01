package updaterapi

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"testing"
)

func TestNewer(t *testing.T) {
	for _, tc := range []struct {
		release, running string
		newer, known     bool
	}{
		{"v2026.10.02", "v2026.10.01-3", true, true},
		{"v2026.10.01-3", "v2026.10.01-2", true, true},
		{"v2026.10.01-2", "v2026.10.01", true, true},
		{"v2026.10.01-3", "v2026.10.01-3", false, true},
		{"v2026.10.01-2", "v2026.10.01-3", false, true},
		// A build after a release is newer than that release...
		{"v2026.10.01-3", "v2026.10.01-3-4-gc4f2f7c", false, true},
		{"v2026.10.01-3", "v2026.10.01-3-4-gc4f2f7c-dirty", false, true},
		// ...but older than the next one.
		{"v2026.10.01-4", "v2026.10.01-3-4-gc4f2f7c", true, true},
		// v2026.10.01 plus 5 commits, not v2026.10.01-5.
		{"v2026.10.01-2", "v2026.10.01-5-gabc1234", true, true},
		{"v2026.11.01", "v2026.10.31-9", true, true},
		{"v2027.01.01", "v2026.12.31", true, true},
		{"v2026.10.02", "dev", false, false},
		{"v2026.10.02", "c4f2f7c", false, false},
		{"latest", "v2026.10.01", false, false},
	} {
		newer, known := Newer(tc.release, tc.running)
		if newer != tc.newer || known != tc.known {
			t.Errorf("Newer(%q, %q) = %v, %v; want %v, %v", tc.release, tc.running, newer, known, tc.newer, tc.known)
		}
	}
}

func TestCheckImage(t *testing.T) {
	const repo = "swenske/janus-controller"
	digest := "sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	ok := []struct{ image, want string }{
		{"", repo + ":v2026.10.02"},
		{repo + ":v2026.10.02", repo + ":v2026.10.02"},
		{repo + ":v2026.10.02@" + digest, repo + ":v2026.10.02@" + digest},
	}
	for _, tc := range ok {
		got, err := CheckImage(tc.image, repo, "v2026.10.02")
		if err != nil || got != tc.want {
			t.Errorf("CheckImage(%q) = %q, %v; want %q", tc.image, got, err, tc.want)
		}
	}
	for _, tc := range []struct{ image, version string }{
		{"", "latest"},
		{"", "v2026.10.02-1-gabc1234"},
		{"evil/janus-controller:v2026.10.02", "v2026.10.02"},
		{repo + ":v2026.10.01", "v2026.10.02"},
		{repo + ":v2026.10.02@sha256:abc", "v2026.10.02"},
		{repo + "@" + digest, "v2026.10.02"},
	} {
		if got, err := CheckImage(tc.image, repo, tc.version); err == nil {
			t.Errorf("CheckImage(%q, %q) = %q, want an error", tc.image, tc.version, got)
		}
	}
}

func TestClient(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "u.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var started StartedRequest
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(Status{Version: "v1", Ready: true, Job: &Job{ID: "j", State: JobDone}})
	})
	mux.HandleFunc("POST /v1/started", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&started)
	})
	mux.HandleFunc("POST /v1/update", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "an update is already running", http.StatusConflict)
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	c := NewClient(sock)
	ctx := context.Background()
	st, err := c.Status(ctx)
	if err != nil || !st.Ready || st.Job.State != JobDone {
		t.Fatalf("Status = %+v, %v", st, err)
	}
	if err := c.Started(ctx, "v2026.10.02"); err != nil || started.Version != "v2026.10.02" {
		t.Fatalf("Started: %v, server got %+v", err, started)
	}
	_, err = c.Update(ctx, UpdateRequest{Version: "v2026.10.02"})
	if e, ok := err.(*Error); !ok || e.Code != http.StatusConflict || e.Message != "an update is already running" {
		t.Fatalf("Update error = %#v", err)
	}
}
