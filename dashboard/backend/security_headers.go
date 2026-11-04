package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// csp is the Content-Security-Policy of the page and the node pages,
// enforced since #501 (report-only until then, with no violation through
// hack/ui-preview.sh check and controller-sso-test). It allows what the
// built frontends load (docs/controller-ui.md, "Content Security
// Policy"): their own scripts and stylesheets (Vite's hashed assets,
// theme-boot.js - no inline script), React's inline styles, images from
// data: and blob: URLs (the TOTP QR code, downloads), requests to this
// origin only - nothing from anywhere else, no plugin, no framing. What
// it blocks a browser reports to cspReportPath, logged.
const csp = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data: blob:; connect-src 'self'; object-src 'none'; base-uri 'self'; " +
	"form-action 'self'; frame-ancestors 'none'; report-uri " + cspReportPath

// cspReportPath is where browsers send what the policy blocked - report-
// uri: sent at once, by every browser (report-to's reports come in
// batches, up to a minute later). Outside /api/: no session, not
// audited.
const cspReportPath = "/csp-report"

// securityHeaders adds the defence-in-depth headers every response of
// the main port carries: the session cookie's SameSite=Strict and
// CrossOriginProtection stop cross-site requests, these stop a browser
// from sniffing a type, framing a page, leaking its URL or running what
// the pages don't ship, and keep it on HTTPS for a year (the port is
// HTTPS only - nothing to downgrade to).
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Strict-Transport-Security", "max-age=31536000")
		h.Set("Content-Security-Policy", csp)
		next.ServeHTTP(w, r)
	})
}

// cspReportsPerMinute bounds what cspReport logs: anyone can post
// reports, and one page left open repeats the same violation.
const cspReportsPerMinute = 10

// cspReport logs what browsers say the policy blocked - `csp: "<page>"
// blocked "<directive>": "<what>"` - a page that broke under it shows in the
// Controller's log. At most cspReportsPerMinute lines a minute, then how
// many were left out.
func cspReport() http.HandlerFunc {
	var mu sync.Mutex
	var window time.Time
	var logged, dropped int
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		if err != nil {
			http.Error(w, "unreadable report", http.StatusBadRequest)
			return
		}
		// report-uri's form: {"csp-report": {"document-uri": ...}}.
		var rep struct {
			Report struct {
				Document  string `json:"document-uri"`
				Directive string `json:"effective-directive"`
				Violated  string `json:"violated-directive"`
				Blocked   string `json:"blocked-uri"`
			} `json:"csp-report"`
		}
		if json.Unmarshal(body, &rep) != nil || rep.Report.Document == "" {
			http.Error(w, "not a CSP report", http.StatusBadRequest)
			return
		}
		directive := rep.Report.Directive
		if directive == "" {
			directive, _, _ = strings.Cut(rep.Report.Violated, " ")
		}
		mu.Lock()
		now := time.Now()
		if now.Sub(window) >= time.Minute {
			if dropped > 0 {
				log.Printf("csp: %d more reports in the last minute, not logged", dropped)
			}
			window, logged, dropped = now, 0, 0
		}
		if logged < cspReportsPerMinute {
			logged++
			log.Printf("csp: %q blocked %q: %q", clip(rep.Report.Document), clip(directive), clip(rep.Report.Blocked))
		} else {
			dropped++
		}
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}
}

// clip cuts what a browser - or anyone - sent to 200 bytes for a log
// line (quoted there).
func clip(s string) string {
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
