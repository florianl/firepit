package main

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/florianl/firepit/internal/store"
)

// newEventServer starts a test server streaming the events of st.
func newEventServer(t *testing.T, st *store.Store, minInterval, heartbeat time.Duration) *httptest.Server {
	t.Helper()
	done := make(chan struct{})
	server := httptest.NewServer(handleEvents(st, done, minInterval, heartbeat))
	t.Cleanup(func() {
		close(done)
		server.Close()
	})
	return server
}

// eventStream returns the events and comments received from srv.
func eventStream(t *testing.T, url string) <-chan string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	events := make(chan string, 16)
	go func() {
		defer close(events)
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if name, ok := strings.CutPrefix(line, "event: "); ok {
				events <- name
			} else if strings.HasPrefix(line, ":") {
				events <- "comment"
			}
		}
	}()
	return events
}

func expectEvent(t *testing.T, events <-chan string, want string) {
	t.Helper()
	select {
	case got, ok := <-events:
		if !ok {
			t.Fatalf("stream ended, want event %q", want)
		}
		if got != want {
			t.Fatalf("got event %q, want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no event within 5s, want %q", want)
	}
}

func expectNoEvent(t *testing.T, events <-chan string, d time.Duration) {
	t.Helper()
	select {
	case got, ok := <-events:
		if ok {
			t.Fatalf("got unexpected event %q", got)
		}
		t.Fatal("stream ended unexpectedly")
	case <-time.After(d):
	}
}

func TestEventsOnStoreChanges(t *testing.T) {
	st := store.New(time.Hour, 20*time.Millisecond, 0)
	defer st.Close()
	server := newEventServer(t, st, 100*time.Millisecond, time.Hour)

	events := eventStream(t, server.URL)
	expectEvent(t, events, "ready")
	expectNoEvent(t, events, 150*time.Millisecond)

	addTestProfiles(st, 1, time.Now())
	expectEvent(t, events, "change")

	// A burst of changes is coalesced into one event.
	for range 5 {
		addTestProfiles(st, 1, time.Now())
	}
	expectEvent(t, events, "change")
	expectNoEvent(t, events, 250*time.Millisecond)

	addTestProfiles(st, 1, time.Now().Add(-2*time.Hour))
	expectEvent(t, events, "change") // added
	expectEvent(t, events, "change") // expired and removed
	expectNoEvent(t, events, 250*time.Millisecond)
}

func TestEventsHeartbeat(t *testing.T) {
	st := store.New(time.Hour, time.Hour, 0)
	defer st.Close()
	server := newEventServer(t, st, time.Second, 50*time.Millisecond)

	events := eventStream(t, server.URL)
	expectEvent(t, events, "ready")
	expectEvent(t, events, "comment")
	expectEvent(t, events, "comment")
}

// Open event streams must not keep the server from shutting down.
func TestEventsEndOnShutdown(t *testing.T) {
	st := store.New(time.Hour, time.Hour, 0)
	defer st.Close()

	mux, stop := buildWebUI(st, nil, Config{})
	server := httptest.NewUnstartedServer(mux)
	server.Config.RegisterOnShutdown(stop)
	server.Start()
	t.Cleanup(server.Close)

	events := eventStream(t, server.URL+"/api/events")
	expectEvent(t, events, "ready")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Config.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown with an open event stream: %v", err)
	}
	if _, ok := <-events; ok {
		t.Error("event stream still open after shutdown")
	}
}
