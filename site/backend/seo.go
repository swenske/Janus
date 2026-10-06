package main

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html"
	"net/http"
	"regexp"
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

// spaPage is what one of the site's own pages (spaRoutes) tells search
// engines and link previews: index.html says none of it - the frontend
// renders its pages in the browser - so janus-site writes it into a copy
// of index.html per page (withMeta).
type spaPage struct {
	title, description string
	card, cardAlt      string // the OpenGraph card, under /og/ (make docs-og)
	jsonLD             map[string]any
}

func spaPages(publicURL string) map[string]spaPage {
	free := map[string]any{"@type": "Offer", "price": "0", "priceCurrency": "EUR"}
	return map[string]spaPage{
		"/": {
			title:       "Janus - an immutable Linux for HAProxy load balancers",
			description: "Janus is an immutable, API-driven Linux distribution built around HAProxy: no shell, no SSH, signed A/B updates, SELinux enforcing. Build your image with the extensions you need.",
			card:        "janus.png",
			cardAlt:     "Janus - an immutable Linux for your HAProxy load balancers",
			jsonLD: map[string]any{
				"@context":            "https://schema.org",
				"@type":               "SoftwareApplication",
				"name":                "Janus",
				"description":         "An immutable, API-driven Linux distribution for HAProxy load balancers: no shell, no SSH, signed A/B updates with rollback, SELinux enforcing - every change through an mTLS API.",
				"url":                 publicURL + "/",
				"image":               publicURL + "/og/janus.png",
				"applicationCategory": "Operating system",
				"license":             "https://opensource.org/licenses/MIT",
				"softwareHelp":        map[string]any{"@type": "CreativeWork", "url": publicURL + "/docs/"},
				"sameAs":              []string{"https://github.com/swenske/Janus"},
				"offers":              free,
			},
		},
		"/builder": {
			title:       "Image builder - Janus",
			description: "Build a Janus image with the extensions you need - Proxmox VE, KVM, VMware, an installer ISO or a Raspberry Pi - signed, and kept up to date with its own updates.",
			card:        "builder.png",
			cardAlt:     "Build your Janus image",
			jsonLD: map[string]any{
				"@context":            "https://schema.org",
				"@type":               "WebApplication",
				"name":                "Janus image builder",
				"url":                 publicURL + "/builder",
				"image":               publicURL + "/og/builder.png",
				"applicationCategory": "DeveloperApplication",
				"operatingSystem":     "Any - in a web browser",
				"offers":              free,
			},
		},
	}
}

var (
	titleTag       = regexp.MustCompile(`<title>[^<]*</title>`)
	descriptionTag = regexp.MustCompile(`<meta name="description" content="[^"]*"\s*/?>`)
)

// withMeta is index.html as route's page: its title and description, its
// canonical link, its OpenGraph and Twitter tags, its JSON-LD.
func withMeta(index []byte, route string, p spaPage, publicURL string) []byte {
	url := publicURL + route
	attr := html.EscapeString
	page := titleTag.ReplaceAllLiteralString(string(index), "<title>"+html.EscapeString(p.title)+"</title>")
	page = descriptionTag.ReplaceAllLiteralString(page, `<meta name="description" content="`+attr(p.description)+`" />`)
	ld, _ := json.Marshal(p.jsonLD) // HTML-safe: <, > and & are escaped
	var b strings.Builder
	fmt.Fprintf(&b, "<link rel=\"canonical\" href=\"%s\" />\n", attr(url))
	for _, m := range [][2]string{
		{"og:type", "website"}, {"og:site_name", "Janus"}, {"og:locale", "en"},
		{"og:title", p.title}, {"og:description", p.description}, {"og:url", url},
		{"og:image", publicURL + "/og/" + p.card}, {"og:image:width", "1200"}, {"og:image:height", "630"}, {"og:image:alt", p.cardAlt},
	} {
		fmt.Fprintf(&b, "    <meta property=\"%s\" content=\"%s\" />\n", m[0], attr(m[1]))
	}
	fmt.Fprintf(&b, "    <meta name=\"twitter:card\" content=\"summary_large_image\" />\n")
	fmt.Fprintf(&b, "    <script type=\"application/ld+json\">%s</script>\n  </head>", ld)
	return []byte(strings.Replace(page, "</head>", b.String(), 1))
}
