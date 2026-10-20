package webstatic

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func testSPA(t *testing.T) http.Handler {
	t.Helper()
	h, err := SPA(fstest.MapFS{
		"index.html":           {Data: []byte("<!doctype html><title>Janus</title>" + strings.Repeat("<p>x</p>", 200))},
		"assets/index-Ab12.js": {Data: []byte("console.log(1);" + strings.Repeat("// padding\n", 100))},
		"assets/font.woff2":    {Data: []byte("wOF2 not compressible already")},
		"favicon.svg":          {Data: []byte("<svg/>")},
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func get(h http.Handler, path string, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestSPA(t *testing.T) {
	h := testSPA(t)

	// The page: revalidated every time, an ETag to do it with.
	w := get(h, "/")
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-cache" || w.Header().Get("ETag") == "" || !strings.Contains(w.Body.String(), "<title>Janus</title>") {
		t.Fatalf("/: %d %v", w.Code, w.Header())
	}
	if w2 := get(h, "/", "If-None-Match", w.Header().Get("ETag")); w2.Code != http.StatusNotModified {
		t.Fatalf("If-None-Match: %d, want 304", w2.Code)
	}
	if w2 := get(h, "/index.html"); w2.Code != 200 || w2.Header().Get("ETag") != w.Header().Get("ETag") {
		t.Fatalf("/index.html: %d %v", w2.Code, w2.Header())
	}

	// A hashed asset: a year, immutable, gzipped for who takes it.
	w = get(h, "/assets/index-Ab12.js", "Accept-Encoding", "gzip, deflate")
	if w.Code != 200 || w.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" || w.Header().Get("Content-Encoding") != "gzip" || w.Header().Get("Vary") != "Accept-Encoding" {
		t.Fatalf("asset: %d %v", w.Code, w.Header())
	}
	zr, err := gzip.NewReader(w.Body)
	if err != nil {
		t.Fatal(err)
	}
	if body, _ := io.ReadAll(zr); !strings.HasPrefix(string(body), "console.log(1);") {
		t.Fatalf("gzipped body: %q", body)
	}
	plain := get(h, "/assets/index-Ab12.js")
	if plain.Header().Get("Content-Encoding") != "" || !strings.HasPrefix(plain.Body.String(), "console.log(1);") || plain.Header().Get("ETag") == w.Header().Get("ETag") {
		t.Fatalf("identity: %v %q", plain.Header(), plain.Body.String()[:20])
	}
	if w := get(h, "/assets/font.woff2", "Accept-Encoding", "gzip"); w.Header().Get("Content-Encoding") != "" || w.Header().Get("Content-Type") != "font/woff2" {
		t.Fatalf("font: %v", w.Header())
	}

	// Other files: revalidated. Anything else, a directory included: 404.
	if w := get(h, "/favicon.svg"); w.Code != 200 || w.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("favicon: %d %v", w.Code, w.Header())
	}
	for _, p := range []string{"/nothing.js", "/assets/", "/assets", "/index.html/x"} {
		if w := get(h, p); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", p, w.Code)
		}
	}
	if w := get(h, "/a/../assets/index-Ab12.js"); w.Code != 200 {
		t.Errorf("cleaned path: %d", w.Code)
	}
}

func TestSPANeedsIndex(t *testing.T) {
	if _, err := SPA(fstest.MapFS{"a.txt": {Data: []byte("x")}}); err == nil {
		t.Fatal("no index.html accepted")
	}
}
