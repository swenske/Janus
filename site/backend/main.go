// Command janus-site serves janus.sw-servers.net: the landing page, the
// image builder (a React SPA, built into static/ and embedded), the docs
// (/docs/, built into docsdist/ and embedded - docs.go), and the image
// factory API behind it - schematics, the extension catalog of each
// release, custom builds (run by the schematic-build workflow on the
// project's runner, which uploads its results here), downloads, and the
// update lookup nodes and Controllers use. See docs/image-factory.md.
package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/swenske/Janus/internal/schematic"
)

//go:embed all:static
var staticFiles embed.FS

func main() {
	addr := flag.String("addr", ":8080", "listen address (plain HTTP: TLS is terminated by the front end)")
	dataDir := flag.String("data-dir", "/srv/janus-site/data", "schematics and built images")
	publicURL := flag.String("public-url", "https://janus.sw-servers.net", "the site's public URL, for links in API answers")
	repo := flag.String("repo", "swenske/Janus", "GitHub repository")
	githubAPI := flag.String("github-api", "https://api.github.com", "GitHub API base URL")
	tokenFile := flag.String("github-token-file", "", "file holding a GitHub token allowed to start the schematic-build workflow (Actions: read and write); without it, custom builds can't be started")
	keep := flag.Int("keep-versions", 3, "built versions kept per schematic")
	flag.Parse()

	token := ""
	if *tokenFile != "" {
		data, err := os.ReadFile(*tokenFile)
		if err != nil {
			log.Fatalf("read -github-token-file: %v", err)
		}
		token = strings.TrimSpace(string(data))
	}
	st, err := newStore(*dataDir)
	if err != nil {
		log.Fatal(err)
	}
	a := &app{store: st, gh: newGitHub(*githubAPI, *repo, token, "schematic-build.yml"), publicURL: strings.TrimRight(*publicURL, "/"), repo: *repo, builds: newBuildLimiter()}

	mux := http.NewServeMux()
	a.routes(mux)
	static, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatal(err)
	}
	frontend, err := loadStatic(static)
	if err != nil || frontend["index.html"] == nil {
		log.Fatalf("static: %v (index.html: %v)", err, frontend["index.html"] != nil)
	}
	mux.Handle("/", spa(frontend, a.publicURL))
	docs, err := loadDocs(docsDist)
	if err != nil {
		log.Fatalf("docs: %v", err)
	}
	mux.Handle("/docs", docs)
	mux.Handle("/docs/", docs)
	a.seoRoutes(mux, docs)
	for name, ch := range map[string]*docsChannel{"/docs/": docs.latest, "/docs/next/": docs.next} {
		if ch == nil {
			log.Printf("docs: %s not built into this binary", name)
			continue
		}
		log.Printf("docs: %s = %s (%s, %d files)", name, ch.build.Version, ch.build.Commit, len(ch.files))
	}

	go a.maintain(context.Background(), *keep)

	srv := &http.Server{Addr: *addr, Handler: securityHeaders(mux), ReadHeaderTimeout: 10 * time.Second}
	log.Printf("janus-site listening on %s (data in %s, token: %v)", *addr, *dataDir, token != "")
	log.Fatal(srv.ListenAndServe())
}

// spaRoutes are the frontend's own pages (App.jsx): index.html answers
// them, and the sitemap lists them.
var spaRoutes = []string{"/", "/builder"}

// spa serves the built frontend: its files, index.html for its routes
// (/builder... too: the builder keeps its state in the path's query) -
// and for anything else index.html again, with a 404 (the frontend shows
// its "not found" page; a crawler sees the status).
func spa(files staticSet, publicURL string) http.Handler {
	index := files["index.html"]
	// Each page's own index.html: its title, description, canonical link,
	// cards and JSON-LD (seo.go).
	pages := map[string]*staticFile{}
	for route, p := range spaPages(publicURL) {
		pages[route] = newStaticFile("index.html", withMeta(index.body, route, p, publicURL))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name == "index.html" {
			redirect(w, r, "/")
			return
		}
		if f := files[name]; f != nil && name != "" {
			if strings.HasPrefix(name, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "public, max-age=3600")
			}
			f.serve(w, r, http.StatusOK)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		switch {
		case name == "":
			pages["/"].serve(w, r, http.StatusOK)
		case name == "builder" || strings.HasPrefix(name, "builder/"):
			pages["/builder"].serve(w, r, http.StatusOK)
		default:
			index.serve(w, r, http.StatusNotFound)
		}
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

// maintain runs the site's periodic work: when a release appears, build
// its update bundle for every schematic already in use, so nodes find
// their update ready; and drop old builds.
func (a *app) maintain(ctx context.Context, keep int) {
	for {
		a.prebuild(ctx)
		if err := a.store.Prune(keep, compareVersions); err != nil {
			log.Printf("prune: %v", err)
		}
		time.Sleep(15 * time.Minute) //nolint:gosec // G118: runs for the process's life, there's nothing to cancel it
	}
}

func (a *app) prebuild(ctx context.Context) {
	rels, err := a.gh.Releases(ctx)
	if err != nil {
		return
	}
	var latest *release
	for i := range rels {
		if rels[i].Schematics {
			latest = &rels[i]
			break
		}
	}
	if latest == nil {
		return
	}
	ids, err := a.store.SchematicIDs()
	if err != nil {
		return
	}
	for _, id := range ids {
		if !a.inUse(id) {
			continue
		}
		st, sc, _, err := a.status(ctx, id, latest.Version, "amd64")
		if err == nil && st.State == "unavailable" {
			// The release may offer the schematic's extensions under new
			// names: build the migrated schematic its nodes will be offered.
			st, sc, err = a.migrated(ctx, sc, latest.Version)
		}
		if err != nil || st.State != "none" {
			continue
		}
		if err := a.startBuild(ctx, sc, latest.Version, "amd64", "prebuild", false); err != nil {
			log.Printf("prebuild %s %s: %v", id, latest.Version, err)
		}
	}
}

// migrated is the status of sc migrated to the extension names of
// version's catalog (schematic.Catalog.Migrate), stored - an error when
// nothing was renamed or the result can't be built.
func (a *app) migrated(ctx context.Context, sc *schematic.Schematic, version string) (*imageStatus, *schematic.Schematic, error) {
	c, err := a.gh.Catalog(ctx, version)
	if err != nil || c == nil {
		return nil, nil, fmt.Errorf("no catalog for %s", version)
	}
	m, renamed := c.Migrate(sc)
	if len(renamed) == 0 || c.Check(m, "amd64") != nil {
		return nil, nil, fmt.Errorf("%s offers no migration of %s", version, sc.ID())
	}
	if _, err := a.store.PutSchematic(m); err != nil {
		return nil, nil, err
	}
	st, msc, _, err := a.status(ctx, m.ID(), version, "amd64")
	return st, msc, err
}

// inUse reports whether a schematic was ever built: only those get their
// updates built ahead of time.
func (a *app) inUse(id string) bool {
	entries, err := os.ReadDir(a.store.dir + "/images/" + id)
	return err == nil && len(entries) > 0
}
