package main

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"strings"
)

// What search engines read: robots.txt - everything but the API and the
// downloads, and where the sitemaps are - and the site's own sitemap (the
// landing page and the image builder). The docs have theirs, built with
// them (/docs/sitemap-index.xml): robots.txt names both - one sitemap
// index can't list another.
func (a *app) seoRoutes(mux *http.ServeMux, docs *docsSite) {
	mux.HandleFunc("GET /robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		var b strings.Builder
		b.WriteString("User-agent: *\nDisallow: /api/\nDisallow: /image/\n\n")
		fmt.Fprintf(&b, "Sitemap: %s/sitemap.xml\n", a.publicURL)
		if docs.latest != nil {
			fmt.Fprintf(&b, "Sitemap: %s/docs/sitemap-index.xml\n", a.publicURL)
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		_, _ = w.Write([]byte(b.String()))
	})
	mux.HandleFunc("GET /sitemap.xml", func(w http.ResponseWriter, _ *http.Request) {
		type url struct {
			Loc string `xml:"loc"`
		}
		set := struct {
			XMLName xml.Name `xml:"urlset"`
			NS      string   `xml:"xmlns,attr"`
			URLs    []url    `xml:"url"`
		}{NS: "http://www.sitemaps.org/schemas/sitemap/0.9"}
		for _, route := range spaRoutes {
			set.URLs = append(set.URLs, url{a.publicURL + route})
		}
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		_, _ = w.Write([]byte(xml.Header))
		_ = xml.NewEncoder(w).Encode(set)
	})
}
