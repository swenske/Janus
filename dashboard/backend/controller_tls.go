package main

import (
	"errors"
	"net/http"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/auth"
	"github.com/swenske/Janus/dashboard/backend/internal/uitls"
)

// --- the main port's certificate: /api/controller/tls (internal/uitls) ---
//
// An admin gives the page a certificate of its own - a public one, or
// one of the organization's CA - instead of the Controller's
// self-signed identity; nodes keep registering against that identity on
// the registration port. -tls-cert/-tls-key, when given, win: the page
// then only shows them.

func (a *app) registerControllerTLSRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/controller/tls", a.gate(auth.Admin, auth.Admin, a.handleControllerTLS))
	mux.HandleFunc("POST /api/controller/tls/check", a.gate(auth.Admin, auth.Admin, a.handleControllerTLSCheck))
	mux.HandleFunc("PUT /api/controller/tls", a.gate(auth.Admin, auth.Admin, a.handleControllerTLSSet))
	mux.HandleFunc("DELETE /api/controller/tls", a.gate(auth.Admin, auth.Admin, a.handleControllerTLSRemove))
}

// controllerTLSView is the served certificate, and whether the page may
// change it.
type controllerTLSView struct {
	uitls.Info
	// FromFiles: -tls-cert/-tls-key give it - changed by replacing them.
	FromFiles bool `json:"from_files"`
}

func (a *app) controllerTLSView(r *http.Request) controllerTLSView {
	cert, source := a.ui.Current()
	return controllerTLSView{Info: uitls.Describe(cert, source, r.Host, time.Now()), FromFiles: a.ui.FromFiles()}
}

func (a *app) handleControllerTLS(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.controllerTLSView(r))
}

type controllerTLSRequest struct {
	// PEM is the certificate, its chain and its private key.
	PEM string `json:"pem"`
}

// handleControllerTLSCheck says what a bundle is - and what may go wrong
// serving it - without serving it.
func (a *app) handleControllerTLSCheck(w http.ResponseWriter, r *http.Request) {
	var req controllerTLSRequest
	if !decodeBody(w, r, &req) {
		return
	}
	cert, err := uitls.Check([]byte(req.PEM), time.Now())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, uitls.Describe(cert, uitls.SourceUploaded, r.Host, time.Now()))
}

func (a *app) handleControllerTLSSet(w http.ResponseWriter, r *http.Request) {
	var req controllerTLSRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if _, err := a.ui.Upload([]byte(req.PEM)); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, uitls.ErrFromFiles) {
			status = http.StatusConflict
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a.controllerTLSView(r))
}

func (a *app) handleControllerTLSRemove(w http.ResponseWriter, r *http.Request) {
	if err := a.ui.Remove(); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, uitls.ErrFromFiles) {
			status = http.StatusConflict
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a.controllerTLSView(r))
}
