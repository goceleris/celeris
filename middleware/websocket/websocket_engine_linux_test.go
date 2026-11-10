//go:build linux

package websocket_test

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/websocket"
)

// TestWebSocketUpgradeOnNativeEngines is a regression pin for
// celeris#273. Before v1.4.4, async-mode iouring and epoll would
// deadlock the dispatch goroutine on the post-Detach guarded write
// lock when the websocket middleware tried to emit the 101 Switching
// Protocols response — the lock was held by the same goroutine that
// just installed the guarded writeFn.
//
// The test exercises the upgrade path on every (engine, async) pair
// and asserts the server replies "HTTP/1.1 101 Switching Protocols"
// within a 2 s read deadline.
func TestWebSocketUpgradeOnNativeEngines(t *testing.T) {
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
			_, addr, stop := startWS888(t, func() *celeris.Server {
				srv := celeris.New(celeris.Config{
					Engine:        tc.engine,
					AsyncHandlers: tc.async,
				})
				srv.GET("/ws", websocket.New(websocket.Config{
					CheckOrigin: func(c *celeris.Context) bool { return true },
					Handler: func(c *websocket.Conn) {
						for {
							mt, msg, err := c.ReadMessage()
							if err != nil {
								return
							}
							if err := c.WriteMessage(mt, msg); err != nil {
								return
							}
						}
					},
				}))
				return srv
			})
			defer stop()

			conn, err := net.DialTimeout("tcp", addr, 1*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
			req := "GET /ws HTTP/1.1\r\n" +
				"Host: " + addr + "\r\n" +
				"Connection: Upgrade\r\n" +
				"Upgrade: websocket\r\n" +
				"Sec-WebSocket-Version: 13\r\n" +
				"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
				"\r\n"
			if _, err := conn.Write([]byte(req)); err != nil {
				t.Fatal(err)
			}
			br := bufio.NewReader(conn)
			line, err := br.ReadString('\n')
			if err != nil {
				t.Fatalf("read first line: %v", err)
			}
			if !strings.HasPrefix(line, "HTTP/1.1 101") {
				t.Fatalf("expected 101 Switching Protocols, got %q", strings.TrimSpace(line))
			}
		})
	}
}
