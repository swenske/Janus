package main

import "net/http"

// cspReportOnly is the Content-Security-Policy the page and the node
// pages are checked against, in report-only mode for now: a browser
// logs what it would have blocked in its console (what the Playwright
// checks of the UI read) and blocks nothing. The policy matches what
// the built frontends load: their own scripts and stylesheets (Vite's
// hashed assets), React's inline styles, images from data: and blob:
// URLs (the TOTP QR code, downloads), requests to this origin only.
// Once the console stays clean through qemu-dashboard-test, it becomes
// Content-Security-Policy.
const cspReportOnly = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data: blob:; connect-src 'self'; object-src 'none'; base-uri 'self'; " +
	"form-action 'self'; frame-ancestors 'none'"

// securityHeaders adds the defence-in-depth headers every response of
// the main port carries: the session cookie's SameSite=Strict and
// CrossOriginProtection stop cross-site requests, these stop a browser
// from sniffing a type, framing a page or leaking its URL, and keep it
// on HTTPS for a year (the port is HTTPS only - nothing to downgrade
// to).
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Strict-Transport-Security", "max-age=31536000")
		h.Set("Content-Security-Policy-Report-Only", cspReportOnly)
		next.ServeHTTP(w, r)
	})
}
