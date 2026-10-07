// Command janus-controller-updater updates the Janus Controller when an
// administrator asks for it from the Controller's page - the Controller
// never gets the Docker socket itself, this small companion does, and it
// only knows how to do one thing: move the Controller's Compose service
// to the image of a published release.
//
// It does what an operator would do by hand - set the image variable in
// the Compose project's .env and run docker compose up -d for the
// Controller's service - with a safety net: the image is pulled first
// (nothing changes if that fails), the Controller's data and .env are
// backed up, and if the new version doesn't come up (the Controller says
// so over the shared socket, see updaterapi) everything is put back.
//
// It runs in the Controller's own image, from the same compose.yaml
// (dashboard/README.md, "Updating the Controller"), and needs: the
// Docker socket, the Compose directory mounted at the same path, the
// Controller's data volume, and a volume shared with the Controller for
// its socket.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/swenske/Janus/dashboard/updater/updaterapi"
)

// version is set at build time (-X main.version), like dashboardd's.
var version = "dev"

type config struct {
	Socket       string
	DockerSocket string
	StateDir     string
	DataDir      string
	Project      string
	Service      string
	Variable     string
	Repository   string
	ComposeBin   string
	StartTimeout time.Duration
	Stable       time.Duration
}

func configFromEnv() config {
	return config{
		Socket:       env("JANUS_UPDATER_SOCKET", updaterapi.DefaultSocket),
		DockerSocket: env("JANUS_UPDATER_DOCKER_SOCKET", "/var/run/docker.sock"),
		StateDir:     env("JANUS_UPDATER_STATE", "/var/lib/janus-updater"),
		DataDir:      env("JANUS_UPDATER_DATA", "/data"),
		Project:      env("JANUS_UPDATER_PROJECT", ""),
		Service:      env("JANUS_UPDATER_SERVICE", "janus-controller"),
		Variable:     env("JANUS_UPDATER_VARIABLE", "JANUS_CONTROLLER_IMAGE"),
		Repository:   env("JANUS_UPDATER_REPOSITORY", "swenske/janus-controller"),
		ComposeBin:   env("JANUS_UPDATER_COMPOSE", "/usr/local/bin/docker-compose"),
		StartTimeout: duration("JANUS_UPDATER_START_TIMEOUT", 2*time.Minute),
		Stable:       duration("JANUS_UPDATER_STABLE", 10*time.Second),
	}
}

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	cfg := configFromEnv()
	for _, dir := range []string{cfg.StateDir, filepath.Join(cfg.StateDir, "home"), filepath.Join(cfg.StateDir, "docker"), filepath.Join(cfg.StateDir, "tmp")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			log.Fatalf("state directory: %v", err)
		}
	}
	u := newUpdater(cfg, &realPlatform{cfg: cfg, docker: newDocker(cfg.DockerSocket)})

	// The socket's directory is a volume only the two containers mount:
	// that mount is who may talk to the updater. The Controller runs as
	// 65532 (dashboard/Dockerfile), the updater as root, so the socket
	// is open to any user of the directory.
	if err := os.MkdirAll(filepath.Dir(cfg.Socket), 0o755); err != nil {
		log.Fatalf("socket directory: %v", err)
	}
	if err := os.Remove(cfg.Socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Fatalf("remove stale socket: %v", err)
	}
	ln, err := net.Listen("unix", cfg.Socket)
	if err != nil {
		log.Fatalf("listen on %s: %v", cfg.Socket, err)
	}
	if err := os.Chmod(cfg.Socket, 0o666); err != nil {
		log.Fatalf("chmod %s: %v", cfg.Socket, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	st := u.status(ctx)
	cancel()
	log.Printf("janus-controller-updater %s listening on %s - service %q of project %q", version, cfg.Socket, cfg.Service, st.Project)
	if st.Ready {
		log.Printf("ready to update the Controller (now running %s)", st.CurrentImage)
	} else {
		for _, p := range st.Problems {
			log.Printf("not ready: %s", p)
		}
	}
	log.Fatal((&http.Server{Handler: u.handler(), ReadHeaderTimeout: 10 * time.Second}).Serve(ln))
}

func (u *updater) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, u.status(r.Context()))
	})
	mux.HandleFunc("POST /v1/update", func(w http.ResponseWriter, r *http.Request) {
		var req updaterapi.UpdateRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			http.Error(w, "decode request: "+err.Error(), http.StatusBadRequest)
			return
		}
		job, err := u.start(req)
		switch {
		case errors.Is(err, errBusy):
			http.Error(w, err.Error(), http.StatusConflict)
		case err != nil:
			http.Error(w, err.Error(), http.StatusBadRequest)
		default:
			writeJSON(w, http.StatusAccepted, job)
		}
	})
	mux.HandleFunc("POST /v1/started", func(w http.ResponseWriter, r *http.Request) {
		var req updaterapi.StartedRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil || req.Version == "" {
			http.Error(w, "a version is required", http.StatusBadRequest)
			return
		}
		log.Printf("the Controller %s has started", req.Version)
		u.checkins.add(req.Version)
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func duration(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		log.Fatalf("%s=%q isn't a duration (like 2m or 30s)", key, v)
	}
	return d
}
