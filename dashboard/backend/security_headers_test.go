package main

import (
	"bytes"
	"fmt"
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
	postFrom := func(addr, body string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, cspReportPath, strings.NewReader(body))
		req.RemoteAddr = addr
		h(rec, req)
		return rec.Code
	}
	post := func(body string) int { return postFrom("192.0.2.1:1234", body) }
	report := `{"csp-report":{"document-uri":"https://c.example/nodes/n1/","violated-directive":"img-src 'self' data: blob:","blocked-uri":"https://evil.example/x.png\n"}}`
	if code := post(report); code != http.StatusNoContent {
		t.Fatalf("report answered %d", code)
	}
	if want := `csp: "https://c.example/nodes/n1/" blocked "img-src": "https://evil.example/x.png\n"` + "\n"; logs.String() != want {
		t.Errorf("logged %q, want %q", logs.String(), want)
	}
	// One address posting in a loop gets its few lines, not the minute's
	// budget: the others' reports still come through, up to the budget.
	for range 2 * cspReportsPerMinute {
		post(report)
	}
	if n := strings.Count(logs.String(), "csp: "); n != cspReportsPerSource {
		t.Errorf("one address: %d lines logged, want %d", n, cspReportsPerSource)
	}
	for i := range 2 * cspReportsPerMinute {
		if code := postFrom(fmt.Sprintf("198.51.100.%d:443", i+1), report); code != http.StatusNoContent {
			t.Fatalf("answered %d", code)
		}
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
