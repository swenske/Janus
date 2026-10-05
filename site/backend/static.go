package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
)

// A tree of embedded files prepared once at startup to be served well:
// each file's content type, a strong ETag (embedded files have no
// modification time, so no Last-Modified to revalidate with), and its
// gzipped body when that's worth it - the front end doesn't compress.
type staticFile struct {
	body  []byte
	gz    []byte // nil when compressing doesn't pay
	etag  string // quoted, of the identity body; the gzipped one adds -gz
	ctype string
}

type staticSet map[string]*staticFile

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

func newStaticFile(name string, body []byte) *staticFile {
	sum := sha256.Sum256(body)
	f := &staticFile{body: body, etag: `"` + hex.EncodeToString(sum[:12]) + `"`, ctype: contentType(name)}
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

// loadStatic prepares every file of fsys, keyed by its path ("a/b.css").
func loadStatic(fsys fs.FS) (staticSet, error) {
	set := staticSet{}
	err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		set[name] = newStaticFile(name, body)
		return nil
	})
	return set, err
}

// acceptsGzip reports whether the client takes gzip (and didn't refuse
// it with q=0).
func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		coding, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if strings.EqualFold(strings.TrimSpace(coding), "gzip") {
			q := strings.ReplaceAll(strings.TrimSpace(params), " ", "")
			return q != "q=0" && q != "q=0.0" && q != "q=0.00" && q != "q=0.000"
		}
	}
	return false
}

// serve writes f with status (200: conditional requests, ranges and HEAD
// through http.ServeContent; anything else: the body as is).
func (f *staticFile) serve(w http.ResponseWriter, r *http.Request, status int) {
	h := w.Header()
	h.Set("Content-Type", f.ctype)
	body, etag := f.body, f.etag
	if f.gz != nil {
		h.Add("Vary", "Accept-Encoding")
		if acceptsGzip(r) {
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
