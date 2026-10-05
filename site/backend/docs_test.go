package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

const themeScript = `document.documentElement.dataset.theme = localStorage.getItem('t') ?? 'auto'`

func scriptHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}

func docsFixture(t *testing.T) *docsSite {
	t.Helper()
	page := func(title string) *fstest.MapFile {
		return &fstest.MapFile{Data: []byte(`<!doctype html><html><head><title>` + title + `</title>` +
			`<script>` + themeScript + `</script>` +
			`<script type="module" src="/docs/_astro/page.js"></script>` +
			`<script type="application/ld+json">{"@type":"TechArticle"}</script>` +
			`</head><body>` + strings.Repeat("<p>Janus docs.</p>", 100) + `</body></html>`)}
	}
	fsys := fstest.MapFS{
		"docsdist/README.md":                                       {Data: []byte("placeholder")},
		"docsdist/latest/index.html":                               page("Home"),
		"docsdist/latest/guide/vrrp/index.html":                    page("VRRP"),
		"docsdist/latest/404.html":                                 page("Not found"),
		"docsdist/latest/_astro/page.Ab12Cd.js":                    {Data: []byte("console.log(1)")},
		"docsdist/latest/pagefind/fragment/en_621fd74.pf_fragment": {Data: []byte{1, 2, 3}},
		"docsdist/latest/pagefind/pagefind-entry.json":             {Data: []byte(`{}`)},
		"docsdist/latest/_janus/build.json":                        {Data: []byte(`{"channel":"latest","version":"v2026.10.05-5","commit":"abc"}`)},
		"docsdist/latest/_janus/redirects.json":                    {Data: []byte(`{"vrrp.md":"guide/vrrp/","dashboard/README.md":"guide/controller/"}`)},
		"docsdist/next/index.html":                                 page("Next home"),
		"docsdist/next/_janus/build.json":                          {Data: []byte(`{"channel":"next","version":"v2026.10.05-5-3-gdef"}`)},
	}
	site, err := loadDocs(fsys)
	if err != nil {
		t.Fatal(err)
	}
	return site
}

func docsGet(t *testing.T, h http.Handler, method, target string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestDocsRoutes(t *testing.T) {
	site := docsFixture(t)
	for _, c := range []struct {
		target, code, location string
	}{
		{"/docs", "301", "/docs/"},
		{"/docs/", "200", ""},
		{"/docs/guide/vrrp/", "200", ""},
		{"/docs/guide/vrrp", "301", "/docs/guide/vrrp/"},
		{"/docs/guide/vrrp?x=1", "301", "/docs/guide/vrrp/?x=1"},
		{"/docs/guide//vrrp/", "301", "/docs/guide/vrrp/"},
		{"/docs/guide/../guide/vrrp/", "301", "/docs/guide/vrrp/"},
		{"/docs/vrrp.md", "301", "/docs/guide/vrrp/"},
		{"/docs/dashboard/README.md", "301", "/docs/guide/controller/"},
		{"/docs/no-such-page/", "404", ""},
		{"/docs/guide/", "404", ""},
		{"/docs/_janus/build.json", "404", ""},
		{"/docs/_astro/page.Ab12Cd.js", "200", ""},
		{"/docs/next", "301", "/docs/next/"},
		{"/docs/next/", "200", ""},
		{"/docs/next/vrrp.md", "404", ""},
	} {
		rec := docsGet(t, site, "GET", c.target, nil)
		if got := rec.Result().Status[:3]; got != c.code {
			t.Errorf("GET %s: %s, want %s", c.target, got, c.code)
		}
		if loc := rec.Header().Get("Location"); loc != c.location {
			t.Errorf("GET %s: Location %q, want %q", c.target, loc, c.location)
		}
	}
	if rec := docsGet(t, site, "POST", "/docs/", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", rec.Code)
	}
	// The 404 is the docs' own page.
	if rec := docsGet(t, site, "GET", "/docs/nope/", nil); !strings.Contains(rec.Body.String(), "<title>Not found</title>") {
		t.Errorf("404 body: %.80q", rec.Body.String())
	}
}

func TestDocsHeaders(t *testing.T) {
	site := docsFixture(t)
	rec := docsGet(t, site, "GET", "/docs/guide/vrrp/", nil)
	h := rec.Header()
	csp := h.Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'self' 'wasm-unsafe-eval' "+scriptHash(themeScript)+";") {
		t.Errorf("an HTML page's CSP doesn't allow its inline script (and only it): %s", csp)
	}
	for _, d := range strings.Split(csp, ";") {
		if strings.HasPrefix(strings.TrimSpace(d), "script-src") && strings.Contains(d, "unsafe-inline") {
			t.Errorf("unsafe-inline scripts: %s", csp)
		}
	}
	if got := h.Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Errorf("Content-Type %q", got)
	}
	if h.Get("Cache-Control") != "no-cache" || h.Get("ETag") == "" {
		t.Errorf("HTML caching: %q, ETag %q", h.Get("Cache-Control"), h.Get("ETag"))
	}
	if h.Get("X-Janus-Docs-Version") != "v2026.10.05-5" || h.Get("X-Robots-Tag") != "" {
		t.Errorf("latest: version %q, robots %q", h.Get("X-Janus-Docs-Version"), h.Get("X-Robots-Tag"))
	}

	// Assets: immutable when hashed, and the hash-less policy (Pagefind's
	// worker takes the policy of its own script's response).
	rec = docsGet(t, site, "GET", "/docs/_astro/page.Ab12Cd.js", nil)
	if rec.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Errorf("_astro: %q", rec.Header().Get("Cache-Control"))
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self' 'wasm-unsafe-eval';") {
		t.Errorf("asset CSP: %s", csp)
	}
	if rec := docsGet(t, site, "GET", "/docs/pagefind/fragment/en_621fd74.pf_fragment", nil); rec.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Errorf("hashed pagefind file: %q", rec.Header().Get("Cache-Control"))
	}
	if rec := docsGet(t, site, "GET", "/docs/pagefind/pagefind-entry.json", nil); rec.Header().Get("Cache-Control") != "public, max-age=300" {
		t.Errorf("pagefind entry: %q", rec.Header().Get("Cache-Control"))
	}

	// next: noindex.
	rec = docsGet(t, site, "GET", "/docs/next/", nil)
	if rec.Header().Get("X-Robots-Tag") != "noindex" || rec.Header().Get("X-Janus-Docs-Version") != "v2026.10.05-5-3-gdef" {
		t.Errorf("next: robots %q, version %q", rec.Header().Get("X-Robots-Tag"), rec.Header().Get("X-Janus-Docs-Version"))
	}
	// The 404: its own policy, not cached.
	rec = docsGet(t, site, "GET", "/docs/nope/", nil)
	if !strings.Contains(rec.Header().Get("Content-Security-Policy"), scriptHash(themeScript)) || rec.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("404 headers: %v", rec.Header())
	}
}

func TestDocsGzipAndRevalidation(t *testing.T) {
	site := docsFixture(t)
	plain := docsGet(t, site, "GET", "/docs/", nil)
	if plain.Header().Get("Content-Encoding") != "" || plain.Header().Get("Vary") != "Accept-Encoding" {
		t.Fatalf("identity: encoding %q, vary %q", plain.Header().Get("Content-Encoding"), plain.Header().Get("Vary"))
	}
	gz := docsGet(t, site, "GET", "/docs/", map[string]string{"Accept-Encoding": "br, gzip"})
	if gz.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("gzip not used: %v", gz.Header())
	}
	zr, err := gzip.NewReader(gz.Body)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(zr)
	if !bytes.Equal(body, plain.Body.Bytes()) || gz.Body.Len() >= plain.Body.Len() {
		t.Errorf("gzipped body: %d bytes for %d", gz.Body.Len(), plain.Body.Len())
	}
	if gz.Header().Get("ETag") == plain.Header().Get("ETag") {
		t.Errorf("one ETag for both encodings: %s", gz.Header().Get("ETag"))
	}
	if rec := docsGet(t, site, "GET", "/docs/", map[string]string{"Accept-Encoding": "gzip;q=0"}); rec.Header().Get("Content-Encoding") != "" {
		t.Errorf("gzip;q=0 got gzip")
	}
	if rec := docsGet(t, site, "GET", "/docs/", map[string]string{"If-None-Match": plain.Header().Get("ETag")}); rec.Code != http.StatusNotModified {
		t.Errorf("If-None-Match: %d", rec.Code)
	}
	if rec := docsGet(t, site, "HEAD", "/docs/", nil); rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Errorf("HEAD: %d, %d bytes", rec.Code, rec.Body.Len())
	}
	// Small or binary files aren't compressed.
	if rec := docsGet(t, site, "GET", "/docs/pagefind/fragment/en_621fd74.pf_fragment", map[string]string{"Accept-Encoding": "gzip"}); rec.Header().Get("Content-Encoding") != "" {
		t.Errorf("a pagefind fragment was gzipped")
	}
}

func TestDocsNotBuilt(t *testing.T) {
	site, err := loadDocs(fstest.MapFS{"docsdist/README.md": {Data: []byte("placeholder")}})
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"/docs/", "/docs/next/", "/docs/vrrp.md"} {
		if rec := docsGet(t, site, "GET", target, nil); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s without docs: %d", target, rec.Code)
		}
	}
}

func TestInlineScriptHashes(t *testing.T) {
	page := []byte(`<script src="/a.js"></script><script>` + themeScript + `</script>` +
		`<script type="application/ld+json">{"a":1}</script><script type="module"></script>` +
		`<template><script>` + themeScript + `</script></template><p>&lt;script&gt;x()&lt;/script&gt;</p>`)
	got := inlineScriptHashes(page)
	want := []string{scriptHash(themeScript), scriptHash("")}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("hashes %v, want %v", got, want)
	}
	// A script's text is hashed raw: entities in it are not decoded.
	if got := inlineScriptHashes([]byte(`<script>a &amp;&amp; b</script>`)); got[0] != scriptHash("a &amp;&amp; b") {
		t.Errorf("entities decoded before hashing: %v", got)
	}
}

// The docs this binary embeds - whatever make docs-build left in
// docsdist/, or the placeholder alone - load.
func TestEmbeddedDocsLoad(t *testing.T) {
	if _, err := loadDocs(docsDist); err != nil {
		t.Fatal(err)
	}
}
