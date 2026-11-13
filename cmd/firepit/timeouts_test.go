package main

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestDefaultTimeouts(t *testing.T) {
	server := newHTTPServer(":0", http.NotFoundHandler(), defaultTimeouts)
	if server.ReadHeaderTimeout <= 0 || server.ReadTimeout <= 0 || server.WriteTimeout <= 0 || server.IdleTimeout <= 0 {
		t.Errorf("timeouts are not set: read header %s, read %s, write %s, idle %s",
			server.ReadHeaderTimeout, server.ReadTimeout, server.WriteTimeout, server.IdleTimeout)
	}
}

// serveWithTimeouts starts a server and returns its address.
func serveWithTimeouts(t *testing.T, handler http.Handler, timeouts serverTimeouts) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := newHTTPServer(lis.Addr().String(), handler, timeouts)
	go server.Serve(lis)
	t.Cleanup(func() { server.Close() })
	return lis.Addr().String()
}

// waitForClose returns what the server sent until it closed conn.
func waitForClose(t *testing.T, conn net.Conn) string {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	response, err := io.ReadAll(conn)
	if nerr, ok := errors.AsType[net.Error](err); ok && nerr.Timeout() {
		t.Fatal("server did not close the connection of a stalled client")
	}
	return string(response)
}

func TestServerClosesStalledConnections(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		io.WriteString(w, "ok")
	})
	timeouts := serverTimeouts{
		readHeader: 100 * time.Millisecond,
		read:       200 * time.Millisecond,
		idle:       100 * time.Millisecond,
	}
	addr := serveWithTimeouts(t, handler, timeouts)

	dial := func(t *testing.T) net.Conn {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		return conn
	}

	t.Run("headers never completed", func(t *testing.T) {
		conn := dial(t)
		io.WriteString(conn, "POST /v1/profiles HTTP/1.1\r\nHost: firepit\r\n")
		waitForClose(t, conn)
	})

	t.Run("body never completed", func(t *testing.T) {
		conn := dial(t)
		io.WriteString(conn, "POST /v1/profiles HTTP/1.1\r\nHost: firepit\r\nContent-Length: 100\r\n\r\npartial")
		waitForClose(t, conn)
	})

	t.Run("idle after a request", func(t *testing.T) {
		conn := dial(t)
		io.WriteString(conn, "GET / HTTP/1.1\r\nHost: firepit\r\n\r\n")
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatal(err)
		}
		if body, _ := io.ReadAll(resp.Body); string(body) != "ok" {
			t.Fatalf("body = %q, want ok", body)
		}
		waitForClose(t, conn)
	})

	t.Run("regular request", func(t *testing.T) {
		resp, err := http.Post("http://"+addr+"/v1/profiles", "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if body, _ := io.ReadAll(resp.Body); resp.StatusCode != http.StatusOK || string(body) != "ok" {
			t.Errorf("status = %d, body = %q", resp.StatusCode, body)
		}
	})
}

func TestServerLimitsResponseTime(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		io.WriteString(w, "late")
	})
	timeouts := defaultTimeouts
	timeouts.write = 100 * time.Millisecond
	addr := serveWithTimeouts(t, handler, timeouts)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	io.WriteString(conn, "GET / HTTP/1.1\r\nHost: firepit\r\n\r\n")

	// The handler is not interrupted, but its late response must not be sent.
	time.Sleep(300 * time.Millisecond)
	release <- struct{}{}
	if response := waitForClose(t, conn); response != "" {
		t.Errorf("got response %q after the write timeout", response)
	}
}
