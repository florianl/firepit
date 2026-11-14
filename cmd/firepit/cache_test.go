package main

import (
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/florianl/firepit/internal/store"
	profilespb "go.opentelemetry.io/proto/otlp/profiles/v1development"
)

// addTestProfiles adds the given number of profiles with 200 samples of 30 frames each to st.
func addTestProfiles(st *store.Store, profiles int) {
	const (
		numFunctions = 500
		numStacks    = 300
	)
	rng := rand.New(rand.NewPCG(1, 2))
	dict := &profilespb.ProfilesDictionary{
		StringTable:   []string{"", "samples", "count"},
		FunctionTable: []*profilespb.Function{{}},
		LocationTable: []*profilespb.Location{{}},
		StackTable:    []*profilespb.Stack{{}},
	}
	for f := range numFunctions {
		dict.StringTable = append(dict.StringTable, fmt.Sprintf("example.com/pkg%d.Function%d", f%20, f))
		dict.FunctionTable = append(dict.FunctionTable, &profilespb.Function{NameStrindex: int32(len(dict.StringTable) - 1)})
		dict.LocationTable = append(dict.LocationTable, &profilespb.Location{
			Lines: []*profilespb.Line{{FunctionIndex: int32(f + 1)}},
		})
	}
	for range numStacks {
		stack := &profilespb.Stack{}
		for range 30 {
			stack.LocationIndices = append(stack.LocationIndices, int32(1+rng.IntN(numFunctions)))
		}
		dict.StackTable = append(dict.StackTable, stack)
	}

	sp := &profilespb.ScopeProfiles{}
	for range profiles {
		profile := &profilespb.Profile{
			SampleType:   &profilespb.ValueType{TypeStrindex: 1, UnitStrindex: 2},
			TimeUnixNano: uint64(time.Now().UnixNano()),
		}
		for range 200 {
			profile.Samples = append(profile.Samples, &profilespb.Sample{
				StackIndex: int32(1 + rng.IntN(numStacks)),
				Values:     []int64{1},
			})
		}
		sp.Profiles = append(sp.Profiles, profile)
	}
	st.Add([]*profilespb.ResourceProfiles{{ScopeProfiles: []*profilespb.ScopeProfiles{sp}}}, dict)
}

func TestResponseCache(t *testing.T) {
	st := store.New(5*time.Minute, time.Hour, 0)
	defer st.Close()
	addTestProfiles(st, 1)

	var calls atomic.Int32
	inner := handleFlamegraph(st, newTestAggregator(t, st), nil)
	handler := newResponseCache(st).wrap(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		inner(w, r)
	})

	get := func(url, ifNoneMatch string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, url, nil)
		if ifNoneMatch != "" {
			req.Header.Set("If-None-Match", ifNoneMatch)
		}
		w := httptest.NewRecorder()
		handler(w, req)
		return w
	}

	first := get("/api/flamegraph", "")
	if first.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", first.Code)
	}
	if ct := first.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if cc := first.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", cc)
	}
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag")
	}

	// The same request is served from the cache.
	second := get("/api/flamegraph", "")
	if second.Body.String() != first.Body.String() || second.Header().Get("ETag") != etag {
		t.Error("cached response differs from first response")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("handler called %d times, want 1", got)
	}

	// Revalidation of an unchanged response does not transfer it again.
	for _, inm := range []string{etag, "W/" + etag, `"other", ` + etag} {
		notModified := get("/api/flamegraph", inm)
		if notModified.Code != http.StatusNotModified || notModified.Body.Len() != 0 {
			t.Errorf("If-None-Match %s: status = %d with %d bytes, want 304 without body",
				inm, notModified.Code, notModified.Body.Len())
		}
	}
	if stale := get("/api/flamegraph", `"stale"`); stale.Code != http.StatusOK {
		t.Errorf("status = %d for outdated ETag, want 200", stale.Code)
	}

	// Other query parameters are cached separately.
	filtered := get("/api/flamegraph?resourceType=service.name:none", "")
	if filtered.Body.String() == first.Body.String() {
		t.Error("filtered response equals unfiltered response")
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("handler called %d times, want 2", got)
	}

	// New profiles invalidate the cache.
	addTestProfiles(st, 1)
	updated := get("/api/flamegraph", etag)
	if updated.Code != http.StatusOK {
		t.Fatalf("status = %d after store changed, want 200", updated.Code)
	}
	if updated.Header().Get("ETag") == etag {
		t.Error("ETag did not change with the store")
	}
	if updated.Body.String() == first.Body.String() {
		t.Error("response did not change with the store")
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("handler called %d times, want 3", got)
	}
}

func TestResponseCacheErrors(t *testing.T) {
	st := store.New(5*time.Minute, time.Hour, 0)
	defer st.Close()
	cache := newResponseCache(st)

	// Error responses keep their status and are not tagged.
	req := httptest.NewRequest(http.MethodGet, "/api/sandwich-detail", nil)
	w := httptest.NewRecorder()
	cache.wrap(handleSandwichDetail(st, newTestAggregator(t, st), nil))(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if etag := w.Header().Get("ETag"); etag != "" {
		t.Errorf("error response has ETag %s", etag)
	}
}

func TestResponseCacheConcurrent(t *testing.T) {
	st := store.New(5*time.Minute, time.Hour, 0)
	defer st.Close()
	addTestProfiles(st, 1)

	var calls atomic.Int32
	inner := handleFlamegraph(st, newTestAggregator(t, st), nil)
	cache := newResponseCache(st)
	handler := cache.wrap(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		inner(w, r)
	})

	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			w := httptest.NewRecorder()
			handler(w, httptest.NewRequest(http.MethodGet, "/api/flamegraph", nil))
			if w.Code != http.StatusOK || w.Body.Len() == 0 {
				t.Errorf("status = %d with %d bytes", w.Code, w.Body.Len())
			}
		})
	}
	wg.Wait()
	if got := calls.Load(); got != 1 {
		t.Errorf("handler called %d times for concurrent requests, want 1", got)
	}

	// The number of cached responses is bounded.
	for i := range 4 * maxCachedResponses {
		w := httptest.NewRecorder()
		handler(w, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/flamegraph?resourceType=k:%d", i), nil))
		if n := len(cache.entries); n > maxCachedResponses {
			t.Fatalf("%d cached responses, want at most %d", n, maxCachedResponses)
		}
	}
}

// BenchmarkPollFlamegraph measures polling an unchanged flame graph of 100 profiles.
func BenchmarkPollFlamegraph(b *testing.B) {
	st := store.New(5*time.Minute, time.Hour, 0)
	defer st.Close()
	for range 10 {
		addTestProfiles(st, 10)
	}

	poll := func(b *testing.B, handler http.HandlerFunc, etag string) {
		req := httptest.NewRequest(http.MethodGet, "/api/flamegraph", nil)
		if etag != "" {
			req.Header.Set("If-None-Match", etag)
		}
		b.ReportAllocs()
		for b.Loop() {
			w := httptest.NewRecorder()
			handler(w, req)
			b.SetBytes(int64(w.Body.Len()))
		}
	}

	b.Run("uncached", func(b *testing.B) {
		poll(b, handleFlamegraph(st, newTestAggregator(b, st), nil), "")
	})
	b.Run("revalidated", func(b *testing.B) {
		handler := newResponseCache(st).wrap(handleFlamegraph(st, newTestAggregator(b, st), nil))
		w := httptest.NewRecorder()
		handler(w, httptest.NewRequest(http.MethodGet, "/api/flamegraph", nil))
		poll(b, handler, w.Header().Get("ETag"))
	})
}
