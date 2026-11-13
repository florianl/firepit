package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/florianl/firepit/internal/receiver"
	"github.com/florianl/firepit/internal/store"
)

// scrape returns the samples served at path by name and labels and checks the format.
func scrape(t *testing.T, mux *http.ServeMux, path string) map[string]string {
	t.Helper()
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: status = %d", path, w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Errorf("Content-Type = %q", ct)
	}

	sampleRE := regexp.MustCompile(`^([a-z_]+)(\{[a-z_]+="[^"]*"(,[a-z_]+="[^"]*")*\})? (\d+)$`)
	samples := make(map[string]string)
	described := make(map[string]bool)
	for line := range strings.SplitSeq(strings.TrimSuffix(w.Body.String(), "\n"), "\n") {
		if name, ok := strings.CutPrefix(line, "# TYPE "); ok {
			described[strings.Fields(name)[0]] = true
			continue
		}
		if strings.HasPrefix(line, "# HELP ") {
			continue
		}
		m := sampleRE.FindStringSubmatch(line)
		if m == nil {
			t.Errorf("malformed line: %q", line)
			continue
		}
		if !described[m[1]] {
			t.Errorf("sample of %s precedes its TYPE", m[1])
		}
		samples[m[1]+m[2]] = m[4]
	}
	return samples
}

func TestMetrics(t *testing.T) {
	st := store.New(time.Hour, time.Hour, 0)
	defer st.Close()
	recv := &receiver.Metrics{}
	mux, stop := buildWebUI(st, recv, Config{})
	defer stop()

	ingest := receiver.NewHTTPHandler(st, 1<<20, recv)
	post := func(body string) {
		ingest(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/profiles", strings.NewReader(body)))
	}

	before := scrape(t, mux, "/metrics")
	for name, want := range map[string]string{
		`firepit_receiver_requests_total{protocol="http",result="accepted"}`: "0",
		`firepit_store_profiles`:             "0",
		`firepit_store_profiles_added_total`: "0",
		`firepit_store_limit_bytes`:          "0",
	} {
		if got := before[name]; got != want {
			t.Errorf("empty store: %s = %q, want %s", name, got, want)
		}
	}

	// Dictionary-less requests are accepted and dropped by the store.
	dictionary := `"dictionary":{"stringTable":["","cpu","alloc"]}`
	post(`{"resourceProfiles":[{"scopeProfiles":[{"profiles":[
		{"sampleType":{"typeStrindex":1},"timeUnixNano":"1"},
		{"sampleType":{"typeStrindex":2},"timeUnixNano":"1"}]}]}],` + dictionary + `}`)
	post(`{"resourceProfiles":[{"scopeProfiles":[{"profiles":[{"sampleType":{"typeStrindex":1},"timeUnixNano":"1"}]}]}],` + dictionary + `}`)
	post(`{"resourceProfiles":[{"scopeProfiles":[{"profiles":[{}]}]}]}`)
	post(`not json`)
	ingest(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/profiles", bytes.NewReader(make([]byte, 2<<20))))

	after := scrape(t, mux, "/metrics")
	for name, want := range map[string]string{
		`firepit_receiver_requests_total{protocol="http",result="accepted"}`:  "3",
		`firepit_receiver_requests_total{protocol="http",result="invalid"}`:   "1",
		`firepit_receiver_requests_total{protocol="http",result="too_large"}`: "1",
		`firepit_receiver_requests_total{protocol="grpc",result="accepted"}`:  "0",
		`firepit_store_requests_dropped_total`:                                "1",
		`firepit_store_profiles`:                                              "3",
		`firepit_store_sample_types`:                                          "2",
		`firepit_store_profiles_added_total`:                                  "3",
		`firepit_store_profiles_removed_total{reason="expired"}`:              "0",
		`firepit_store_profiles_removed_total{reason="evicted"}`:              "0",
	} {
		if got := after[name]; got != want {
			t.Errorf("%s = %q, want %s", name, got, want)
		}
	}
	for _, name := range []string{"firepit_store_bytes", "firepit_heap_objects_bytes", "firepit_goroutines", "firepit_start_time_seconds"} {
		if got := after[name]; got == "" || got == "0" {
			t.Errorf("%s = %q, want a positive value", name, got)
		}
	}
}

func TestMetricsWithoutReceiver(t *testing.T) {
	st := store.New(time.Hour, time.Hour, 0)
	defer st.Close()
	samples := scrape(t, buildWebUIMux(st, Config{BasePath: "/firepit"}), "/firepit/metrics")
	if _, ok := samples["firepit_store_profiles"]; !ok {
		t.Error("firepit_store_profiles is missing")
	}
}

func TestMetricLabelEscaping(t *testing.T) {
	var w metricWriter
	w.sample("m", 1, "a", "plain", "b", "quote\" backslash\\ newline\n")
	if got, want := w.b.String(), `m{a="plain",b="quote\" backslash\\ newline\n"} 1`+"\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestHealth(t *testing.T) {
	st := store.New(time.Hour, time.Hour, 0)
	defer st.Close()

	tests := []struct {
		basePath string
		path     string
		method   string
		want     int
	}{
		{"", "/healthz", http.MethodGet, http.StatusOK},
		{"", "/healthz", http.MethodHead, http.StatusOK},
		{"", "/healthz", http.MethodPost, http.StatusMethodNotAllowed},
		{"/firepit", "/firepit/healthz", http.MethodGet, http.StatusOK},
		// Probes need not know the base path.
		{"/firepit", "/healthz", http.MethodGet, http.StatusOK},
	}
	for _, tt := range tests {
		mux := buildWebUIMux(st, Config{BasePath: tt.basePath})
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(tt.method, tt.path, nil))
		if w.Code != tt.want {
			t.Errorf("%s %s with base path %q: status = %d, want %d", tt.method, tt.path, tt.basePath, w.Code, tt.want)
		}
	}
}
