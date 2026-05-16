//go:build linux

package sse_test

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/sse"
)

// TestSSEOnNativeEngines is a regression pin for celeris#273. Before
// v1.4.4, an SSE handler running under iouring or epoll would hang
// because the user handler ran inline on the worker thread (or the
// async dispatch goroutine), preventing the engine from flushing the
// chunked-encoded events. Async-mode iouring/epoll additionally
// deadlocked the dispatch goroutine on the post-Detach guarded write
// lock.
//
// The test boots celeris with each native engine + each AsyncHandlers
// setting, drives /events for 1 s, and expects at least two ticks to
// reach the client. A timeout-and-no-events outcome is the bug.
func TestSSEOnNativeEngines(t *testing.T) {
	cases := []struct {
		name   string
		engine celeris.EngineType
		async  bool
	}{
		{"iouring/async", celeris.IOUring, true},
		{"iouring/sync", celeris.IOUring, false},
		{"epoll/async", celeris.Epoll, true},
		{"epoll/sync", celeris.Epoll, false},
		{"std/async", celeris.Std, true},
		{"std/sync", celeris.Std, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			addr := ln.Addr().String()

			srv := celeris.New(celeris.Config{
				Engine:        tc.engine,
				AsyncHandlers: tc.async,
			})
			srv.GET("/events", sse.New(sse.Config{
				HeartbeatInterval: -1,
				Handler: func(client *sse.Client) {
					tick := time.NewTicker(50 * time.Millisecond)
					defer tick.Stop()
					ctx := client.Context()
					for n := 0; n < 5; {
						select {
						case <-ctx.Done():
							return
						case <-tick.C:
							n++
							if err := client.Send(sse.Event{
								Event: "tick",
								Data:  fmt.Sprintf("%d", n),
							}); err != nil {
								return
							}
						}
					}
				},
			}))
			var startErr atomic.Pointer[error]
			startCtx, startCancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				defer close(done)
				if e := srv.StartWithListenerAndContext(startCtx, ln); e != nil {
					startErr.Store(&e)
				}
			}()
			// iouring/epoll rebind via SO_REUSEPORT to acquire workers
			// on their own listening FDs — they close the listener we
			// passed in and re-bind to the same port. The transient
			// kernel-side flip can briefly accept and immediately RST
			// a probe connection during the window. Sleep half a
			// second to let the engine settle before driving the
			// real test. Mirrors the wait in cmd/runner and the
			// repro binary that proves the fix on cluster hardware.
			time.Sleep(500 * time.Millisecond)
			if p := startErr.Load(); p != nil {
				// Docker / minimal-kernel CI runners may lack io_uring
				// support (e.g. seccomp filtering, kernel <5.1 emulation).
				// Skip rather than fail — the test exercises a
				// kernel-feature-gated path and the failure mode is
				// "engine unavailable", not a celeris bug.
				msg := (*p).Error()
				if strings.Contains(msg, "io_uring") || strings.Contains(msg, "not available") {
					t.Skipf("engine unavailable on this runner: %v", *p)
				}
				t.Fatalf("server start: %v", *p)
			}
			defer func() {
				startCancel()
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Log("server goroutine did not exit within 3s")
				}
			}()

			// Open the SSE stream with a 2 s read deadline; require at
			// least two `event: tick` lines.
			conn, err := net.DialTimeout("tcp", addr, 1*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
			req := "GET /events HTTP/1.1\r\n" +
				"Host: " + addr + "\r\n" +
				"Accept: text/event-stream\r\n" +
				"Connection: keep-alive\r\n" +
				"\r\n"
			if _, err := conn.Write([]byte(req)); err != nil {
				t.Fatal(err)
			}
			br := bufio.NewReader(conn)
			events := 0
			for events < 2 {
				line, err := br.ReadString('\n')
				if err != nil {
					t.Fatalf("read after %d events: %v", events, err)
				}
				if strings.HasPrefix(line, "event: tick") {
					events++
				}
			}
			if events < 2 {
				t.Fatalf("only %d SSE events received; engine hung the stream", events)
			}
		})
	}
}
