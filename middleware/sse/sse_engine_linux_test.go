//go:build linux

package sse_test

import (
	"bufio"
	"fmt"
	"net"
	"strings"
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
//
// The server is started by startSSEServer, so a Start that fails is
// classified the same way as in the other tests of this package: retried on
// ENOMEM, failed where io_uring works, and skipped only where it does not.
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
			addr, shutdown := startSSEServer(t, tc.engine, tc.async, false, sse.New(sse.Config{
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
			defer shutdown()

			// Open the SSE stream and require at least two `event: tick`
			// lines. The handler ticks every 50 ms, so two events arrive
			// in ~100 ms on real hardware; the deadline only guards
			// against the engine hanging the stream entirely. It is set
			// generously (10 s) because constrained CI runners under the
			// race detector — where io_uring degrades to single-shot and
			// scheduling is slow — occasionally needed more than the old
			// 2 s, producing a flake that never reproduces on real
			// io_uring hardware (verified 20/20 -race on cluster).
			conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
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
