package nodeproxy

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// TestRunUpgradeStreams: a page asking for NDJSON gets each of the
// node's messages as it comes, "done" last (v2026.11.09-2's lab
// campaign: the retries after a 504 never reached the Update page);
// without, the last message alone, as before.
func TestRunUpgradeStreams(t *testing.T) {
	_, client := fakeNode(t)
	req := &janusv1alpha1.UpgradeRequest{Source: &janusv1alpha1.ImageSource{Reference: "https://example.invalid/v"}}

	r := httptest.NewRequest(http.MethodPost, "/api/lifecycle/upgrade-url", nil)
	r.Header.Set("Accept", "application/x-ndjson")
	rec := httptest.NewRecorder()
	runUpgrade(rec, r, client, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "ndjson") {
		t.Fatalf("%d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	var lines []relayProgress
	sc := bufio.NewScanner(rec.Body)
	for sc.Scan() {
		var p relayProgress
		if err := json.Unmarshal(sc.Bytes(), &p); err != nil {
			t.Fatalf("%q: %v", sc.Text(), err)
		}
		lines = append(lines, p)
	}
	if len(lines) != 3 || lines[0].Stage != "writing-data" || lines[0].Percent != 40 || lines[0].Message != "writing slot B" ||
		lines[1].Stage != "rebooting" || lines[2].Stage != "done" || lines[2].Message != "rebooting into slot B" {
		t.Fatalf("lines: %+v", lines)
	}

	rec = httptest.NewRecorder()
	runUpgrade(rec, httptest.NewRequest(http.MethodPost, "/api/lifecycle/upgrade-url", nil), client, req)
	var last struct{ Stage, Message string }
	if err := json.Unmarshal(rec.Body.Bytes(), &last); rec.Code != http.StatusOK || err != nil || last.Stage != "rebooting" || last.Message != "rebooting into slot B" {
		t.Fatalf("without NDJSON: %d %s", rec.Code, rec.Body.String())
	}
}
