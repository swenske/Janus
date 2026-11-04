package nodeproxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// TestMaintenanceRelease: a maintenance release - a fix of the previous
// release, tagged on its own branch after the newest release, so GitHub
// lists it first (by its commit's date) - is never taken for the latest,
// a fix it carries isn't counted missing on its nodes when a newer
// release lists it again, and it is offered to the nodes on the release
// it fixes - only them (docs/contributing/releasing.md).
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
		if r.URL.Path == "/sha256" {
			_, _ = io.WriteString(w, strings.Repeat("cd", 32)+"\n")
			return
		}
		if r.URL.Path != "/releases" {
			if body, ok := security[r.URL.Path]; ok {
				_, _ = io.WriteString(w, body)
				return
			}
			http.NotFound(w, r)
			return
		}
		rel := func(tag, name string) string {
			assets := `{"name":"rootfs.squashfs.sha256","browser_download_url":"` + gh.URL + `/sha256"}`
			if _, ok := security["/"+tag+".json"]; ok {
				assets += `,{"name":"security.json","browser_download_url":"` + gh.URL + `/` + tag + `.json"}`
			}
			return `{"tag_name":"` + tag + `","name":"` + name + `","html_url":"https://github.invalid/r/releases/tag/` + tag + `","assets":[` + assets + `]}`
		}
		// GitHub's order: by the date of each tag's commit. v2026.12.20-2
		// is a second release of its day, not a maintenance release.
		_, _ = io.WriteString(w, "["+strings.Join([]string{
			rel("v2027.01.04-3", "Janus v2027.01.04-3 (Alpha) - maintenance of v2027.01.04 - 🔒 security update"),
			rel("v2027.01.12", "Janus v2027.01.12 (Alpha) - 🔒 security update"),
			rel("v2027.01.10", "Janus v2027.01.10 (Alpha)"),
			rel("v2027.01.04", "Janus v2027.01.04 (Alpha)"),
			rel("v2026.12.20-2", "Janus v2026.12.20-2 (Alpha)"),
			rel("v2026.12.20", "Janus v2026.12.20 (Alpha)"),
		}, ",")+"]")
	}))
	t.Cleanup(gh.Close)
	prev := ReleasesURL
	ReleasesURL = gh.URL + "/releases"
	t.Cleanup(func() { ReleasesURL = prev })

	rel, err := fetchLatestRelease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rel.TagName != "v2027.01.12" || strings.Join(rel.tags, " ") != "v2027.01.12 v2027.01.10 v2027.01.04-3 v2027.01.04 v2026.12.20-2 v2026.12.20" {
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
	// Offered to the nodes on the release it fixes, never to another's -
	// nor to a development build.
	for v, want := range map[string]string{"v2027.01.04": "v2027.01.04-3", "v2027.01.04-3": "", "v2027.01.10": "", "v2027.01.12": "", "v2026.12.20": "", "v2027.01.04-2-gabcdef0": ""} {
		got := ""
		if m := rel.MaintenanceFor(v); m != nil {
			got = m.TagName
			if m.MaintenanceOf != "v2027.01.04" || m.SHA256 != strings.Repeat("cd", 32) || m.BundleBaseURL != "https://github.invalid/r/releases/download/v2027.01.04-3" {
				t.Errorf("MaintenanceFor(%s) = %+v", v, m)
			}
		}
		if got != want {
			t.Errorf("MaintenanceFor(%s) = %q, want %q", v, got, want)
		}
	}
}

// TestCheckUpdateMaintenance: a node on the release a maintenance release
// fixes is offered it beside the update - ready for the default
// schematic, to be prepared (POST /api/factory/prerelease) for another;
// the update stays the newest release.
func TestCheckUpdateMaintenance(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/releases/tags/v2027.01.04-3" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"assets":[{"name":"rootfs.squashfs","browser_download_url":"https://github.invalid/download/v2027.01.04-3/rootfs.squashfs",
			"size":3,"digest":"sha256:`+strings.Repeat("ef", 32)+`"}]}`)
	}))
	t.Cleanup(gh.Close)
	prev := ReleasesURL
	ReleasesURL = gh.URL + "/releases"
	t.Cleanup(func() { ReleasesURL = prev })
	seedRelease(t, &ReleaseInfo{
		TagName: "v2027.01.12", BundleBaseURL: "https://github.invalid/download/v2027.01.12", SHA256: "newest",
		tags: []string{"v2027.01.12", "v2027.01.10", "v2027.01.04-3", "v2027.01.04"}, security: map[string][]releaseFix{},
		maintenance: []*ReleaseInfo{{TagName: "v2027.01.04-3", MaintenanceOf: "v2027.01.04", HTMLURL: "https://github.invalid/r/releases/tag/v2027.01.04-3",
			BundleBaseURL: "https://github.invalid/download/v2027.01.04-3", SHA256: "maintenance"}},
	})

	node := &janusv1alpha1.VersionResponse{Version: "v2027.01.04", Arch: "amd64"}
	uc := checkUpdate(context.Background(), node)
	if !uc.UpdateAvailable || uc.Latest != "v2027.01.12" {
		t.Errorf("the update: %+v", uc)
	}
	if m := uc.Maintenance; m == nil || m.Version != "v2027.01.04-3" || m.MaintenanceOf != "v2027.01.04" || m.State != "ready" ||
		m.BundleURL != "https://github.invalid/download/v2027.01.04-3" || m.SHA256 != "maintenance" {
		t.Errorf("maintenance offer = %+v", uc.Maintenance)
	}
	for _, v := range []string{"v2027.01.10", "v2027.01.04-3"} {
		if uc := checkUpdate(context.Background(), &janusv1alpha1.VersionResponse{Version: v, Arch: "amd64"}); uc.Maintenance != nil {
			t.Errorf("on %s: offered %+v", v, uc.Maintenance)
		}
	}

	// Prepared for the node it's offered to, only.
	p, err := preparePrerelease(context.Background(), node, "v2027.01.04-3")
	if err != nil {
		t.Fatal(err)
	}
	if p.State != "ready" || p.MaintenanceOf != "v2027.01.04" || p.BundleURL != "https://github.invalid/download/v2027.01.04-3" || p.SHA256 != strings.Repeat("ef", 32) {
		t.Errorf("prepared = %+v", p)
	}
	other := &janusv1alpha1.VersionResponse{Version: "v2027.01.10", Arch: "amd64"}
	if _, err := preparePrerelease(context.Background(), other, "v2027.01.04-3"); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("prepared for a node on another release: %v", err)
	}
}
