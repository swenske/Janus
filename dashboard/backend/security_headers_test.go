package main

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The policy is enforced, never report-only, and says where reports go.
func TestSecurityHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	securityHeaders(http.NotFoundHandler()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	h := rec.Header()
	if got := h.Get("Content-Security-Policy"); got != csp || !strings.Contains(got, "script-src 'self';") || !strings.HasSuffix(got, "report-uri /csp-report") {
		t.Errorf("Content-Security-Policy = %q", got)
	}
	if h.Get("Content-Security-Policy-Report-Only") != "" {
		t.Error("still a report-only policy")
	}
	for k, v := range map[string]string{"X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY", "Strict-Transport-Security": "max-age=31536000"} {
		if h.Get(k) != v {
			t.Errorf("%s = %q", k, h.Get(k))
		}
	}
}

// A browser's report is logged - quoted, cut, a few a minute - and
// anything else refused.
func TestCSPReport(t *testing.T) {
	var logs bytes.Buffer
	prev, flags := log.Writer(), log.Flags()
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prev); log.SetFlags(flags) })

	h := cspReport()
	post := func(body string) int {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPost, cspReportPath, strings.NewReader(body)))
		return rec.Code
	}
	report := `{"csp-report":{"document-uri":"https://c.example/nodes/n1/","violated-directive":"img-src 'self' data: blob:","blocked-uri":"https://evil.example/x.png\n"}}`
	if code := post(report); code != http.StatusNoContent {
		t.Fatalf("report answered %d", code)
	}
	if want := `csp: "https://c.example/nodes/n1/" blocked "img-src": "https://evil.example/x.png\n"` + "\n"; logs.String() != want {
		t.Errorf("logged %q, want %q", logs.String(), want)
	}
	for range 2 * cspReportsPerMinute {
		post(report)
	}
	if n := strings.Count(logs.String(), "csp: "); n != cspReportsPerMinute {
		t.Errorf("%d lines logged in a minute, want %d", n, cspReportsPerMinute)
	}
	for _, body := range []string{"", "{}", `{"csp-report":{}}`, "not json"} {
		if code := post(body); code != http.StatusBadRequest {
			t.Errorf("%q answered %d", body, code)
		}
	}
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, cspReportPath, nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET answered %d", rec.Code)
	}
}
