package nodeproxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// fakeReleases serves body as GitHub's release list, and each asset at
// its path: "/sha256" a checksum, "/<name>.json" a security.json - counted
// in reads.
func fakeReleases(t *testing.T, reads *atomic.Int32, body func(base string) string) {
	t.Helper()
	var gh *httptest.Server
	gh = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases":
			if r.URL.Query().Get("per_page") != "100" {
				t.Errorf("releases asked for with %q", r.URL.RawQuery)
			}
			_, _ = io.WriteString(w, body(gh.URL))
		case "/sha256":
			_, _ = io.WriteString(w, strings.Repeat("ab", 32)+"\n")
		case "/stable.json":
			reads.Add(1)
			_, _ = io.WriteString(w, `{"updates":[{"name":"haproxy","target":"node","fixes":[{"id":"a","severity":"high"}]}]}`)
		case "/pre.json":
			reads.Add(1)
			_, _ = io.WriteString(w, `{"updates":[{"name":"linux","target":"node","fixes":[{"id":"b","severity":"critical"}]}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(gh.Close)
	prev := ReleasesURL
	ReleasesURL = gh.URL + "/releases"
	t.Cleanup(func() { ReleasesURL = prev })
}

// TestPrereleasesApart: the newest release is the newest that isn't a
// pre-release; the newest pre-release is offered apart when newer, and
// never counts in security updates.
func TestPrereleasesApart(t *testing.T) {
	var reads atomic.Int32
	fakeReleases(t, &reads, func(base string) string {
		rel := func(tag string, pre bool, sec string) string {
			assets := `{"name":"rootfs.squashfs.sha256","browser_download_url":"` + base + `/sha256"}`
			if sec != "" {
				assets += `,{"name":"security.json","browser_download_url":"` + base + `/` + sec + `.json"}`
			}
			p := "false"
			if pre {
				p = "true"
			}
			return `{"tag_name":"` + tag + `","html_url":"https://github.invalid/r/releases/tag/` + tag + `","prerelease":` + p + `,"assets":[` + assets + `]}`
		}
		return "[" + strings.Join([]string{
			rel("v2026.10.05", true, "pre"),
			rel("v2026.10.04", false, "stable"),
			rel("v2026.10.03-2", true, ""), // a pre-release never promoted
			rel("v2026.10.03", false, ""),
		}, ",") + "]"
	})

	rel, err := fetchLatestRelease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rel.TagName != "v2026.10.04" || strings.Join(rel.tags, " ") != "v2026.10.04 v2026.10.03" {
		t.Fatalf("release %s, tags %q", rel.TagName, rel.tags)
	}
	pre := rel.Prerelease
	if pre == nil || pre.TagName != "v2026.10.05" || pre.SHA256 != strings.Repeat("ab", 32) ||
		pre.BundleBaseURL != "https://github.invalid/r/releases/download/v2026.10.05" {
		t.Fatalf("pre-release = %+v", pre)
	}
	if n := reads.Load(); n != 1 {
		t.Errorf("security.json read %d times, want the release's only", n)
	}
	for v, want := range map[string]bool{"v2026.10.05": true, "v2026.10.03-2": true, "v2026.10.04": false, "dev": false} {
		if rel.IsPrerelease(v) != want {
			t.Errorf("IsPrerelease(%s) = %v", v, !want)
		}
	}
	for _, tc := range []struct{ version, severity, release string }{
		{"v2026.10.03", "high", "v2026.10.04"},   // the pre-release's critical fix doesn't count
		{"v2026.10.03-2", "high", "v2026.10.04"}, // on a pre-release: the releases after it
		{"v2026.10.04", "", ""},
		{"v2026.10.05", "", ""}, // newer than every release
	} {
		if sev, r := rel.SecurityUpdate(tc.version, "node", NodeImage{}); sev != tc.severity || r != tc.release {
			t.Errorf("SecurityUpdate(%s) = %q %q, want %q %q", tc.version, sev, r, tc.severity, tc.release)
		}
	}
	for v, want := range map[string]string{"v2026.10.03": "v2026.10.05", "v2026.10.04": "v2026.10.05", "v2026.10.05": "", "v2026.10.06": ""} {
		got := ""
		if p := rel.PrereleaseFor(v); p != nil {
			got = p.TagName
		}
		if got != want {
			t.Errorf("PrereleaseFor(%s) = %q, want %q", v, got, want)
		}
	}
}

// TestPrereleaseOlderThanRelease: a pre-release the newest release came
// after isn't offered at all.
func TestPrereleaseOlderThanRelease(t *testing.T) {
	var reads atomic.Int32
	fakeReleases(t, &reads, func(string) string {
		return `[{"tag_name":"v2026.10.06","html_url":"https://github.invalid/r/releases/tag/v2026.10.06"},
			{"tag_name":"v2026.10.05","html_url":"https://github.invalid/r/releases/tag/v2026.10.05","prerelease":true}]`
	})
	rel, err := fetchLatestRelease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rel.TagName != "v2026.10.06" || rel.Prerelease != nil || !rel.IsPrerelease("v2026.10.05") {
		t.Fatalf("release %s, pre-release %+v", rel.TagName, rel.Prerelease)
	}
}

func seedWithPrerelease(t *testing.T) {
	t.Helper()
	seedRelease(t, &ReleaseInfo{
		TagName: "v2026.10.04", BundleBaseURL: "https://github.invalid/download/v2026.10.04", SHA256: "stable",
		tags: []string{"v2026.10.04"}, security: map[string][]releaseFix{},
		prereleases: map[string]bool{"v2026.10.05": true},
		Prerelease: &ReleaseInfo{TagName: "v2026.10.05", HTMLURL: "https://github.invalid/r/releases/tag/v2026.10.05",
			BundleBaseURL: "https://github.invalid/download/v2026.10.05", SHA256: "pre"},
	})
}

// TestCheckUpdatePrerelease: the update check offers the pre-release
// apart, ready for the default schematic, to be prepared for another -
// and never as the update.
func TestCheckUpdatePrerelease(t *testing.T) {
	// The factory answers the update of the release; an image request
	// would start a build.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/schematics":
			_, _ = io.WriteString(w, `{"id":"`+nodeWith("qemu-guest-agent").GetSchematicId()+`"}`)
		case strings.HasPrefix(r.URL.Path, "/api/v1/updates/"):
			_, _ = io.WriteString(w, `{"version":"v2026.10.04","state":"ready","bundle_url":"https://f.invalid/b","sha256":"x"}`)
		default:
			t.Errorf("the image factory was asked %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	resetFactory(t, srv.URL)
	seedWithPrerelease(t)

	uc := checkUpdate(context.Background(), &janusv1alpha1.VersionResponse{Version: "v2026.10.04", Arch: "amd64"})
	if uc.UpdateAvailable || uc.RunningPrerelease || uc.Prerelease == nil {
		t.Fatalf("update check = %+v", uc)
	}
	if p := uc.Prerelease; p.Version != "v2026.10.05" || p.State != "ready" || p.BundleURL != "https://github.invalid/download/v2026.10.05" || p.SHA256 != "pre" {
		t.Errorf("pre-release = %+v", p)
	}

	// With extensions: the factory isn't asked to build it by a mere check.
	v := nodeWith("qemu-guest-agent")
	v.Version = "v2026.10.04"
	uc = checkUpdate(context.Background(), v)
	if p := uc.Prerelease; p == nil || p.State != "prepare" || p.BundleURL != "" {
		t.Errorf("pre-release with extensions = %+v", p)
	}

	// On the pre-release: nothing newer, and the release isn't an update.
	uc = checkUpdate(context.Background(), &janusv1alpha1.VersionResponse{Version: "v2026.10.05", Arch: "amd64"})
	if uc.UpdateAvailable || !uc.RunningPrerelease || uc.Prerelease != nil {
		t.Errorf("on the pre-release: %+v", uc)
	}
}

// TestPreparePrerelease: only the pre-release on offer is prepared; the
// default schematic's comes from its GitHub release.
func TestPreparePrerelease(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/releases/tags/v2026.10.05" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"assets":[{"name":"rootfs.squashfs","browser_download_url":"https://github.invalid/download/v2026.10.05/rootfs.squashfs",
			"size":3,"digest":"sha256:`+strings.Repeat("cd", 32)+`"}]}`)
	}))
	t.Cleanup(gh.Close)
	prev := ReleasesURL
	ReleasesURL = gh.URL + "/releases"
	t.Cleanup(func() { ReleasesURL = prev })
	seedWithPrerelease(t) // for this ReleasesURL

	node := &janusv1alpha1.VersionResponse{Version: "v2026.10.04", Arch: "amd64"}
	if _, err := preparePrerelease(context.Background(), node, "v2026.10.03"); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("another version: %v", err)
	}
	p, err := preparePrerelease(context.Background(), node, "v2026.10.05")
	if err != nil {
		t.Fatal(err)
	}
	if p.State != "ready" || p.BundleURL != "https://github.invalid/download/v2026.10.05" || p.SHA256 != strings.Repeat("cd", 32) {
		t.Errorf("prepared = %+v", p)
	}
	node.Version = "v2026.10.05"
	if _, err := preparePrerelease(context.Background(), node, "v2026.10.05"); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("on the pre-release already: %v", err)
	}
}
