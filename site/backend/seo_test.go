package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestSPARoutes(t *testing.T) {
	files, err := loadStatic(fstest.MapFS{
		"index.html":           {Data: []byte("<!doctype html><title>Janus</title>" + strings.Repeat("<p>x</p>", 200))},
		"assets/index-Ab12.js": {Data: []byte("console.log(1)")},
		"favicon.svg":          {Data: []byte("<svg/>")},
	})
	if err != nil {
		t.Fatal(err)
	}
	h := spa(files, "https://janus.example")
	for _, c := range []struct {
		method, target string
		code           int
		cache          string
	}{
		{"GET", "/", 200, "no-cache"},
		{"GET", "/builder", 200, "no-cache"},
		{"GET", "/builder/kvm?x=1", 200, "no-cache"},
		{"GET", "/no-such-page", 404, "no-cache"},
		{"GET", "/robots.txt", 404, "no-cache"}, // seo.go answers it before
		{"GET", "/assets/index-Ab12.js", 200, "public, max-age=31536000, immutable"},
		{"GET", "/favicon.svg", 200, "public, max-age=3600"},
		{"GET", "/index.html", 301, ""},
		{"HEAD", "/", 200, "no-cache"},
		{"POST", "/", 405, ""},
	} {
		rec := docsGet(t, h, c.method, c.target, nil)
		if rec.Code != c.code || rec.Header().Get("Cache-Control") != c.cache {
			t.Errorf("%s %s: %d %q, want %d %q", c.method, c.target, rec.Code, rec.Header().Get("Cache-Control"), c.code, c.cache)
		}
	}
	// The 404 still carries the app: it shows its own "not found" page.
	if rec := docsGet(t, h, "GET", "/no-such-page", nil); !strings.Contains(rec.Body.String(), "<title>Janus</title>") {
		t.Errorf("404 body: %.60q", rec.Body.String())
	}
}

func TestRobotsAndSitemap(t *testing.T) {
	a := &app{publicURL: "https://janus.example"}
	for _, withDocs := range []bool{false, true} {
		docs := &docsSite{}
		if withDocs {
			docs.latest = &docsChannel{}
		}
		mux := http.NewServeMux()
		a.seoRoutes(mux, docs)
		robots := docsGet(t, mux, "GET", "/robots.txt", nil).Body.String()
		for _, want := range []string{"Disallow: /api/\n", "Disallow: /image/\n", "Sitemap: https://janus.example/sitemap.xml\n"} {
			if !strings.Contains(robots, want) {
				t.Errorf("robots.txt lacks %q:\n%s", want, robots)
			}
		}
		if got := strings.Contains(robots, "Sitemap: https://janus.example/docs/sitemap-index.xml\n"); got != withDocs {
			t.Errorf("docs built %v, docs sitemap listed %v:\n%s", withDocs, got, robots)
		}
		if strings.Contains(robots, "next") {
			t.Errorf("robots.txt names /docs/next/ - its pages say noindex themselves:\n%s", robots)
		}
	}
	mux := http.NewServeMux()
	a.seoRoutes(mux, &docsSite{})
	rec := docsGet(t, mux, "GET", "/sitemap.xml", nil)
	body := rec.Body.String()
	for _, want := range []string{`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">`, "<loc>https://janus.example/</loc>", "<loc>https://janus.example/builder</loc>"} {
		if !strings.Contains(body, want) {
			t.Errorf("sitemap.xml lacks %q:\n%s", want, body)
		}
	}
	if rec.Header().Get("Content-Type") != "application/xml; charset=utf-8" {
		t.Errorf("sitemap content type %q", rec.Header().Get("Content-Type"))
	}
}

// The site's own pages tell link previews and search engines what they
// are - index.html doesn't: each gets its title, description, canonical
// link, OpenGraph card and JSON-LD; anything else none of it.
func TestSPAPagesMeta(t *testing.T) {
	files, err := loadStatic(fstest.MapFS{
		"index.html": {Data: []byte(`<!doctype html><html><head><title>Janus</title><meta name="description" content="generic" /></head><body></body></html>`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	h := spa(files, "https://janus.example")
	for _, c := range []struct {
		target, title, canonical, card, ld string
	}{
		{"/", "Janus - an immutable Linux for HAProxy load balancers", "https://janus.example/", "https://janus.example/og/janus.png", `"@type":"SoftwareApplication"`},
		{"/builder?arch=amd64", "Image builder - Janus", "https://janus.example/builder", "https://janus.example/og/builder.png", `"@type":"WebApplication"`},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", c.target, nil))
		body := rec.Body.String()
		for _, want := range []string{
			"<title>" + c.title + "</title>",
			`<link rel="canonical" href="` + c.canonical + `" />`,
			`<meta property="og:image" content="` + c.card + `" />`,
			`<meta name="twitter:card" content="summary_large_image" />`,
			c.ld,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: no %s in\n%s", c.target, want, body)
			}
		}
		if strings.Contains(body, `content="generic"`) || strings.Count(body, "<title>") != 1 {
			t.Errorf("%s: the generic title or description left:\n%s", c.target, body)
		}
		start := strings.Index(body, `<script type="application/ld+json">`)
		end := strings.Index(body[start:], "</script>")
		var ld map[string]any
		if err := json.Unmarshal([]byte(body[start+len(`<script type="application/ld+json">`):start+end]), &ld); err != nil {
			t.Errorf("%s: JSON-LD: %v", c.target, err)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/nope", nil))
	if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "canonical") {
		t.Errorf("/nope: %d, %s", rec.Code, rec.Body)
	}
}
