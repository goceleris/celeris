//go:build linux

package epoll_test

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
	"github.com/goceleris/celeris/middleware/websocket"
)

// TestDetachInlineNoDoubleUnlock is the regression pin for celeris#309:
// the per-handler-async inline dispatch path (celeris#300/#302) crashed the
// process with "fatal error: sync: unlock of unlocked mutex" on the FIRST
// WebSocket upgrade (/ws) or SSE request (/events) on the epoll engine.
//
// Trigger conditions, ALL required (this is why the pre-existing
// middleware/{websocket,sse} engine tests missed it):
//
//   - The server registers at least one .Async() route. This flips
//     Config.AsyncHandlers on for the WHOLE engine (server.go: hasAsyncRoutes
//     → AsyncHandlers=true), so the epoll Loop runs with l.async==true even
//     when Config.AsyncHandlers was left default-false (the "sync" engine
//     label the benchmark uses).
//   - The /ws and /events routes are themselves SYNC (detached flows run
//     async by construction via their middleware goroutine, so they are not
//     marked .Async()).
//   - A fresh keep-alive conn's FIRST request is the /ws or /events one, so
//     it runs INLINE on the event-loop thread (tryInline: l.async &&
//     !cs.asyncPromoted). The inline path never Locks cs.detachMu before the
//     handler runs, but the OnDetach guard used to Unlock it whenever
//     l.async — unlocking a never-locked mutex → fatal.
//
// The pre-existing websocket_engine_linux_test / sse_engine_linux_test set
// AsyncHandlers explicitly and register ONLY the streaming route (no .Async()
// data route), so they never reach the inline-with-async-engine path; this
// test reproduces the exact adapter shape (async data route + sync streaming
// routes) the cluster benchmark crashed on.
//
// Asserts: across every engine the streaming upgrade succeeds AND the server
// is still alive afterwards (a crash would kill the test binary outright; the
// post-streaming request additionally proves the process kept serving).
func TestDetachInlineNoDoubleUnlock(t *testing.T) {
	engines := []struct {
		name   string
		engine celeris.EngineType
	}{
		{"epoll", celeris.Epoll},
		{"iouring", celeris.IOUring},
		{"std", celeris.Std},
	}
	for _, eng := range engines {
		eng := eng
		t.Run(eng.name, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			addr := ln.Addr().String()

			// AsyncHandlers deliberately LEFT DEFAULT (false). The .Async()
			// data route below is what flips the engine into async mode —
			// exactly as the probatorium celeris adapter does with its
			// /db,/cache,/mc,/session driver routes. This combination (async
			// data route + sync streaming routes, AsyncHandlers unset) is what
			// the older streaming engine tests never exercised.
			srv := celeris.New(celeris.Config{Engine: eng.engine})

			// Async data route → forces l.async=true engine-wide.
			srv.GET("/db/user/:id", func(c *celeris.Context) error {
				return c.String(200, "user")
			}).Async()

			// Sync streaming routes (NOT .Async()) — detached flows.
			srv.GET("/ws", websocket.New(websocket.Config{
				CheckOrigin: func(c *celeris.Context) bool { return true },
				Handler: func(c *websocket.Conn) {
					for {
						mt, msg, rerr := c.ReadMessage()
						if rerr != nil {
							return
						}
						if werr := c.WriteMessage(mt, msg); werr != nil {
							return
						}
					}
				},
			}))
			srv.GET("/events", sse.New(sse.Config{
				HeartbeatInterval: -1,
				Handler: func(client *sse.Client) {
					tick := time.NewTicker(50 * time.Millisecond)
					defer tick.Stop()
					ctx := client.Context()
					for n := 0; n < 3; {
						select {
						case <-ctx.Done():
							return
						case <-tick.C:
							n++
							if err := client.Send(sse.Event{Event: "tick", Data: fmt.Sprintf("%d", n)}); err != nil {
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
			time.Sleep(500 * time.Millisecond)
			if p := startErr.Load(); p != nil {
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

			// --- WS upgrade on a FRESH conn (first request → inline path) ---
			t.Run("ws", func(t *testing.T) {
				conn, derr := net.DialTimeout("tcp", addr, time.Second)
				if derr != nil {
					t.Fatal(derr)
				}
				defer func() { _ = conn.Close() }()
				_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
				req := "GET /ws HTTP/1.1\r\nHost: " + addr + "\r\n" +
					"Connection: Upgrade\r\nUpgrade: websocket\r\n" +
					"Sec-WebSocket-Version: 13\r\n" +
					"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n"
				if _, werr := conn.Write([]byte(req)); werr != nil {
					t.Fatal(werr)
				}
				line, rerr := bufio.NewReader(conn).ReadString('\n')
				if rerr != nil {
					t.Fatalf("read 101 line: %v", rerr)
				}
				if !strings.HasPrefix(line, "HTTP/1.1 101") {
					t.Fatalf("expected 101 Switching Protocols, got %q", strings.TrimSpace(line))
				}
			})

			// --- SSE on a FRESH conn (first request → inline path) ---
			t.Run("sse", func(t *testing.T) {
				conn, derr := net.DialTimeout("tcp", addr, time.Second)
				if derr != nil {
					t.Fatal(derr)
				}
				defer func() { _ = conn.Close() }()
				_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
				req := "GET /events HTTP/1.1\r\nHost: " + addr + "\r\nAccept: text/event-stream\r\n\r\n"
				if _, werr := conn.Write([]byte(req)); werr != nil {
					t.Fatal(werr)
				}
				line, rerr := bufio.NewReader(conn).ReadString('\n')
				if rerr != nil {
					t.Fatalf("read status line: %v", rerr)
				}
				if !strings.HasPrefix(line, "HTTP/1.1 200") {
					t.Fatalf("expected 200 for SSE, got %q", strings.TrimSpace(line))
				}
			})

			// --- liveness: the server still serves after streaming ---
			t.Run("alive", func(t *testing.T) {
				conn, derr := net.DialTimeout("tcp", addr, time.Second)
				if derr != nil {
					t.Fatalf("server unreachable after streaming (crashed?): %v", derr)
				}
				defer func() { _ = conn.Close() }()
				_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
				if _, werr := conn.Write([]byte("GET /db/user/42 HTTP/1.1\r\nHost: " + addr + "\r\n\r\n")); werr != nil {
					t.Fatal(werr)
				}
				line, rerr := bufio.NewReader(conn).ReadString('\n')
				if rerr != nil {
					t.Fatalf("server not serving after streaming (crashed?): %v", rerr)
				}
				if !strings.HasPrefix(line, "HTTP/1.1 200") {
					t.Fatalf("expected 200 from async route post-streaming, got %q", strings.TrimSpace(line))
				}
			})
		})
	}
}
