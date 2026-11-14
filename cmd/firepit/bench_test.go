package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/florianl/firepit/internal/store"
)

// pollFlamegraph requests the flame graph like the web UI does. With an etag,
// it expects the response to be not modified.
func pollFlamegraph(b *testing.B, mux *http.ServeMux, etag string) *httptest.ResponseRecorder {
	b.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/flamegraph", nil)
	want := http.StatusOK
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
		want = http.StatusNotModified
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != want || (want == http.StatusOK && w.Body.Len() < 1000) {
		b.Fatalf("status = %d with %d bytes, want %d", w.Code, w.Body.Len(), want)
	}
	return w
}

// BenchmarkPollUnchanged measures polling 100 unchanged profiles.
func BenchmarkPollUnchanged(b *testing.B) {
	st := store.New(5*time.Minute, time.Hour, 0)
	defer st.Close()
	for range 10 {
		addTestProfiles(st, 10, time.Now())
	}
	mux := buildWebUIMux(b, st, Config{})
	etag := pollFlamegraph(b, mux, "").Header().Get("ETag")

	b.Run("cached", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			pollFlamegraph(b, mux, "")
		}
	})
	b.Run("revalidated", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			pollFlamegraph(b, mux, etag)
		}
	})
}

// BenchmarkPollAfterIngest measures polling right after 10 profiles arrived.
func BenchmarkPollAfterIngest(b *testing.B) {
	var st *store.Store
	var mux *http.ServeMux
	reset := func() {
		if st != nil {
			st.Close()
		}
		st = store.New(5*time.Minute, time.Hour, 0)
		for range 10 {
			addTestProfiles(st, 10, time.Now())
		}
		mux = buildWebUIMux(b, st, Config{})
		pollFlamegraph(b, mux, "")
	}

	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		b.StopTimer()
		if i%10 == 0 {
			reset()
		}
		addTestProfiles(st, 10, time.Now())
		b.StartTimer()

		pollFlamegraph(b, mux, "")
	}
	st.Close()
}
