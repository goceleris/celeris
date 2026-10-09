//go:build linux

package celeris_test

import (
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// Tests for celeris#875. On HTTP/1.1 StreamWriter.Flush does nothing on the
// native engines (internal/conn/response.go, h1ResponseAdapter.Flush returns
// nil: "write() is synchronous"), and the write it relies on is not
// synchronous there: before Detach the epoll and io_uring write hooks only
// append to the connection's send buffer, which goes out when the handler
// returns, and a handler that blocks holds the bytes it has written back.
// After Detach each Write sends on the calling goroutine. The std engine
// flushes through http.Flusher. The docs said Flush "guarantees the bytes are
// on the wire" (docs streaming.md) and its godoc "ensures buffered data is
// sent to the network"; they now say what is measured here.
//
// The handler writes the head and one chunk, calls Flush, then waits for the
// test to release it. The client reports whether the chunk reached it before
// the release (early) and then that the whole body arrives once released
// (the control: a case that never delivered would otherwise read as "late").

const (
	first875  = "first-875"
	second875 = "second-875"
	// early875 is how long the client waits for the chunk before it calls the
	// engine "held back". Loopback delivers in microseconds; an engine that
	// sends on Write answers inside this window with a wide margin even under
	// -race, and one that holds the bytes back never does.
	early875 = 750 * time.Millisecond
)

// flushRoute875 registers /flush875: head, one chunk, Flush, wait for
// release, a second chunk, Close. With detach it follows the canonical
// pattern of the streaming docs: Detach, then a goroutine on an engine that
// supports it, the handler's own goroutine on std.
func flushRoute875(s *celeris.Server, release <-chan struct{}, detach, async bool) {
	r := s.GET("/flush875", func(c *celeris.Context) error {
		sw := c.StreamWriter()
		if sw == nil {
			return fmt.Errorf("no StreamWriter")
		}
		var done func()
		if detach {
			done = c.Detach()
		}
		run := func() {
			if done != nil {
				defer done()
			}
			defer func() { _ = sw.Close() }()
			if err := sw.WriteHeader(200, [][2]string{{"content-type", "text/plain"}}); err != nil {
				return
			}
			if _, err := sw.Write([]byte(first875)); err != nil {
				return
			}
			if err := sw.Flush(); err != nil {
				return
			}
			select {
			case <-release:
			case <-time.After(20 * time.Second):
			}
			_, _ = sw.Write([]byte(second875))
		}
		if detach && c.EngineSupportsAsyncDetach() {
			go run()
			return nil
		}
		run()
		return nil
	})
	if async {
		r.Async()
	}
}

// flushProbe875 requests /flush875 with Connection: close and reports
// whether the first chunk arrived within early875, then, after release is
// called, the whole chunked response up to its last chunk.
func flushProbe875(addr string, release func()) (early bool, full string, err error) {
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return false, "", err
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte("GET /flush875 HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")); err != nil {
		return false, "", err
	}
	var mu sync.Mutex
	var buf strings.Builder
	seenFirst := make(chan struct{})
	var once sync.Once
	readDone := make(chan error, 1)
	go func() {
		p := make([]byte, 4096)
		for {
			_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
			n, rerr := conn.Read(p)
			mu.Lock()
			buf.Write(p[:n])
			got := buf.String()
			mu.Unlock()
			if strings.Contains(got, first875) {
				once.Do(func() { close(seenFirst) })
			}
			// The chunked body ends with the last chunk. A detached
			// connection is not closed by the server after it, so the read
			// does not wait for EOF.
			if strings.Contains(got, second875) && strings.HasSuffix(got, "0\r\n\r\n") {
				readDone <- nil
				return
			}
			if rerr != nil {
				if rerr == io.EOF {
					rerr = nil
				}
				readDone <- rerr
				return
			}
		}
	}()
	select {
	case <-seenFirst:
		early = true
	case <-time.After(early875):
	}
	release()
	if rerr := <-readDone; rerr != nil {
		mu.Lock()
		defer mu.Unlock()
		return early, buf.String(), rerr
	}
	mu.Lock()
	defer mu.Unlock()
	return early, buf.String(), nil
}

func TestStreamWriterFlushH1_875(t *testing.T) {
	for _, e := range []struct {
		name string
		eng  celeris.EngineType
	}{{"std", celeris.Std}, {"epoll", celeris.Epoll}, {"io_uring", celeris.IOUring}} {
		for _, sh := range []struct {
			name  string
			async bool
		}{{"sync", false}, {"async-route", true}} {
			for _, detach := range []bool{false, true} {
				name := fmt.Sprintf("%s/%s/detach=%v", e.name, sh.name, detach)
				t.Run(name, func(t *testing.T) {
					release := make(chan struct{})
					var once sync.Once
					rel := func() { once.Do(func() { close(release) }) }
					t.Cleanup(rel)
					addr := startServerConfig761(t, celeris.Config{Engine: e.eng, Protocol: celeris.HTTP1}, func(s *celeris.Server) {
						flushRoute875(s, release, detach, sh.async)
					})
					early, full, err := flushProbe875(addr, rel)
					// std flushes through http.Flusher. A native engine sends on
					// Write only after Detach; before it the chunk waits for the
					// handler to return, Flush or not.
					want := e.eng == celeris.Std || detach
					t.Logf("%s: chunk before the handler returned = %v (want %v)", name, early, want)
					if err != nil {
						t.Fatalf("read: %v (got %q)", err, full)
					}
					if early != want {
						t.Errorf("chunk written and flushed arrived before the handler returned = %v, want %v", early, want)
					}
					if !strings.HasPrefix(full, "HTTP/1.1 200") || !strings.Contains(full, first875) || !strings.Contains(full, second875) {
						t.Fatalf("response incomplete after release: %q", full)
					}
				})
			}
		}
	}
}
