package nodeproxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestSecurityUpdate: a node is told about the vulnerabilities the
// releases after its own fix, from their security.json assets - each read
// once.
func TestSecurityUpdate(t *testing.T) {
	var reads atomic.Int32
	var gh *httptest.Server
	gh = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asset := func(name, path string) string {
			return `{"name":"` + name + `","browser_download_url":"` + gh.URL + path + `"}`
		}
		switch r.URL.Path {
		case "/releases":
			_, _ = io.WriteString(w, `[
				{"tag_name":"sec-v4","html_url":"https://github.invalid/r/releases/tag/sec-v4","assets":[`+asset("security.json", "/v4.json")+`]},
				{"tag_name":"sec-v3","html_url":"https://github.invalid/r/releases/tag/sec-v3","assets":[`+asset("security.json", "/v3.json")+`]},
				{"tag_name":"sec-draft","draft":true,"assets":[`+asset("security.json", "/missing.json")+`]},
				{"tag_name":"sec-v2","html_url":"https://github.invalid/r/releases/tag/sec-v2","assets":[`+asset("security.json", "/v2.json")+`]},
				{"tag_name":"sec-v1","html_url":"https://github.invalid/r/releases/tag/sec-v1","assets":[]}]`)
		case "/v4.json":
			reads.Add(1)
			_, _ = io.WriteString(w, `{"version":"sec-v4","updates":[
				{"name":"haproxy","target":"node","fixes":[{"id":"a","severity":"high"},{"id":"b","severity":"low"}]},
				{"name":"golang.org/x/net","target":"controller","fixes":[{"id":"c","severity":"medium"}]},
				{"name":"jansson","target":"node"}]}`)
		case "/v2.json":
			reads.Add(1)
			_, _ = io.WriteString(w, `{"version":"sec-v2","updates":[{"name":"linux","target":"node","fixes":[{"id":"d","severity":"critical"},{"id":"e"}]}]}`)
		case "/v3.json":
			reads.Add(1)
			_, _ = io.WriteString(w, `{"version":"sec-v3","updates":[{"name":"node-exporter","target":"node","extension":"prometheus-node-exporter","fixes":[{"id":"f","severity":"critical"}]}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(gh.Close)
	prevURL := ReleasesURL
	ReleasesURL = gh.URL + "/releases"
	t.Cleanup(func() { ReleasesURL = prevURL })

	rel, err := fetchLatestRelease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ne := []string{"prometheus-node-exporter"}
	for _, tc := range []struct {
		version, target   string
		extensions        []string
		severity, release string
	}{
		{"sec-v1", "node", nil, "critical", "sec-v4"},
		{"sec-v2", "node", nil, "high", "sec-v4"},
		// sec-v3's fix is node_exporter's: only for a node with it.
		{"sec-v2", "node", ne, "critical", "sec-v4"},
		{"sec-v3", "node", ne, "high", "sec-v4"},
		{"sec-v4", "node", ne, "", ""},
		{"sec-v1", "controller", nil, "medium", "sec-v4"},
		{"sec-v3", "client", nil, "", ""},
		{"v2026.10.03-4-3-gabcdef", "node", nil, "", ""}, // a development build
	} {
		sev, r := rel.SecurityUpdate(tc.version, tc.target, NodeImage{Extensions: tc.extensions})
		if sev != tc.severity || r != tc.release {
			t.Errorf("SecurityUpdate(%s, %s, %v) = %q, %q; want %q, %q", tc.version, tc.target, tc.extensions, sev, r, tc.severity, tc.release)
		}
	}
	if _, err := fetchLatestRelease(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := reads.Load(); n != 3 {
		t.Errorf("security.json read %d times, want each once (3)", n)
	}
}

// TestSecurityUpdateVariants: a fix to a HAProxy branch or kernel track
// reaches the nodes its audience names - those taking the default, or
// those pinned to it.
func TestSecurityUpdateVariants(t *testing.T) {
	rel := &ReleaseInfo{tags: []string{"v3", "v2", "v1"}, security: map[string][]releaseFix{
		"v3": {
			{target: "node", severity: "high", component: "haproxy", variant: "3.4", audience: []string{"default", "pinned"}},
			{target: "node", severity: "critical", component: "haproxy", variant: "3.2", audience: []string{"pinned"}},
		},
		"v2": {
			{target: "node", severity: "medium", component: "kernel", variant: "stable", audience: []string{"pinned"}},
			{target: "node", severity: "low", component: "kernel", variant: "longterm", audience: []string{"default"}},
		},
	}}
	for _, tc := range []struct {
		img               NodeImage
		severity, release string
	}{
		// The defaults: 3.4's fix, the default kernel's.
		{NodeImage{}, "high", "v3"},
		// Pinned to 3.2: its own fix, not 3.4's; the default kernel's.
		{NodeImage{HAProxy: "3.2"}, "critical", "v3"},
		// Pinned to 3.0: none of HAProxy's; the stable track's.
		{NodeImage{HAProxy: "3.0", Kernel: "stable"}, "medium", "v2"},
		// Pinned to longterm: the longterm record is for the default's
		// audience only.
		{NodeImage{HAProxy: "3.0", Kernel: "longterm"}, "", ""},
	} {
		sev, r := rel.SecurityUpdate("v1", "node", tc.img)
		if sev != tc.severity || r != tc.release {
			t.Errorf("%+v: %q %q, want %q %q", tc.img, sev, r, tc.severity, tc.release)
		}
	}
}
