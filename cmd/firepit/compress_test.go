package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/florianl/firepit/internal/store"
)

func gunzip(t *testing.T, b []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("not a gzip stream: %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("failed to decompress: %v", err)
	}
	return out
}

func TestAcceptsGzip(t *testing.T) {
	tests := map[string]bool{
		"":                      false,
		"gzip":                  true,
		"gzip, deflate, br":     true,
		"deflate, gzip;q=0.5":   true,
		"br;q=1.0, gzip; q=0.8": true,
		"*":                     true,
		"gzip;q=0":              false,
		"deflate, br":           false,
		"identity, gzip;q=0.0":  false,
	}
	for header, want := range tests {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Accept-Encoding", header)
		if got := acceptsGzip(r); got != want {
			t.Errorf("acceptsGzip(%q) = %v, want %v", header, got, want)
		}
	}
}

func TestCompressResponse(t *testing.T) {
	st := store.New(5*time.Minute, time.Hour, 0)
	defer st.Close()

	plain := httptest.NewRecorder()
	handleConfig(Config{})(plain, httptest.NewRequest(http.MethodGet, "/api/config", nil))

	handler := compressResponse(handleConfig(Config{}))

	t.Run("gzip accepted", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
		req.Header.Set("Accept-Encoding", "gzip, deflate")
		w := httptest.NewRecorder()
		handler(w, req)

		if enc := w.Header().Get("Content-Encoding"); enc != "gzip" {
			t.Fatalf("Content-Encoding = %q, want gzip", enc)
		}
		if vary := w.Header().Get("Vary"); vary != "Accept-Encoding" {
			t.Errorf("Vary = %q, want Accept-Encoding", vary)
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		if got := gunzip(t, w.Body.Bytes()); !bytes.Equal(got, plain.Body.Bytes()) {
			t.Errorf("decompressed body = %q, want %q", got, plain.Body.Bytes())
		}
	})

	t.Run("gzip not accepted", func(t *testing.T) {
		w := httptest.NewRecorder()
		handler(w, httptest.NewRequest(http.MethodGet, "/api/config", nil))

		if enc := w.Header().Get("Content-Encoding"); enc != "" {
			t.Errorf("Content-Encoding = %q, want none", enc)
		}
		if !bytes.Equal(w.Body.Bytes(), plain.Body.Bytes()) {
			t.Errorf("body = %q, want %q", w.Body.Bytes(), plain.Body.Bytes())
		}
	})

	t.Run("error status is kept", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/config", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		w := httptest.NewRecorder()
		handler(w, req)

		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("status = %d, want 405", w.Code)
		}
		if got := string(gunzip(t, w.Body.Bytes())); !strings.Contains(got, "Method not allowed") {
			t.Errorf("decompressed body = %q", got)
		}
	})

	t.Run("no body", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		w := httptest.NewRecorder()
		compressResponse(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotModified)
		})(w, req)

		if enc := w.Header().Get("Content-Encoding"); enc != "" || w.Body.Len() != 0 {
			t.Errorf("Content-Encoding = %q with %d bytes, want an empty plain response", enc, w.Body.Len())
		}
	})
}

func TestStaticFiles(t *testing.T) {
	script := []byte(strings.Repeat("function f() { return 42; }\n", 200))
	fsys := fstest.MapFS{
		"index.html": {Data: []byte("placeholder")},
		"app.js":     {Data: script},
		"tiny.css":   {Data: []byte("a{}")},
	}
	index := []byte("<html>" + strings.Repeat("<p>firepit</p>", 100) + "</html>")
	static, err := newStaticFiles(fsys, index)
	if err != nil {
		t.Fatal(err)
	}

	get := func(path string, header ...string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/"+strings.TrimPrefix(path, "/"), nil)
		for i := 0; i < len(header); i += 2 {
			req.Header.Set(header[i], header[i+1])
		}
		w := httptest.NewRecorder()
		static.serve(w, req, path)
		return w
	}

	for _, path := range []string{"", "/", "/index.html"} {
		w := get(path)
		if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), index) {
			t.Errorf("%q: status = %d, body is index.html = %v", path, w.Code, bytes.Equal(w.Body.Bytes(), index))
		}
		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("%q: Content-Type = %q", path, ct)
		}
	}

	plain := get("/app.js")
	if plain.Header().Get("Content-Encoding") != "" || !bytes.Equal(plain.Body.Bytes(), script) {
		t.Error("/app.js is not served as is without Accept-Encoding")
	}
	if ct := plain.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("/app.js: Content-Type = %q", ct)
	}
	if cc := plain.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("/app.js: Cache-Control = %q, want no-cache", cc)
	}

	zipped := get("/app.js", "Accept-Encoding", "gzip")
	if enc := zipped.Header().Get("Content-Encoding"); enc != "gzip" {
		t.Fatalf("/app.js: Content-Encoding = %q, want gzip", enc)
	}
	if zipped.Body.Len() >= len(script) {
		t.Errorf("/app.js: %d compressed bytes for %d bytes", zipped.Body.Len(), len(script))
	}
	if !bytes.Equal(gunzip(t, zipped.Body.Bytes()), script) {
		t.Error("/app.js: decompressed body differs")
	}
	if plain.Header().Get("ETag") == "" || plain.Header().Get("ETag") == zipped.Header().Get("ETag") {
		t.Errorf("ETags of plain and gzip response: %q, %q; want distinct tags",
			plain.Header().Get("ETag"), zipped.Header().Get("ETag"))
	}

	// Revalidation transfers no body
	for _, etag := range []string{plain.Header().Get("ETag"), zipped.Header().Get("ETag")} {
		w := get("/app.js", "Accept-Encoding", "gzip", "If-None-Match", etag)
		if w.Code != http.StatusNotModified || w.Body.Len() != 0 {
			t.Errorf("If-None-Match %s: status = %d with %d bytes, want 304 without body", etag, w.Code, w.Body.Len())
		}
	}
	if w := get("/app.js", "If-None-Match", `"outdated"`); w.Code != http.StatusOK {
		t.Errorf("status = %d for outdated ETag, want 200", w.Code)
	}

	// Incompressible files are served as is
	if w := get("/tiny.css", "Accept-Encoding", "gzip"); w.Header().Get("Content-Encoding") != "" || w.Body.String() != "a{}" {
		t.Errorf("/tiny.css: Content-Encoding = %q, body = %q", w.Header().Get("Content-Encoding"), w.Body.String())
	}

	if w := get("/missing.js"); w.Code != http.StatusNotFound {
		t.Errorf("/missing.js: status = %d, want 404", w.Code)
	}

	req := httptest.NewRequest(http.MethodPost, "/app.js", nil)
	w := httptest.NewRecorder()
	static.serve(w, req, "/app.js")
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: status = %d, want 405", w.Code)
	}

	req = httptest.NewRequest(http.MethodHead, "/app.js", nil)
	w = httptest.NewRecorder()
	static.serve(w, req, "/app.js")
	if w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Errorf("HEAD: status = %d with %d bytes, want 200 without body", w.Code, w.Body.Len())
	}
}

func TestWebUIServesBasePathInIndex(t *testing.T) {
	st := store.New(5*time.Minute, time.Hour, 0)
	defer st.Close()
	mux := buildWebUIMux(st, Config{BasePath: "/firepit"})

	for _, path := range []string{"/firepit/", "/firepit/index.html"} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		body := w.Body.String()
		if w.Code != http.StatusOK || strings.Contains(body, "FIREPIT_BASE_PATH") || !strings.Contains(body, "/firepit") {
			t.Errorf("%s: status = %d, base path not applied to index.html", path, w.Code)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/firepit/api/config", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if enc := w.Header().Get("Content-Encoding"); enc != "gzip" {
		t.Errorf("API response: Content-Encoding = %q, want gzip", enc)
	}
}
