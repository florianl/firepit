package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/florianl/firepit/internal/store"
)

// pollFlamegraph requests the flame graph like the web UI does.
func pollFlamegraph(b *testing.B, mux *http.ServeMux) {
	b.Helper()
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/flamegraph", nil))
	if w.Code != http.StatusOK || w.Body.Len() < 1000 {
		b.Fatalf("status = %d with %d bytes", w.Code, w.Body.Len())
	}
}

// BenchmarkPollUnchanged measures polling 100 unchanged profiles.
func BenchmarkPollUnchanged(b *testing.B) {
	st := store.New(5*time.Minute, time.Hour, 0)
	defer st.Close()
	for range 10 {
		addTestProfiles(st, 10)
	}
	mux := buildWebUIMux(b, st, Config{})
	pollFlamegraph(b, mux)

	b.ReportAllocs()
	for b.Loop() {
		pollFlamegraph(b, mux)
	}
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
			addTestProfiles(st, 10)
		}
		mux = buildWebUIMux(b, st, Config{})
		pollFlamegraph(b, mux)
	}

	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		b.StopTimer()
		if i%10 == 0 {
			reset()
		}
		addTestProfiles(st, 10)
		b.StartTimer()

		pollFlamegraph(b, mux)
	}
	st.Close()
}
