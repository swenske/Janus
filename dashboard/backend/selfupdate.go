package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/nodeproxy"
	"github.com/swenske/Janus/dashboard/updater/updaterapi"
)

// The Controller's own updates (dashboard/README.md, "Updating the
// Controller"): the main page says when a newer release exists, and - if
// the Compose project runs janus-controller-updater next to the Controller
// - installs it after the administrator confirms. The Controller only
// asks; the updater, which has the Docker socket, does it, and rolls back
// if this new Controller doesn't tell it, once started, that it runs.

// defaultRepository is where release images are published - used for the
// manual instructions when no updater says otherwise.
const defaultRepository = "swenske/janus-controller"

type selfUpdate struct {
	socket string
	client *updaterapi.Client
}

func newSelfUpdate(socket string) *selfUpdate {
	if socket == "" {
		return &selfUpdate{}
	}
	return &selfUpdate{socket: socket, client: updaterapi.NewClient(socket)}
}

// configured: the updater's socket is there (its volume is mounted and
// an updater created it at some point).
func (s *selfUpdate) configured() bool {
	if s.client == nil {
		return false
	}
	_, err := os.Stat(s.socket)
	return err == nil
}

// announce tells the updater this Controller has started - called once
// it's listening. That's what an update waits for: no word from the new
// version within the updater's timeout and it rolls back.
func (s *selfUpdate) announce() {
	if !s.configured() {
		return
	}
	var err error
	for range 15 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err = s.client.Started(ctx, version)
		cancel()
		if err == nil {
			log.Printf("told the updater (%s) this Controller %s has started", s.socket, version)
			return
		}
		time.Sleep(2 * time.Second)
	}
	log.Printf("updater at %s: %v - a running update will roll back", s.socket, err)
}

type controllerRelease struct {
	Version     string `json:"version"`
	URL         string `json:"url"`
	PublishedAt string `json:"published_at"`
	// Image is what the Controller runs for that release: pinned to the
	// digest the release published, or its tag for older releases.
	Image string `json:"image"`
}

type updaterView struct {
	// Configured: the socket is there; Reachable: the updater answered.
	Configured bool   `json:"configured"`
	Reachable  bool   `json:"reachable"`
	Error      string `json:"error,omitempty"`
	*updaterapi.Status
}

type controllerUpdateView struct {
	Version string             `json:"version"`
	Latest  *controllerRelease `json:"latest,omitempty"`
	// LatestError: the newest release couldn't be found (no internet...).
	LatestError     string `json:"latest_error,omitempty"`
	UpdateAvailable bool   `json:"update_available"`
	// SecurityUpdate: the newer releases fix vulnerabilities in this
	// Controller, the worst this severe (nodeproxy.ReleaseInfo.SecurityUpdate).
	SecurityUpdate string `json:"security_update,omitempty"`
	// VersionKnown is false for a build that isn't a release or after one
	// ("dev") - nothing to compare, any release can be installed.
	VersionKnown bool         `json:"version_known"`
	Repository   string       `json:"repository"`
	Updater      *updaterView `json:"updater"`
}

func (a *app) controllerUpdate(ctx context.Context) controllerUpdateView {
	v := controllerUpdateView{Version: version, Repository: defaultRepository, Updater: &updaterView{}}
	if rel, err := nodeproxy.LatestRelease(ctx); err != nil {
		v.LatestError = err.Error()
	} else {
		v.Latest = &controllerRelease{Version: rel.TagName, URL: rel.HTMLURL, PublishedAt: rel.PublishedAt, Image: rel.ControllerImage}
		v.UpdateAvailable, v.VersionKnown = updaterapi.Newer(rel.TagName, version)
		if !v.VersionKnown {
			v.UpdateAvailable = updaterapi.IsRelease(rel.TagName) && rel.TagName != version
		}
		v.SecurityUpdate, _ = rel.SecurityUpdate(version, "controller", nodeproxy.NodeImage{})
	}
	if a.selfUpdate.configured() {
		v.Updater.Configured = true
		sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		st, err := a.selfUpdate.client.Status(sctx)
		cancel()
		if err != nil {
			v.Updater.Error = err.Error()
		} else {
			v.Updater.Reachable = true
			v.Updater.Status = st
			if st.Repository != "" {
				v.Repository = st.Repository
			}
		}
	}
	if v.Latest != nil && v.Latest.Image == "" {
		v.Latest.Image = v.Repository + ":" + v.Latest.Version
	}
	return v
}

func (a *app) handleControllerUpdate(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, a.controllerUpdate(r.Context()))
	case http.MethodPost:
		var req struct {
			Version string `json:"version"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			http.Error(w, "decode request: "+err.Error(), http.StatusBadRequest)
			return
		}
		v := a.controllerUpdate(r.Context())
		switch {
		case v.Latest == nil:
			http.Error(w, "the newest release is unknown: "+v.LatestError, http.StatusBadGateway)
			return
		case req.Version != v.Latest.Version:
			http.Error(w, fmt.Sprintf("only the newest release, %s, can be installed from here", v.Latest.Version), http.StatusConflict)
			return
		case !v.UpdateAvailable:
			http.Error(w, "this Controller already runs "+version, http.StatusConflict)
			return
		case !v.Updater.Configured:
			http.Error(w, "no updater runs next to this Controller - see dashboard/README.md, Updating the Controller", http.StatusPreconditionFailed)
			return
		case !v.Updater.Reachable:
			http.Error(w, "the updater doesn't answer: "+v.Updater.Error, http.StatusBadGateway)
			return
		case !v.Updater.Ready:
			http.Error(w, "the updater can't update yet: "+fmt.Sprint(v.Updater.Problems), http.StatusPreconditionFailed)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		job, err := a.selfUpdate.client.Update(ctx, updaterapi.UpdateRequest{Version: v.Latest.Version, Image: v.Latest.Image, FromVersion: version})
		if err != nil {
			code := http.StatusBadGateway
			var ue *updaterapi.Error
			if errors.As(err, &ue) {
				code = ue.Code
			}
			http.Error(w, err.Error(), code)
			return
		}
		log.Printf("update of the Controller from %s to %s (%s) started", version, job.ToVersion, job.ToImage)
		writeJSON(w, http.StatusAccepted, job)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
