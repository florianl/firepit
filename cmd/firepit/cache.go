package main

import (
	"bytes"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/florianl/firepit/internal/store"
)

const (
	// maxCachedResponses is the number of distinct URLs cached per generation.
	maxCachedResponses = 16
	// maxCachedQueryLen is the longest query string that is cached.
	maxCachedQueryLen = 1024
)

// responseCache caches handler responses until the store generation changes.
type responseCache struct {
	st *store.Store
	// id makes ETags unique across process restarts.
	id string

	mu         sync.Mutex
	generation uint64
	entries    map[string]*cachedResponse
}

type cachedResponse struct {
	once        sync.Once
	status      int
	contentType string
	body        []byte
}

func newResponseCache(st *store.Store) *responseCache {
	return &responseCache{
		st:      st,
		id:      strconv.FormatInt(time.Now().UnixNano(), 36),
		entries: make(map[string]*cachedResponse),
	}
}

// entry returns the cache entry for key and the current store generation.
func (c *responseCache) entry(key string) (*cachedResponse, uint64) {
	// Read before computing, so a racing store update only causes a recompute.
	generation := c.st.Generation()

	c.mu.Lock()
	defer c.mu.Unlock()

	if generation != c.generation {
		clear(c.entries)
		c.generation = generation
	}
	e, ok := c.entries[key]
	if !ok {
		if len(c.entries) >= maxCachedResponses {
			clear(c.entries)
		}
		e = &cachedResponse{}
		c.entries[key] = e
	}
	return e, generation
}

// wrap returns a handler serving the GET responses of next from the cache.
func (c *responseCache) wrap(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || len(r.URL.RawQuery) > maxCachedQueryLen {
			next(w, r)
			return
		}

		e, generation := c.entry(r.URL.Path + "?" + r.URL.RawQuery)
		e.once.Do(func() {
			// Reported if next panics.
			e.status = http.StatusInternalServerError
			rec := &responseRecorder{header: make(http.Header), status: http.StatusOK}
			next(rec, r)
			e.status = rec.status
			e.contentType = rec.header.Get("Content-Type")
			e.body = rec.body.Bytes()
		})

		h := w.Header()
		if e.contentType != "" {
			h.Set("Content-Type", e.contentType)
		}
		if e.status == http.StatusOK {
			etag := `"` + c.id + "-" + strconv.FormatUint(generation, 36) + `"`
			h.Set("ETag", etag)
			h.Set("Cache-Control", "no-cache")
			if etagMatches(r.Header.Get("If-None-Match"), etag) {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		h.Set("Content-Length", strconv.Itoa(len(e.body)))
		w.WriteHeader(e.status)
		w.Write(e.body)
	}
}

// etagMatches reports whether ifNoneMatch weakly matches etag.
func etagMatches(ifNoneMatch, etag string) bool {
	for candidate := range strings.SplitSeq(ifNoneMatch, ",") {
		candidate = strings.TrimPrefix(strings.TrimSpace(candidate), "W/")
		if candidate == etag || candidate == "*" {
			return true
		}
	}
	return false
}

// responseRecorder is an http.ResponseWriter that buffers the response.
type responseRecorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *responseRecorder) Header() http.Header         { return r.header }
func (r *responseRecorder) WriteHeader(status int)      { r.status = status }
func (r *responseRecorder) Write(b []byte) (int, error) { return r.body.Write(b) }
