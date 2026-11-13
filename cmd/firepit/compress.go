package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
)

// acceptsGzip reports whether the client accepts gzip encoded responses.
func acceptsGzip(r *http.Request) bool {
	for enc := range strings.SplitSeq(r.Header.Get("Accept-Encoding"), ",") {
		name, params, _ := strings.Cut(enc, ";")
		if name = strings.TrimSpace(name); name != "gzip" && name != "*" {
			continue
		}
		if q, ok := strings.CutPrefix(strings.TrimSpace(params), "q="); ok {
			if v, err := strconv.ParseFloat(q, 64); err == nil && v == 0 {
				continue
			}
		}
		return true
	}
	return false
}

// matchesETag reports whether If-None-Match weakly matches any of etags.
func matchesETag(r *http.Request, etags ...string) bool {
	for candidate := range strings.SplitSeq(r.Header.Get("If-None-Match"), ",") {
		candidate = strings.TrimPrefix(strings.TrimSpace(candidate), "W/")
		if candidate == "*" {
			return true
		}
		for _, etag := range etags {
			if candidate == etag {
				return true
			}
		}
	}
	return false
}

var gzipWriterPool = sync.Pool{
	New: func() any {
		// Compressed on the fly, so favor speed
		zw, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed)
		return zw
	},
}

// compressResponse gzip encodes the response of next if the client accepts it.
func compressResponse(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Accept-Encoding")
		if !acceptsGzip(r) {
			next(w, r)
			return
		}
		gw := &gzipResponseWriter{ResponseWriter: w}
		defer gw.finish()
		next(gw, r)
	}
}

// gzipResponseWriter gzip encodes the body of a response.
type gzipResponseWriter struct {
	http.ResponseWriter
	zw          *gzip.Writer
	wroteHeader bool
}

func (w *gzipResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true

	h := w.Header()
	hasBody := status >= http.StatusOK && status != http.StatusNoContent && status != http.StatusNotModified
	if hasBody && h.Get("Content-Encoding") == "" {
		h.Set("Content-Encoding", "gzip")
		h.Del("Content-Length")
		w.zw = gzipWriterPool.Get().(*gzip.Writer)
		w.zw.Reset(w.ResponseWriter)
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *gzipResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.zw == nil {
		return w.ResponseWriter.Write(b)
	}
	return w.zw.Write(b)
}

// finish flushes the gzip stream and returns the writer to the pool.
func (w *gzipResponseWriter) finish() {
	if w.zw == nil {
		return
	}
	w.zw.Close()
	w.zw.Reset(io.Discard)
	gzipWriterPool.Put(w.zw)
	w.zw = nil
}

// staticFile is a web UI file prepared for serving.
type staticFile struct {
	contentType string
	etag        string
	gzipETag    string
	raw         []byte
	// gzipped is nil if compression does not pay off
	gzipped []byte
}

// staticTypes pins the content types of the web UI's own files, since
// mime.TypeByExtension consults the host's MIME database and varies by system.
var staticTypes = map[string]string{
	".html": "text/html; charset=utf-8",
	".css":  "text/css; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
	".json": "application/json",
	".svg":  "image/svg+xml",
}

func newStaticFile(name string, raw []byte) (*staticFile, error) {
	ext := path.Ext(name)
	contentType, ok := staticTypes[ext]
	if !ok {
		contentType = mime.TypeByExtension(ext)
	}
	if contentType == "" {
		contentType = http.DetectContentType(raw)
	}

	sum := sha256.Sum256(raw)
	tag := hex.EncodeToString(sum[:8])
	f := &staticFile{
		contentType: contentType,
		etag:        `"` + tag + `"`,
		gzipETag:    `"` + tag + `-gzip"`,
		raw:         raw,
	}

	// Compressed once at startup, so use the best ratio
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := zw.Write(raw); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	if buf.Len() < len(raw)*9/10 {
		f.gzipped = buf.Bytes()
	}
	return f, nil
}

// staticFiles serves the web UI files from memory.
type staticFiles map[string]*staticFile

// newStaticFiles loads all files of fsys, using indexHTML for index.html.
func newStaticFiles(fsys fs.FS, indexHTML []byte) (staticFiles, error) {
	files := make(staticFiles)
	err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw := indexHTML
		if name != "index.html" {
			if raw, err = fs.ReadFile(fsys, name); err != nil {
				return err
			}
		}
		files["/"+name], err = newStaticFile(name, raw)
		return err
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

// serve writes the file at the absolute path name to w.
func (s staticFiles) serve(w http.ResponseWriter, r *http.Request, name string) {
	if name == "" || name == "/" {
		name = "/index.html"
	}
	f, ok := s[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, etag := f.raw, f.etag
	h := w.Header()
	if f.gzipped != nil {
		h.Set("Vary", "Accept-Encoding")
		if acceptsGzip(r) {
			body, etag = f.gzipped, f.gzipETag
			h.Set("Content-Encoding", "gzip")
		}
	}
	h.Set("Content-Type", f.contentType)
	h.Set("ETag", etag)
	// Always revalidate so a new firepit version is picked up right away
	h.Set("Cache-Control", "no-cache")

	if matchesETag(r, f.etag, f.gzipETag) {
		h.Del("Content-Encoding")
		w.WriteHeader(http.StatusNotModified)
		return
	}

	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		w.Write(body)
	}
}
