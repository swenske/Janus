package main

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/swenske/Janus/internal/variants"
)

func TestVersionCompare(t *testing.T) {
	// A pre-release sorts before its release; "6.18" is "6.18.0".
	ordered := []string{"3.4-dev14", "3.4.0", "3.4.1", "3.4.10", "3.5-dev8", "6.18.0-rc1", "6.18", "6.18.1", "7.3-rc1", "7.3-rc2", "7.3-rc10", "7.3"}
	for i := range ordered {
		for j := range ordered {
			a, b := mustVersion(ordered[i]), mustVersion(ordered[j])
			want := 0
			if i < j {
				want = -1
			} else if i > j {
				want = 1
			}
			if got := a.compare(b); got != want {
				t.Errorf("compare(%s, %s) = %d, want %d", ordered[i], ordered[j], got, want)
			}
		}
	}
	if mustVersion("6.18").compare(mustVersion("6.18.0")) != 0 {
		t.Error("6.18 != 6.18.0")
	}
	if mustVersion("v1.53").compare(mustVersion("1.53")) != 0 {
		t.Error("v1.53 != 1.53")
	}
	if v := mustVersion("2.0.4+ent"); v.compare(mustVersion("2.0.4")) != 0 || v.prerelease() {
		t.Error("build metadata isn't ignored")
	}
	for _, bad := range []string{"test1", "227df8b99103f9c59f6570babf892978e293082f", "", "v", "1..2", "1.2-"} {
		if _, ok := parseVersion(bad); ok {
			t.Errorf("parseVersion(%q) accepted", bad)
		}
	}
	if b := mustVersion("6.18.55").branch(2); b != "6.18" {
		t.Errorf("branch = %q", b)
	}
	if s := sameStyle("v1.53", "1.54"); s != "v1.54" {
		t.Errorf("sameStyle = %q", s)
	}
	if s := sameStyle("1.3.1", "v1.3.2"); s != "1.3.2" {
		t.Errorf("sameStyle = %q", s)
	}
}

func TestVersionsMk(t *testing.T) {
	mk := []byte("# comment := not\nKERNEL_VERSION := 6.18.53\nKERNEL_SHA256  := aaaa\nHAPROXY_VERSION := 3.4.0\nNODE_EXPORTER_SHA256_amd64 := bbbb\n")
	vars := parseVersionsMk(mk)
	if vars["KERNEL_VERSION"] != "6.18.53" || vars["KERNEL_SHA256"] != "aaaa" || vars["NODE_EXPORTER_SHA256_amd64"] != "bbbb" {
		t.Fatalf("parse: %v", vars)
	}
	out, err := setVars(mk, map[string]string{"KERNEL_VERSION": "6.18.55", "KERNEL_SHA256": "cccc"})
	if err != nil {
		t.Fatal(err)
	}
	want := "# comment := not\nKERNEL_VERSION := 6.18.55\nKERNEL_SHA256  := cccc\nHAPROXY_VERSION := 3.4.0\nNODE_EXPORTER_SHA256_amd64 := bbbb\n"
	if string(out) != want {
		t.Errorf("setVars:\n%s\nwant:\n%s", out, want)
	}
	if _, err := setVars(mk, map[string]string{"NOPE": "1"}); err == nil {
		t.Error("setVars accepted a variable versions.mk doesn't assign")
	}
}

// TestComponentsCoverVersionsMk: every version versions.mk pins is
// followed, and every variable a component names is there - a new
// upstream can't be added without being tracked.
func TestComponentsCoverVersionsMk(t *testing.T) {
	data, err := os.ReadFile("../../versions.mk")
	if err != nil {
		t.Fatal(err)
	}
	vars := parseVersionsMk(data)
	used := map[string]bool{}
	for _, c := range components {
		if vars[c.versionVar] == "" {
			t.Errorf("%s: versions.mk has no %s", c.name, c.versionVar)
		}
		used[c.versionVar] = true
		for arch, v := range c.sumVars {
			if vars[v] == "" {
				t.Errorf("%s: versions.mk has no %s", c.name, v)
			}
			used[v] = true
			if c.url == nil {
				t.Errorf("%s: a checksum for %q but no URL", c.name, arch)
			}
		}
		if c.manual == "" && len(c.sumVars) > 0 && len(c.checks) == 0 && len(c.crossChecks) == 0 {
			t.Errorf("%s: no way to check a download", c.name)
		}
		if _, ok := c.feed.(githubCommit); !ok {
			if _, ok := parseVersion(vars[c.versionVar]); !ok {
				t.Errorf("%s: %s = %q isn't a version", c.name, c.versionVar, vars[c.versionVar])
			}
		}
	}
	for name := range vars {
		if !used[name] {
			t.Errorf("versions.mk's %s isn't followed by any component (hack/upstream/components.go)", name)
		}
	}
}

// TestSigningKeys: every key a check names is in keys/, under its own
// fingerprint.
func TestSigningKeys(t *testing.T) {
	keysDir = "keys"
	want := map[string]bool{}
	for _, c := range components {
		for _, ch := range append(slices.Clone(c.checks), c.crossChecks...) {
			switch x := ch.(type) {
			case gpgSig:
				for _, k := range x.keys {
					want[k] = true
				}
			case signedSums:
				for _, k := range x.keys {
					want[k] = true
				}
			}
		}
	}
	for k := range want {
		data, err := os.ReadFile(filepath.Join("keys", k+".asc"))
		if err != nil {
			t.Errorf("key %s: %v", k, err)
			continue
		}
		if !strings.Contains(string(data), "BEGIN PGP PUBLIC KEY BLOCK") {
			t.Errorf("keys/%s.asc isn't an armored public key", k)
		}
	}
	files, _ := filepath.Glob("keys/*.asc")
	for _, f := range files {
		if !want[strings.TrimSuffix(filepath.Base(f), ".asc")] {
			t.Errorf("%s isn't used by any check", f)
		}
	}
}

func TestNewestVersions(t *testing.T) {
	// A kernel track's feed lists its moniker's releases only.
	linux := findComponent("linux-longterm")
	longterm := []string{"6.18.55", "6.12.112", "6.6.158"}
	if l, n := newestVersions(linux, "6.18.53", longterm); l != "6.18.55" || n != "" {
		t.Errorf("linux: %q, %q", l, n)
	}
	if l, n := newestVersions(linux, "6.18.55", longterm); l != "" || n != "" {
		t.Errorf("linux newest: %q, %q", l, n)
	}
	// A newer branch from its x.y.2 on.
	if l, n := newestVersions(linux, "7.2.9", []string{"7.3.1", "7.2.10"}); l != "7.2.10" || n != "7.3.1" {
		t.Errorf("linux, new branch at .1: %q, %q", l, n)
	}
	if l, _ := newestVersions(linux, "7.2.10", []string{"7.3.2", "7.2.10"}); l != "7.3.2" {
		t.Errorf("linux, new branch at .2: %q", l)
	}
	bird := findComponent("bird")
	if l, n := newestVersions(bird, "2.19.2", []string{"2.19.2", "3.1.8", "3.3.2", "2.19.1"}); l != "" || n != "3.3.2" {
		t.Errorf("bird: %q, %q", l, n)
	}
	rpi5 := findComponent("rpi5-uefi")
	if l, _ := newestVersions(rpi5, "v0.1", []string{"test1", "v0.1", "v0.2-rc1"}); l != "v0.2-rc1" {
		t.Errorf("rpi5 follows pre-releases: %q", l)
	}
	tofu := findComponent("opentofu")
	if l, n := newestVersions(tofu, "1.13.1", []string{"v1.14.0-rc1", "v1.13.1", "v1.12.7", "v2.0.0"}); l != "" || n != "v2.0.0" {
		t.Errorf("opentofu: %q, %q", l, n)
	}
	mcm := findComponent("musl-cross-make")
	if l, _ := newestVersions(mcm, "227df8b9", []string{"9f0f0d6a"}); l != "9f0f0d6a" {
		t.Errorf("commit: %q", l)
	}
}

func TestKernelCVEs(t *testing.T) {
	load := func(name string) kernelCVE {
		data, err := os.ReadFile("testdata/kernel/" + name + ".json")
		if err != nil {
			t.Fatal(err)
		}
		c, err := parseKernelCVE(data)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	for _, tc := range []struct {
		cve      string
		affected map[string]bool
	}{
		// Fixed in 6.18.52 on the 6.18 branch, in 7.3-rc1 upstream.
		{"CVE-2026-100070", map[string]bool{"6.18.51": true, "6.18.52": false, "6.18.55": false, "2.6.25": false, "6.12.109": true, "6.12.110": false, "7.2.5": true, "7.3": false}},
		// Introduced by a backport to 6.16.3, gone in 6.17: default unaffected.
		{"CVE-2026-100073", map[string]bool{"6.16.2": false, "6.16.3": true, "6.16.9": true, "6.17": false, "6.18.55": false}},
		// Fixed in 7.0, 6.19.7, 6.12.82 - never on 6.18.
		{"CVE-2026-23374", map[string]bool{"6.18.55": true, "6.19.7": false, "6.12.82": false, "6.12.81": true, "7.0": false, "3.7": false}},
	} {
		c := load(tc.cve)
		for v, want := range tc.affected {
			if got := c.affects(mustVersion(v)); got != want {
				t.Errorf("%s affects %s = %v, want %v", tc.cve, v, got, want)
			}
		}
	}
	k := &kernelCVEs{built: map[string]bool{"net/netfilter/nf_nat_sip.c": true, "include/linux/list.h": true, "include/kvm/arm_vgic.h": true}}
	if !k.applies(load("CVE-2026-100070")) || k.applies(load("CVE-2026-23374")) {
		t.Error("applies ignores the built files")
	}
	// A fix to unbuilt code doesn't apply for touching a header every
	// build reads; a header-only fix does.
	if k.applies(kernelCVE{Files: []string{"fs/netfs/misc.c", "include/linux/list.h"}}) {
		t.Error("a header made an unbuilt fix apply")
	}
	if !k.applies(kernelCVE{Files: []string{"include/kvm/arm_vgic.h"}}) {
		t.Error("a header-only fix to a built header doesn't apply")
	}
}

func TestHAProxyBugs(t *testing.T) {
	old, err := os.ReadFile("testdata/bugs-3.4.0.html")
	if err != nil {
		t.Fatal(err)
	}
	bugs := parseHAProxyBugs(string(old))
	count := map[string]int{}
	for _, b := range bugs {
		count[b.Severity]++
	}
	// The page's own totals: 258 = 0 critical, 6 major, 118 medium, 134 minor.
	if len(bugs) != 258 || count["high"] != 6 || count["medium"] != 118 || count["low"] != 134 {
		t.Errorf("bugs-3.4.0: %d bugs, %v", len(bugs), count)
	}
	for _, tc := range []struct{ subject, reason string }{
		{"BUG/MAJOR: mux_quic: fix potential crash on RESET_STREAM receive", "QUIC"},
		{"BUG/MAJOR: h3: reject H3 truncated frames", "QUIC"},
		{"BUG/MEDIUM: quic-be: something", ""},
		{"BUG/MINOR: hlua: leak", "Lua"},
		{"BUG/MAJOR: htx: Check the header/trailer length limits when one is updated", ""},
		{"BUG/MAJOR: ssl/ocsp: lock the OCSP response", ""},
		{"BUG/MINOR: no subsystem", ""},
	} {
		got := haproxyNotBuilt(tc.subject)
		if (tc.reason == "") != (got == "") || !strings.Contains(got, tc.reason) {
			t.Errorf("haproxyNotBuilt(%q) = %q, want about %q", tc.subject, got, tc.reason)
		}
	}
}

// fakeFetcher serves recorded answers by URL.
type fakeFetcher map[string]string

func (f fakeFetcher) get(_ context.Context, url string) ([]byte, error) {
	name, ok := f[url]
	if !ok {
		return nil, &httpError{url: url, status: 404}
	}
	if strings.HasPrefix(name, "=") {
		return []byte(name[1:]), nil
	}
	return os.ReadFile("testdata/" + name)
}

func (f fakeFetcher) post(ctx context.Context, url string, _ []byte) ([]byte, error) {
	return f.get(ctx, url)
}

func (f fakeFetcher) download(context.Context, string) (string, error) {
	return "", errors.New("no downloads in tests")
}

// TestHAProxyFixed: 3.4.0 -> 3.4.6 fixes six MAJOR bugs, three of them in
// QUIC/HTTP/3, which Janus's HAProxy is built without.
func TestHAProxyFixed(t *testing.T) {
	e := &env{ctx: context.Background(), f: fakeFetcher{
		"https://www.haproxy.org/bugs/bugs-3.4.0.html":                                        "bugs-3.4.0.html",
		"https://www.haproxy.org/bugs/bugs-3.4.6.html":                                        "bugs-3.4.6.html",
		"https://www.cisa.gov/sites/default/files/feeds/known_exploited_vulnerabilities.json": `={"vulnerabilities":[]}`,
	}}
	res := &bumpResult{}
	if err := fixedBetween(e, findComponent("haproxy-3.4"), "3.4.0", "3.4.6", res); err != nil {
		t.Fatal(err)
	}
	var fixed, skipped []string
	for _, v := range res.Fixed {
		fixed = append(fixed, v.Title)
	}
	for _, v := range res.Skipped {
		skipped = append(skipped, v.Title)
	}
	if len(fixed) != 3 || len(skipped) != 3 || res.MaxSeverity != "high" {
		t.Errorf("fixed %q, skipped %q, max %s", fixed, skipped, res.MaxSeverity)
	}
	for _, s := range skipped {
		if !regexp.MustCompile(`mux_quic|h3`).MatchString(s) {
			t.Errorf("skipped a fix that applies: %s", s)
		}
	}
	if res.FixedBugs == 0 {
		t.Error("no MEDIUM/MINOR fix counted")
	}
}

func TestInRange(t *testing.T) {
	for _, tc := range []struct {
		v, expr string
		want    bool
	}{
		{"1.65.0", ">= v1.61.0, < 1.71.0", true},
		{"1.71.0", ">= v1.61.0, < 1.71.0", false},
		{"5.11.0", " < v1.71.0", false},
		{"1.70.9", " < v1.71.0", true},
		{"3.0.0", "3.0.0", true},
		{"3.0.1", "3.0.0", false},
		{"3.1.0", ">= AWS-LC-FIPS-3.0.0, < AWS-LC-FIPS-3.2.0", false},
		{"1.0.0", "", false},
	} {
		if got := inRange(mustVersion(tc.v), tc.expr); got != tc.want {
			t.Errorf("inRange(%s, %q) = %v", tc.v, tc.expr, got)
		}
	}
}

func TestMatchSum(t *testing.T) {
	sum := "791e1815f8af6e8b850a227a9a0a190f3d3478c9e8d38a0f51c98b7f4bfe368b"
	if err := matchSum(sum+"  haproxy-3.4.6.tar.gz\n", "haproxy-3.4.6.tar.gz", sum); err != nil {
		t.Error(err)
	}
	if err := matchSum("x  a.tar.gz\n"+sum+" *b.tar.gz\n", "b.tar.gz", sum); err != nil {
		t.Error(err)
	}
	if err := matchSum(sum+"\n", "anything", sum); err != nil {
		t.Error("a file holding only the sum:", err)
	}
	if err := matchSum(sum+"  a.tar.gz\n", "a.tar.gz", strings.Repeat("0", 64)); err == nil {
		t.Error("a different sum matched")
	}
	if err := matchSum(sum+"  a.tar.gz\n", "b.tar.gz", sum); err == nil {
		t.Error("an unlisted file matched")
	}
}

func TestGoRequires(t *testing.T) {
	mod := []byte("module x\n\ngo 1.26.0\n\ntoolchain go1.26.8\n\nrequire github.com/a/b v1.0.0\n\nrequire (\n\tgolang.org/x/net v0.50.0 // indirect\n\tgoogle.golang.org/grpc v1.80.0\n)\n\ntool golang.org/x/vuln/cmd/govulncheck\n")
	req := goRequires(mod)
	want := map[string]string{"toolchain": "go1.26.8", "github.com/a/b": "v1.0.0", "golang.org/x/net": "v0.50.0", "google.golang.org/grpc": "v1.80.0"}
	if fmt.Sprint(req) != fmt.Sprint(want) {
		t.Errorf("goRequires = %v", req)
	}
}

func TestNPMPackages(t *testing.T) {
	lock := []byte(`{"packages":{"":{"version":"0.0.0"},"node_modules/react":{"version":"19.1.0"},"node_modules/vite":{"version":"7.0.0","dev":true},"node_modules/@scope/pkg":{"version":"1.2.3"},"node_modules/a/node_modules/b":{"version":"2.0.0"}}}`)
	pkgs, err := npmPackages(lock)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"react": "19.1.0", "@scope/pkg": "1.2.3", "b": "2.0.0"}
	if fmt.Sprint(pkgs) != fmt.Sprint(want) {
		t.Errorf("npmPackages = %v", pkgs)
	}
}

// TestDocsListEveryComponent: docs/upstreams.md's table names every
// component.
func TestDocsListEveryComponent(t *testing.T) {
	doc, err := os.ReadFile("../../docs/upstreams.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range components {
		if !strings.Contains(string(doc), "`"+c.name+"`") {
			t.Errorf("docs/upstreams.md doesn't list `%s`", c.name)
		}
	}
}

func TestAdvisory(t *testing.T) {
	d := &securityDoc{Version: "v2026.10.10", Previous: "v2026.10.03-4", MaxSeverity: "medium", Updates: []updateRecord{
		{Name: "haproxy", Title: "HAProxy", Target: "node", From: "3.4.0", To: "3.4.6",
			Fixes: []vuln{{ID: "haproxy-79abd43", Title: "BUG/MAJOR: htx: ...", Severity: "medium"}}},
		{Name: "jansson", Title: "Jansson", Target: "node", From: "2.15.1", To: "2.15.2"},
	}}
	if needsAdvisory(d) {
		t.Error("a medium release gets an advisory")
	}
	d.MaxSeverity, d.Updates[0].Fixes[0].Severity = "high", "high"
	if !needsAdvisory(d) {
		t.Error("a high release gets no advisory")
	}
	if needsAdvisory(&securityDoc{MaxSeverity: "none"}) {
		t.Error("a release fixing nothing gets an advisory")
	}
	a := advisory(d, "https://github.com/swenske/Janus/releases/tag/v2026.10.10")
	if a["summary"] != "Janus before v2026.10.10 ships known vulnerabilities (HAProxy)" || a["severity"] != "high" {
		t.Errorf("advisory: %v", a)
	}
	desc := a["description"].(string)
	if !strings.Contains(desc, "haproxy-79abd43") || strings.Contains(desc, "Jansson") {
		t.Errorf("description:\n%s", desc)
	}
	v := a["vulnerabilities"].([]map[string]any)[0]
	if v["vulnerable_version_range"] != "< v2026.10.10" || v["patched_versions"] != "v2026.10.10" {
		t.Errorf("vulnerabilities: %v", v)
	}
}

// TestELFStripped: built normally, a Go binary keeps its symbols; with
// -ldflags=-s, not.
func TestELFStripped(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		ldflags  string
		stripped bool
	}{{"", false}, {"-s", true}} {
		out := filepath.Join(dir, "bin"+tc.ldflags)
		cmd := exec.Command("go", "build", "-ldflags="+tc.ldflags, "-o", out, src)
		cmd.Env = append(os.Environ(), "GOOS=linux", "GO111MODULE=off")
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go build: %v: %s", err, b)
		}
		if stripped, err := elfStripped(out); err != nil || stripped != tc.stripped {
			t.Errorf("elfStripped(-ldflags=%q) = %v, %v", tc.ldflags, stripped, err)
		}
	}
	if _, err := elfStripped("testdata/bugs-3.4.0.html"); err == nil {
		t.Error("a non-ELF file isn't an error")
	}
}

func TestGoModuleZip(t *testing.T) {
	if u := goModuleZip("github.com/prometheus/node_exporter")("1.12.1", ""); u != "https://proxy.golang.org/github.com/prometheus/node_exporter/@v/v1.12.1.zip" {
		t.Errorf("goModuleZip = %s", u)
	}
}

// TestExtractTar: a binary comes out of an extension's plain tar.
func TestExtractTar(t *testing.T) {
	path := filepath.Join(t.TempDir(), "extension-x-amd64.tar")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	for name, body := range map[string]string{"usr/local/sbin/x": "binary", ".janus-labels": "labels"} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := extract(path, "usr/local/sbin/x")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(out)
	if b, _ := os.ReadFile(out); string(b) != "binary" {
		t.Errorf("extracted %q", b)
	}
	if _, err := extract(path, "usr/local/sbin/missing"); err == nil {
		t.Error("a missing member was extracted")
	}
	if l := (shipped{version: "v1.12.1", goVersion: "go1.26.8"}).label(); l != "1.12.1 (Go 1.26.8)" {
		t.Errorf("label = %q", l)
	}
}

// TestExtensionComponents: every extension component names an extension
// that exists, and only those do.
func TestExtensionComponents(t *testing.T) {
	for _, c := range components {
		if (c.kind == kindExtension) != (c.extension != "") {
			t.Errorf("%s: kind %s, extension %q", c.name, c.kind, c.extension)
		}
		if c.extension != "" {
			if _, err := os.Stat("../../extensions/" + c.extension + "/manifest.json"); err != nil {
				t.Errorf("%s: extension %s: %v", c.name, c.extension, err)
			}
		}
		if c.shipped != "" && c.extension == "" {
			t.Errorf("%s: a shipped binary but no extension", c.name)
		}
	}
	u := updateRecord{Target: "node", Extension: "prometheus-node-exporter"}
	if w := whereText(u); w != "nodes with the prometheus-node-exporter extension" {
		t.Errorf("whereText = %q", w)
	}
}

// TestComponentsCoverVariants: every HAProxy branch and kernel track an
// image can be built with (variants.mk) is followed by its own component,
// and no component follows a variant variants.mk doesn't offer.
func TestComponentsCoverVariants(t *testing.T) {
	set, err := variants.Load("../..")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{}
	for _, v := range set.All() {
		want[v.Component+" "+v.Name] = true
		c := findComponent(map[string]string{"haproxy": "haproxy-", "kernel": "linux-"}[v.Component] + v.Name)
		if c == nil || c.variantComponent != v.Component || c.variant != v.Name {
			t.Errorf("%s %s: no component follows it", v.Component, v.Name)
			continue
		}
		if c.versionVar != variants.VersionVar(v.Component, v.Name) {
			t.Errorf("%s: pinned by %s, variants.mk reads %s", c.name, c.versionVar, variants.VersionVar(v.Component, v.Name))
		}
	}
	for _, c := range components {
		if c.variant != "" && !want[c.variantComponent+" "+c.variant] {
			t.Errorf("%s follows %s %s, which variants.mk doesn't offer", c.name, c.variantComponent, c.variant)
		}
	}
}

// TestPinOfLegacy: a release from before variants pinned KERNEL_VERSION
// and HAPROXY_VERSION - security-notes reads them as the variants they
// were.
func TestPinOfLegacy(t *testing.T) {
	old := parseVersionsMk([]byte("KERNEL_VERSION := 6.18.53\nHAPROXY_VERSION := 3.4.0\n"))
	if got := pinOf(findComponent("linux-longterm"), old); got != "6.18.53" {
		t.Errorf("linux-longterm: %q", got)
	}
	if got := pinOf(findComponent("haproxy-3.4"), old); got != "3.4.0" {
		t.Errorf("haproxy-3.4: %q", got)
	}
	other := haproxyBranch("3.2", "HAPROXY_VERSION")
	if got := pinOf(other, old); got != "" {
		t.Errorf("haproxy-3.2 read 3.4's legacy pin: %q", got)
	}
	cur := parseVersionsMk([]byte("KERNEL_LONGTERM_VERSION := 6.18.55\nKERNEL_VERSION := 1.0.0\n"))
	if got := pinOf(findComponent("linux-longterm"), cur); got != "6.18.55" {
		t.Errorf("the current pin first: %q", got)
	}
}

// TestNewLTS: a maintained LTS branch newer than every offered one is
// noted on the newest offered branch, once.
func TestNewLTS(t *testing.T) {
	sts := make([]status, len(components))
	newest := -1
	for i, c := range components {
		if c.variantComponent == "haproxy" {
			sts[i] = status{Name: c.name, Support: &support{Others: []string{"3.6 (LTS)", "3.5", "2.6 (LTS)"}}}
			if newest < 0 {
				newest = i // variants.mk's order: the newest first
			}
		}
	}
	newLTS(sts)
	if len(sts[newest].Notices) != 1 || !strings.Contains(sts[newest].Notices[0], "HAProxy 3.6 is a new LTS branch") {
		t.Fatalf("notices: %v", sts[newest].Notices)
	}
	for i := range sts {
		if i != newest && len(sts[i].Notices) > 0 {
			t.Errorf("%s noted too: %v", sts[i].Name, sts[i].Notices)
		}
	}
}

// TestBuiltFilesPerTrack: each kernel track has its own lists, and a
// release from before tracks is read through its single one.
func TestBuiltFilesPerTrack(t *testing.T) {
	t.Chdir("../..")
	set, err := variants.Load(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range set.Kernel {
		built, err := builtFilesAt("", k.Name)
		if err != nil || len(built) < 1000 {
			t.Errorf("%s: %d files, %v - make kernel-built-files", k.Name, len(built), err)
		}
	}
	// v2026.10.06 had kernel/built-files-{amd64,arm64}.txt only.
	if err := exec.Command("git", "cat-file", "-e", "v2026.10.06:kernel/built-files-amd64.txt").Run(); err != nil {
		t.Skip("no v2026.10.06 tag in this clone")
	}
	old, err := builtFiles("v2026.10.06", "longterm")
	if err != nil || len(old) < 1000 {
		t.Errorf("v2026.10.06's lists: %d files, %v", len(old), err)
	}
}

