package main

import (
	"fmt"
	"io"
	"net/http"
	"runtime"
	"runtime/metrics"
	"strings"
	"time"

	"github.com/florianl/firepit/internal/receiver"
	"github.com/florianl/firepit/internal/store"
)

// handleHealth reports that the process serves requests.
func handleHealth(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet, http.MethodHead) {
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, "ok\n")
}

// metricWriter formats metrics in the Prometheus text format.
type metricWriter struct {
	b strings.Builder
}

// family writes the HELP and TYPE lines of a metric.
func (w *metricWriter) family(name, kind, help string) {
	fmt.Fprintf(&w.b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
}

// sample writes a single sample. labels are name and value pairs.
func (w *metricWriter) sample(name string, value any, labels ...string) {
	w.b.WriteString(name)
	if len(labels) > 0 {
		w.b.WriteByte('{')
		for i := 0; i+1 < len(labels); i += 2 {
			if i > 0 {
				w.b.WriteByte(',')
			}
			fmt.Fprintf(&w.b, "%s=%q", labels[i], labels[i+1])
		}
		w.b.WriteByte('}')
	}
	fmt.Fprintf(&w.b, " %v\n", value)
}

// metric writes a metric with a single sample.
func (w *metricWriter) metric(name, kind, help string, value any) {
	w.family(name, kind, help)
	w.sample(name, value)
}

// handleMetrics serves the metrics of firepit in the Prometheus text format.
func handleMetrics(st *store.Store, recv *receiver.Metrics, started time.Time) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		if !allowMethods(rw, r, http.MethodGet, http.MethodHead) {
			return
		}

		var w metricWriter

		w.family("firepit_receiver_requests_total", "counter",
			"Export requests handled by the receivers, by protocol and result. "+
				"The result is accepted, or the reason of the rejection: invalid or too_large.")
		for _, c := range recv.Requests() {
			w.sample("firepit_receiver_requests_total", c.Count, "protocol", c.Protocol, "result", c.Result)
		}

		m := st.Metrics()
		w.metric("firepit_store_requests_dropped_total", "counter",
			"Accepted export requests that were discarded for lacking a dictionary.", m.DroppedRequests)
		w.metric("firepit_store_profiles", "gauge",
			"Profiles currently stored.", m.Profiles)
		w.metric("firepit_store_sample_types", "gauge",
			"Sample types of the profiles currently stored.", m.SampleTypes)
		w.metric("firepit_store_bytes", "gauge",
			"Size accounted for the profiles currently stored.", m.Bytes)
		w.metric("firepit_store_limit_bytes", "gauge",
			"Limit of firepit_store_bytes above which the oldest profiles are evicted; 0 if unlimited.", m.MaxBytes)
		w.metric("firepit_store_profiles_added_total", "counter",
			"Profiles added to the store.", m.AddedProfiles)
		w.family("firepit_store_profiles_removed_total", "counter",
			"Profiles removed from the store, by reason: expired after the retention time, "+
				"or evicted to meet the memory limit.")
		w.sample("firepit_store_profiles_removed_total", m.ExpiredProfiles, "reason", "expired")
		w.sample("firepit_store_profiles_removed_total", m.EvictedProfiles, "reason", "evicted")

		heapSample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
		metrics.Read(heapSample)
		w.metric("firepit_heap_objects_bytes", "gauge",
			"Memory occupied by live and not yet collected objects on the Go heap.", heapSample[0].Value.Uint64())
		w.metric("firepit_goroutines", "gauge",
			"Number of goroutines.", runtime.NumGoroutine())
		w.metric("firepit_start_time_seconds", "gauge",
			"Start time of the process as Unix time.", started.Unix())

		rw.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		io.WriteString(rw, w.b.String())
	}
}
