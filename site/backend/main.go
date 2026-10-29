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
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/swenske/Janus/internal/schematic"
	"github.com/swenske/Janus/internal/webstatic"
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
	keep := flag.Int("keep-versions", 3, "releases whose builds are kept per schematic (with the pre-releases newer than them all)")
	mirrorTokenFile := flag.String("mirror-token-file", "", "file holding the bearer token of the image factory's mirror: on the site, what lets it read GET /api/v1/mirror/index and send its counts (empty: refused); with -mirror-of, what it sends")
	mirrorOf := flag.String("mirror-of", "", "run as the image factory's mirror of this site (e.g. http://172.16.1.90:8080): copy the update bundles it built for its two newest releases, answer the update questions from them - nothing else (docs/image-factory.md)")
	priorityURL := flag.String("prebuild-priority-url", "", "janus-insights' GET /v1/prebuild-priority: the order a release's builds go in, the schematics most run first (empty: the store's order)")
	priorityTokenFile := flag.String("prebuild-priority-token-file", "", "file holding -prebuild-priority-url's bearer token")
	flag.Parse()

	mirrorToken := ""
	if *mirrorTokenFile != "" {
		if mirrorToken = readSecret(*mirrorTokenFile); mirrorToken == "" {
			log.Fatalf("-mirror-token-file %s is empty", *mirrorTokenFile)
		}
	}
	if *mirrorOf != "" {
		runMirror(*addr, *dataDir, strings.TrimRight(*mirrorOf, "/"), mirrorToken, strings.TrimRight(*publicURL, "/"))
		return
	}

	token := ""
	if *tokenFile != "" {
		token = readSecret(*tokenFile)
	}
	st, err := newStore(*dataDir)
	if err != nil {
		log.Fatal(err)
	}
	stats, err := newDownloadStats(*dataDir)
	if err != nil {
		log.Fatalf("download counts: %v", err)
	}
	a := &app{store: st, gh: newGitHub(*githubAPI, *repo, token, "schematic-build.yml"), publicURL: strings.TrimRight(*publicURL, "/"), repo: *repo,
		builds: newBuildLimiter(), stats: stats, mirrorToken: mirrorToken}
	if *priorityURL != "" {
		a.priority = &priority{url: *priorityURL, token: readSecret(*priorityTokenFile), client: &http.Client{Timeout: 10 * time.Second}}
	}

	mux := http.NewServeMux()
	a.routes(mux)
	static, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatal(err)
	}
	frontend, err := webstatic.Load(static)
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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	go a.maintain(ctx, *keep)
	counted := make(chan struct{})
	go func() {
		stats.run(ctx) // the counts are written once more when the site stops
		close(counted)
	}()

	srv := &http.Server{Addr: *addr, Handler: securityHeaders(mux), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shut)
	}()
	log.Printf("janus-site listening on %s (data in %s, token: %v)", *addr, *dataDir, token != "")
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	<-counted
}

// spaRoutes are the frontend's own pages (App.jsx): index.html answers
// them, and the sitemap lists them.
var spaRoutes = []string{"/", "/builder", "/tour"}

// spa serves the built frontend: its files, index.html for its routes
// (/builder... too: the builder keeps its state in the path's query) -
// and for anything else index.html again, with a 404 (the frontend shows
// its "not found" page; a crawler sees the status).
func spa(files webstatic.Set, publicURL string) http.Handler {
	index := files["index.html"]
	// Each page's own index.html: its title, description, canonical link,
	// cards and JSON-LD (seo.go).
	pages := map[string]*webstatic.File{}
	for route, p := range spaPages(publicURL) {
		pages[route] = webstatic.New("index.html", withMeta(index.Body, route, p, publicURL))
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
			f.Serve(w, r, http.StatusOK)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		switch {
		case name == "":
			pages["/"].Serve(w, r, http.StatusOK)
		case name == "builder" || strings.HasPrefix(name, "builder/"):
			pages["/builder"].Serve(w, r, http.StatusOK)
		case name == "tour":
			pages["/tour"].Serve(w, r, http.StatusOK)
		default:
			index.Serve(w, r, http.StatusNotFound)
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
// their update ready (not for a pre-release: its testers' builds are
// made when asked for, and a promoted one is a release then); count the
// default schematic's updates GitHub served; and drop old builds - only
// once the releases are known, to tell the pre-releases.
func (a *app) maintain(ctx context.Context, keep int) {
	for {
		a.prebuild(ctx)
		if rels, err := a.gh.Releases(ctx); err == nil {
			a.stats.sampleGitHub(rels)
			pre := map[string]bool{}
			for _, r := range rels {
				pre[r.Version] = r.Prerelease
			}
			if err := a.store.Prune(keep, compareVersions, func(v string) bool { return pre[v] }); err != nil {
				log.Printf("prune: %v", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(15 * time.Minute):
		}
	}
}

func (a *app) prebuild(ctx context.Context) {
	rels, err := a.gh.Releases(ctx)
	if err != nil {
		return
	}
	var latest *release
	for i := range rels {
		if rels[i].Schematics && !rels[i].Prerelease {
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
	for _, id := range a.priority.order(ctx, ids) {
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
		if err := a.startBuild(ctx, sc, latest.Version, "amd64", prebuildClient, false); err != nil {
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

// readSecret is a token file's content, trimmed.
func readSecret(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("read %s: %v", path, err)
	}
	return strings.TrimSpace(string(data))
}

// runMirror runs janus-site as the image factory's mirror of site
// (mirror.go): the update routes only, its copy synced every 15
// minutes, its counts written as the site's are.
func runMirror(addr, dataDir, site, token, publicURL string) {
	if token == "" {
		log.Fatal("-mirror-of needs -mirror-token-file")
	}
	st, err := newStore(dataDir)
	if err != nil {
		log.Fatal(err)
	}
	stats, err := newDownloadStats(dataDir)
	if err != nil {
		log.Fatalf("download counts: %v", err)
	}
	m := &mirror{store: st, stats: stats, site: site, token: token, publicURL: publicURL, client: &http.Client{Timeout: 5 * time.Minute}}
	m.load()
	mux := http.NewServeMux()
	m.routes(mux)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	go m.run(ctx)
	counted := make(chan struct{})
	go func() {
		stats.run(ctx)
		close(counted)
	}()
	srv := &http.Server{Addr: addr, Handler: securityHeaders(mux), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shut)
	}()
	log.Printf("janus-site: the image factory's mirror of %s, listening on %s (data in %s)", site, addr, dataDir)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	<-counted
}
