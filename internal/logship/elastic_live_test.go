package logship

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/swenske/Janus/internal/tlstrust"
)

// TestLiveElasticsearch sends to a real cluster - never in CI, only with
// JANUS_ES_URL set (the test bench of docs/guide/log-forwarding.md):
//
//	JANUS_ES_URL          https://elastic.example.com
//	JANUS_ES_API_KEY      a key limited to create_doc on logs-janus*
//	                      (or JANUS_ES_USER + JANUS_ES_PASSWORD)
//	JANUS_ES_CA_FILE      the CA that signed the cluster's certificate,
//	JANUS_ES_FINGERPRINT  or the fingerprint of its certificate or of a CA
//	                      its chain carries (else the system's CAs)
//	JANUS_ES_READ_USER    a user that may read logs-janus* (and refresh
//	JANUS_ES_READ_PASSWORD it): each document is then looked for
//
// A batch sent twice is stored once (each record has an ID), and the
// documents land in logs-janus.<source>-livetest.
func TestLiveElasticsearch(t *testing.T) {
	url := os.Getenv("JANUS_ES_URL")
	if url == "" {
		t.Skip("JANUS_ES_URL not set")
	}
	es := ElasticsearchTarget{
		APIKey:    os.Getenv("JANUS_ES_API_KEY"),
		Username:  os.Getenv("JANUS_ES_USER"),
		Password:  os.Getenv("JANUS_ES_PASSWORD"),
		Namespace: "livetest",
	}
	tg := esTarget(url, es)
	if f := os.Getenv("JANUS_ES_CA_FILE"); f != "" {
		pem, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		tg.TLS = tlstrust.Trust{CAPEM: string(pem)}
	}
	if fp := os.Getenv("JANUS_ES_FINGERPRINT"); fp != "" {
		tg.TLS = tlstrust.Trust{Fingerprint: fp}
	}
	sk := newESSink(t, tg, map[string]string{"test": "live"})
	defer sk.Close()

	run := fmt.Sprintf("live-%d", time.Now().UnixNano())
	at := time.Now().UTC()
	recs := []Record{
		{Time: at, Source: SourceJanusd, Service: "janusd", Severity: SevNotice, Message: "api: SystemService/Reboot: admin (os:admin) " + run, ID: run + "-1"},
		EventRecord(1, at, "logship.livetest", []byte(`{"run":"`+run+`"}`), "janusd"),
		{Time: at, Source: SourceAccess, Service: "haproxy", Severity: SevInfo, Message: `{"client_ip":"192.0.2.1","status":200,"path":"/` + run + `"}`},
	}
	recs[1].ID = run + "-2"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := sk.Send(ctx, recs); err != nil {
		t.Fatalf("send: %v", err)
	}
	// Again, as after a timeout: the copies are answered 409, stored once.
	if _, err := sk.Send(ctx, recs[:2]); err != nil {
		t.Fatalf("send again: %v", err)
	}

	user, pw := os.Getenv("JANUS_ES_READ_USER"), os.Getenv("JANUS_ES_READ_PASSWORD")
	if user == "" {
		t.Logf("sent as %s (JANUS_ES_READ_USER not set: not read back)", run)
		return
	}
	read := func(method, path, body string) map[string]any {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(url, "/")+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.SetBasicAuth(user, pw)
		req.Header.Set("Content-Type", "application/json")
		resp, err := sk.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s %s: %s %s", method, path, resp.Status, buf.String())
		}
		if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	for _, c := range []struct {
		stream, field, value string
	}{
		{"logs-janus.janusd-livetest", "message", run},
		{"logs-janus.events-livetest", "janus.payload", `{"run":"` + run + `"}`}, // a keyword: the whole payload
		{"logs-janus.access-livetest", "haproxy.path", "/" + run},
	} {
		read("POST", "/"+c.stream+"/_refresh", "")
		q := fmt.Sprintf(`{"query":{"match_phrase":{%q:%q}}}`, c.field, c.value)
		got := read("POST", "/"+c.stream+"/_search", q)
		hits, _ := got["hits"].(map[string]any)
		total, _ := hits["total"].(map[string]any)
		if n, _ := total["value"].(float64); n != 1 {
			t.Errorf("%s: %v documents for %s, want 1", c.stream, total["value"], c.value)
		}
	}
}
