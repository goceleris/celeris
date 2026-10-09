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

// The same adapter that makes Flush a no-op on the native engines also
// returns nil from Write (internal/conn/response.go, h1ResponseAdapter): the
// engine's write hook has no error return, and after the connection is gone
// it drops the bytes. So on epoll and io_uring over HTTP/1.1 neither Write nor
// Flush reports a client that has gone away, which the streaming docs said
// both did (celeris#494 is the SSE middleware's answer: it watches the
// connection through the engine's callbacks). std reports it, from
// net/http. The handler detaches (the only way to stream there), waits for the
// client to reset the connection, then writes and flushes for a while and
// reports the first error it saw.

func TestStreamWriterWriteErrorAfterPeerGoneH1_875(t *testing.T) {
	for _, e := range []struct {
		name string
		eng  celeris.EngineType
	}{{"std", celeris.Std}, {"epoll", celeris.Epoll}, {"io_uring", celeris.IOUring}} {
		t.Run(e.name, func(t *testing.T) {
			gone := make(chan struct{})
			var once sync.Once
			goneNow := func() { once.Do(func() { close(gone) }) }
			t.Cleanup(goneNow)
			type result struct {
				err    error
				writes int
			}
			res := make(chan result, 1)
			addr := startServerConfig761(t, celeris.Config{Engine: e.eng, Protocol: celeris.HTTP1}, func(s *celeris.Server) {
				s.GET("/gone875", func(c *celeris.Context) error {
					sw := c.StreamWriter()
					if sw == nil {
						return fmt.Errorf("no StreamWriter")
					}
					done := c.Detach()
					run := func() {
						defer done()
						if err := sw.WriteHeader(200, [][2]string{{"content-type", "text/plain"}}); err != nil {
							res <- result{err: err}
							return
						}
						if _, err := sw.Write([]byte(first875)); err != nil {
							res <- result{err: err}
							return
						}
						if err := sw.Flush(); err != nil {
							res <- result{err: err}
							return
						}
						<-gone
						time.Sleep(300 * time.Millisecond) // the engine sees the reset
						chunk := make([]byte, 1024)
						n := 0
						for ; n < 80; n++ {
							if _, err := sw.Write(chunk); err != nil {
								res <- result{err: err, writes: n}
								return
							}
							if err := sw.Flush(); err != nil {
								res <- result{err: err, writes: n}
								return
							}
							time.Sleep(25 * time.Millisecond)
						}
						res <- result{writes: n}
					}
					if c.EngineSupportsAsyncDetach() {
						go run()
						return nil
					}
					run()
					return nil
				})
			})
			conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := conn.Write([]byte("GET /gone875 HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
				t.Fatal(err)
			}
			// Read until the first chunk is in, then reset the connection.
			var got strings.Builder
			p := make([]byte, 4096)
			for !strings.Contains(got.String(), first875) {
				_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				n, rerr := conn.Read(p)
				got.Write(p[:n])
				if rerr != nil {
					t.Fatalf("read before the first chunk: %v (got %q)", rerr, got.String())
				}
			}
			_ = conn.(*net.TCPConn).SetLinger(0)
			_ = conn.Close()
			goneNow()

			select {
			case r := <-res:
				native := e.eng != celeris.Std
				t.Logf("%s: first write/flush error after the reset = %v, after %d writes", e.name, r.err, r.writes)
				if native && r.err != nil {
					t.Errorf("native Write/Flush reported the gone client: %v (after %d writes); the docs say they do not", r.err, r.writes)
				}
				if !native && r.err == nil {
					t.Errorf("std Write/Flush never reported the gone client in %d writes", r.writes)
				}
			case <-time.After(15 * time.Second):
				t.Fatal("the handler did not finish")
			}
		})
	}
}
