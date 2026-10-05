package main

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strings"

	"golang.org/x/net/html"
)

// The docs site (site/docs), built by hack/docs-build.sh into docsdist/
// before this binary: two channels - latest, the newest release's docs,
// at /docs/; next, main's, at /docs/next/ (noindex). A channel that
// wasn't built answers 404: a plain `go build` embeds only the
// placeholder README.
//
//go:embed all:docsdist
var docsDist embed.FS

// docsBuild is what a channel's build says about itself
// (_janus/build.json, written by site/docs/src/lib/janus-files.mjs).
type docsBuild struct {
	Channel       string `json:"channel"`
	Version       string `json:"version"`
	Commit        string `json:"commit"`
	Date          string `json:"date"`
	Ref           string `json:"ref"`
	LatestVersion string `json:"latestVersion"`
}

type docsChannel struct {
	prefix string // where it's served: /docs/ or /docs/next/
	files  staticSet
	// csp is each HTML page's Content-Security-Policy: its inline scripts
	// (Starlight's theme, Astro's islands) by hash, never 'unsafe-inline'.
	csp map[string]string
	// redirects maps a source file - "vrrp.md" for docs/vrrp.md,
	// "dashboard/README.md" - to its page ("guide/vrrp/"): /docs/vrrp.md
	// is a permalink, whatever the page's route becomes.
	redirects map[string]string
	build     docsBuild
	noindex   bool
}

type docsSite struct {
	latest, next *docsChannel // nil: not built into this binary
}

// docsCSP is the docs' policy - %s: the page's script hashes. Pagefind
// compiles its WebAssembly ('wasm-unsafe-eval'), in a worker which takes
// the policy of its own script's response: every docs response carries
// it. A directive is only ever added after a real violation.
const docsCSP = "default-src 'self'; script-src 'self' 'wasm-unsafe-eval'%s; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'"

func loadDocs(dist fs.FS) (*docsSite, error) {
	site := &docsSite{}
	for _, c := range []struct {
		name, prefix string
		into         **docsChannel
	}{{"docsdist/latest", "/docs/", &site.latest}, {"docsdist/next", "/docs/next/", &site.next}} {
		if _, err := fs.Stat(dist, c.name); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		sub, err := fs.Sub(dist, c.name)
		if err != nil {
			return nil, err
		}
		ch, err := loadDocsChannel(sub, c.prefix, c.prefix != "/docs/")
		if err != nil {
			return nil, fmt.Errorf("%s: %w", c.name, err)
		}
		*c.into = ch
	}
	return site, nil
}

func loadDocsChannel(fsys fs.FS, prefix string, noindex bool) (*docsChannel, error) {
	files, err := loadStatic(fsys)
	if err != nil {
		return nil, err
	}
	ch := &docsChannel{prefix: prefix, files: files, csp: map[string]string{}, redirects: map[string]string{}, noindex: noindex}
	if f := files["_janus/build.json"]; f != nil {
		if err := json.Unmarshal(f.body, &ch.build); err != nil {
			return nil, fmt.Errorf("_janus/build.json: %w", err)
		}
	}
	if f := files["_janus/redirects.json"]; f != nil {
		if err := json.Unmarshal(f.body, &ch.redirects); err != nil {
			return nil, fmt.Errorf("_janus/redirects.json: %w", err)
		}
	}
	for name, f := range files {
		switch {
		case strings.HasPrefix(name, "_janus/"):
			delete(files, name) // for this server, not for readers
		case strings.HasSuffix(name, ".html"):
			hashes := inlineScriptHashes(f.body)
			ch.csp[name] = fmt.Sprintf(docsCSP, strings.Join(append([]string{""}, hashes...), " "))
		}
	}
	return ch, nil
}

// inlineScriptHashes is the CSP source ('sha256-...') of every inline
// script of an HTML page, but data blocks (JSON-LD). An empty one counts:
// the browser checks it too.
func inlineScriptHashes(page []byte) []string {
	z := html.NewTokenizer(bytes.NewReader(page))
	var out []string
	seen := map[string]bool{}
	for {
		switch z.Next() {
		case html.ErrorToken:
			return out
		case html.StartTagToken:
			name, more := z.TagName()
			if string(name) != "script" {
				continue
			}
			inline := true
			for more {
				var k, v []byte
				k, v, more = z.TagAttr()
				switch string(k) {
				case "src":
					inline = false
				case "type":
					if strings.EqualFold(string(v), "application/ld+json") {
						inline = false
					}
				}
			}
			if !inline {
				continue
			}
			var text []byte
			if z.Next() == html.TextToken {
				text = z.Text() // raw: a script's text is never unescaped
			}
			sum := sha256.Sum256(text)
			h := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
			if !seen[h] {
				seen[h] = true
				out = append(out, h)
			}
		}
	}
}

func (s *docsSite) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p := r.URL.Path
	if p == "/docs" || p == "/docs/next" {
		redirect(w, r, p+"/")
		return
	}
	ch, rel := s.latest, strings.TrimPrefix(p, "/docs/")
	if strings.HasPrefix(rel, "next/") {
		ch, rel = s.next, strings.TrimPrefix(rel, "next/")
	}
	if ch == nil {
		http.Error(w, "this site was built without these docs", http.StatusNotFound)
		return
	}
	ch.serve(w, r, rel)
}

func (ch *docsChannel) serve(w http.ResponseWriter, r *http.Request, rel string) {
	h := w.Header()
	if ch.build.Version != "" {
		h.Set("X-Janus-Docs-Version", ch.build.Version)
	}
	if ch.noindex {
		h.Set("X-Robots-Tag", "noindex")
	}
	clean := strings.TrimPrefix(path.Clean("/"+rel), "/")
	dir := rel == "" || strings.HasSuffix(rel, "/")
	if dir {
		want := clean + "/"
		if clean == "" {
			want = ""
		}
		if f := ch.files[path.Join(clean, "index.html")]; f != nil {
			if rel != want {
				redirect(w, r, ch.prefix+want)
				return
			}
			ch.serveFile(w, r, path.Join(clean, "index.html"), f, http.StatusOK)
			return
		}
	} else {
		if f := ch.files[clean]; f != nil {
			if rel != clean {
				redirect(w, r, ch.prefix+clean)
				return
			}
			ch.serveFile(w, r, clean, f, http.StatusOK)
			return
		}
		if ch.files[path.Join(clean, "index.html")] != nil {
			redirect(w, r, ch.prefix+clean+"/")
			return
		}
		if to, ok := ch.redirects[clean]; ok {
			redirect(w, r, ch.prefix+to)
			return
		}
	}
	if f := ch.files["404.html"]; f != nil {
		ch.serveFile(w, r, "404.html", f, http.StatusNotFound)
		return
	}
	http.NotFound(w, r)
}

// hashedPagefind: Pagefind's index files whose name carries their hash.
var hashedPagefind = regexp.MustCompile(`^pagefind/(.+/)?[^/]*_[0-9a-f]{6,}\.pf_[a-z]+$`)

func (ch *docsChannel) serveFile(w http.ResponseWriter, r *http.Request, name string, f *staticFile, status int) {
	h := w.Header()
	if csp, ok := ch.csp[name]; ok {
		h.Set("Content-Security-Policy", csp)
	} else {
		h.Set("Content-Security-Policy", fmt.Sprintf(docsCSP, ""))
	}
	switch {
	case status != http.StatusOK:
		h.Set("Cache-Control", "no-cache")
	case strings.HasPrefix(name, "_astro/") || hashedPagefind.MatchString(name):
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	case strings.HasSuffix(name, ".html"):
		h.Set("Cache-Control", "no-cache")
	default:
		h.Set("Cache-Control", "public, max-age=300")
	}
	f.serve(w, r, status)
}

// redirect is a permanent redirect that keeps the query.
func redirect(w http.ResponseWriter, r *http.Request, to string) {
	if r.URL.RawQuery != "" {
		to += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, to, http.StatusMovedPermanently)
}
