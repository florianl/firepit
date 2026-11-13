package main

import (
	"io"
	"net/http"
	"time"

	"github.com/florianl/firepit/internal/store"
)

const (
	// eventMinInterval is the minimum time between change events.
	eventMinInterval = time.Second
	// eventHeartbeat keeps idle streams open through proxies.
	eventHeartbeat = 20 * time.Second
)

// handleEvents streams a "ready" event followed by a "change" event whenever
// the stored profiles change.
func handleEvents(st *store.Store, done <-chan struct{}, minInterval, heartbeat time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		rc := http.NewResponseController(w)

		h := w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("X-Accel-Buffering", "no")

		// Subscribe before "ready" to not miss changes.
		changed := st.Changed()

		send := func(msg string) bool {
			if _, err := io.WriteString(w, msg); err != nil {
				return false
			}
			return rc.Flush() == nil
		}
		if !send("retry: 2000\nevent: ready\ndata:\n\n") {
			return
		}

		ticker := time.NewTicker(heartbeat)
		defer ticker.Stop()

		for {
			select {
			case <-changed:
				changed = st.Changed()
				if !send("event: change\ndata:\n\n") {
					return
				}
				select {
				case <-time.After(minInterval):
				case <-r.Context().Done():
					return
				case <-done:
					return
				}
			case <-ticker.C:
				if !send(": keep-alive\n\n") {
					return
				}
			case <-r.Context().Done():
				return
			case <-done:
				return
			}
		}
	}
}
