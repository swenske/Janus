package nodeproxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestMaintenanceRelease: a maintenance release - a fix of the previous
// release, tagged on its own branch after the newest release, so GitHub
// lists it first (by its commit's date) - is never taken for the latest,
// and a fix it carries isn't counted missing on its nodes when a newer
// release lists it again (docs/contributing/releasing.md).
func TestMaintenanceRelease(t *testing.T) {
	security := map[string]string{
		// The newest release: the fix X, found after v2027.01.10 shipped.
		"/v2027.01.12.json": `{"updates":[{"name":"janus","target":"node","first_party":true,"fixes":[{"id":"JANUS-2027-001","severity":"high"}]}]}`,
		// The maintenance release of v2027.01.04: X too.
		"/v2027.01.04-3.json": `{"updates":[{"name":"janus","target":"node","first_party":true,"fixes":[{"id":"JANUS-2027-001","severity":"high"}]}]}`,
		// The release after v2027.01.04: a fix the maintenance release
		// doesn't carry.
		"/v2027.01.10.json": `{"updates":[{"name":"haproxy","target":"node","fixes":[{"id":"CVE-2027-0002","severity":"medium"}]}]}`,
	}
	var gh *httptest.Server
	gh = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/releases" {
			if body, ok := security[r.URL.Path]; ok {
				_, _ = io.WriteString(w, body)
				return
			}
			http.NotFound(w, r)
			return
		}
		rel := func(tag string) string {
			assets := ""
			if _, ok := security["/"+tag+".json"]; ok {
				assets = `{"name":"security.json","browser_download_url":"` + gh.URL + `/` + tag + `.json"}`
			}
			return `{"tag_name":"` + tag + `","html_url":"https://github.invalid/r/releases/tag/` + tag + `","assets":[` + assets + `]}`
		}
		// GitHub's order: by the date of each tag's commit.
		_, _ = io.WriteString(w, "["+strings.Join([]string{rel("v2027.01.04-3"), rel("v2027.01.12"), rel("v2027.01.10"), rel("v2027.01.04")}, ",")+"]")
	}))
	t.Cleanup(gh.Close)
	prev := ReleasesURL
	ReleasesURL = gh.URL + "/releases"
	t.Cleanup(func() { ReleasesURL = prev })

	rel, err := fetchLatestRelease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rel.TagName != "v2027.01.12" || strings.Join(rel.tags, " ") != "v2027.01.12 v2027.01.10 v2027.01.04-3 v2027.01.04" {
		t.Fatalf("release %s, tags %q", rel.TagName, rel.tags)
	}
	for _, tc := range []struct{ version, severity, release string }{
		{"v2027.01.04", "high", "v2027.01.12"},
		{"v2027.01.04-3", "medium", "v2027.01.10"}, // X already there
		{"v2027.01.10", "high", "v2027.01.12"},     // the maintenance release is older
		{"v2027.01.12", "", ""},
	} {
		if sev, r := rel.SecurityUpdate(tc.version, "node", NodeImage{}); sev != tc.severity || r != tc.release {
			t.Errorf("SecurityUpdate(%s) = %q %q, want %q %q", tc.version, sev, r, tc.severity, tc.release)
		}
	}
}
