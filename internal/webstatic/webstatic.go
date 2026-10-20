// Package webstatic serves a tree of embedded files well: each file's
// content type, a strong ETag (embedded files have no modification time,
// so http.FileServerFS sends neither Last-Modified nor ETag - every page
// load fetched everything again), its gzipped body when that's worth it,
// and for a single-page application the Cache-Control each kind of file
// deserves. Shared by the Controller (its page and each node's) and the
// site.
package webstatic

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
)

// File is one file prepared once at startup: its content type, a
// strong ETag, and its gzipped body when that's worth it - the front
// end doesn't compress.
type File struct {
	Body  []byte
	gz    []byte // nil when compressing doesn't pay
	etag  string // quoted, of the identity body; the gzipped one adds -gz
	ctype string
}

// Set is a tree's files, keyed by path ("assets/index.js").
type Set map[string]*File

// compressible is what's worth gzipping: text. Pagefind's index files
// (.pf_*, .pagefind) are compressed already.
var compressible = map[string]bool{
	".html": true, ".css": true, ".js": true, ".mjs": true, ".json": true,
	".xml": true, ".svg": true, ".txt": true, ".webmanifest": true, ".map": true,
}

var extraTypes = map[string]string{
	".mjs":         "text/javascript; charset=utf-8",
	".webmanifest": "application/manifest+json",
	".wasm":        "application/wasm",
	".pagefind":    "application/octet-stream",
	".xml":         "application/xml; charset=utf-8",
	".txt":         "text/plain; charset=utf-8",
}

func contentType(name string) string {
	ext := strings.ToLower(path.Ext(name))
	if t, ok := extraTypes[ext]; ok {
		return t
	}
	if strings.HasPrefix(ext, ".pf_") {
		return "application/octet-stream"
	}
	if t := mime.TypeByExtension(ext); t != "" {
		return t
	}
	return "application/octet-stream"
}

// New prepares body to be served as name.
func New(name string, body []byte) *File {
	sum := sha256.Sum256(body)
	f := &File{Body: body, etag: `"` + hex.EncodeToString(sum[:12]) + `"`, ctype: contentType(name)}
	if compressible[strings.ToLower(path.Ext(name))] && len(body) > 512 {
		var b bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&b, gzip.BestCompression)
		_, _ = zw.Write(body)
		_ = zw.Close()
		if b.Len() < len(body)*9/10 {
			f.gz = b.Bytes()
		}
	}
	return f
}

// Load prepares every file of fsys, keyed by its path ("a/b.css").
func Load(fsys fs.FS) (Set, error) {
	set := Set{}
	err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		set[name] = New(name, body)
		return nil
	})
	return set, err
}

// AcceptsGzip reports whether the client takes gzip (and didn't refuse
// it with q=0).
func AcceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		coding, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if strings.EqualFold(strings.TrimSpace(coding), "gzip") {
			q := strings.ReplaceAll(strings.TrimSpace(params), " ", "")
			return q != "q=0" && q != "q=0.0" && q != "q=0.00" && q != "q=0.000"
		}
	}
	return false
}

// Serve writes f with status (200: conditional requests, ranges and HEAD
// through http.ServeContent; anything else: the body as is).
func (f *File) Serve(w http.ResponseWriter, r *http.Request, status int) {
	h := w.Header()
	h.Set("Content-Type", f.ctype)
	body, etag := f.Body, f.etag
	if f.gz != nil {
		h.Add("Vary", "Accept-Encoding")
		if AcceptsGzip(r) {
			h.Set("Content-Encoding", "gzip")
			body, etag = f.gz, strings.TrimSuffix(f.etag, `"`)+`-gz"`
		}
	}
	if status != http.StatusOK {
		h.Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(status)
		if r.Method != http.MethodHead {
			_, _ = w.Write(body)
		}
		return
	}
	h.Set("ETag", etag)
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
}

// SPA serves a single-page application's built tree: index.html for
// "/", its hashed assets (assets/*) cached for a year as immutable,
// every other file revalidated with its ETag (no-cache), nothing else
// (404). Any method, like http.FileServerFS; a directory isn't listed.
func SPA(fsys fs.FS) (http.Handler, error) {
	files, err := Load(fsys)
	if err != nil {
		return nil, err
	}
	if files["index.html"] == nil {
		return nil, fmt.Errorf("webstatic: no index.html")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name == "" {
			name = "index.html"
		}
		f := files[name]
		if f == nil {
			http.NotFound(w, r)
			return
		}
		if strings.HasPrefix(name, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		f.Serve(w, r, http.StatusOK)
	}), nil
}
