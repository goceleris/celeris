//go:build linux

package websocket_test

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/websocket"
)

// TestWSRSTMidUpgradeTorture reproduces the v1.5.7 weekend-soak crash over
// REAL TCP on the epoll engine with async handlers: many concurrent clients
// upgrade to WebSocket and then abort with a TCP RST (SO_LINGER=0) either
// mid-upgrade or just after, while inbound frame bytes are in flight. That
// drives the engine's OnError -> reader.closeWith concurrently with the
// UpgradeWebSocket data callback's chanReader.Append. On the buggy reader
// (which close(r.ch)'d) this panics with "send on closed channel" and kills
// the process; with the done-channel fix it runs clean.
//
// Run with -race (and ideally -gcflags=all=-d=checkptr) to match the soak
// build. Skipped under -short (it runs for several seconds under load).
func TestWSRSTMidUpgradeTorture(t *testing.T) {
	if testing.Short() {
		t.Skip("torture test; skipped under -short")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()

	srv := celeris.New(celeris.Config{Engine: celeris.Epoll, AsyncHandlers: true})
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

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.StartWithListenerAndContext(ctx, ln) }()
	time.Sleep(500 * time.Millisecond) // SO_REUSEPORT rebind settle
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	}()

	req := "GET /ws HTTP/1.1\r\nHost: " + addr + "\r\n" +
		"Connection: Upgrade\r\nUpgrade: websocket\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n"
	// A masked client text frame "hello" (mask key 0x00000000 → payload as-is).
	frame := []byte{0x81, 0x85, 0x00, 0x00, 0x00, 0x00, 'h', 'e', 'l', 'l', 'o'}
	reqBytes := []byte(req)

	const workers = 32
	deadline := time.Now().Add(6 * time.Second)
	var cycles atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for time.Now().Before(deadline) {
				c, err := net.DialTimeout("tcp", addr, time.Second)
				if err != nil {
					continue
				}
				tc, _ := c.(*net.TCPConn)
				if tc != nil {
					_ = tc.SetLinger(0) // Close() emits RST, not FIN
				}
				if w%2 == 0 {
					// RST during upgrade: request + frame together, then abort.
					_, _ = c.Write(append(append([]byte{}, reqBytes...), frame...))
					_ = c.Close()
				} else {
					// RST just after upgrade: let the handler start reading,
					// deliver a frame, then abort mid-read.
					_, _ = c.Write(reqBytes)
					time.Sleep(200 * time.Microsecond)
					_, _ = c.Write(frame)
					_ = c.Close()
				}
				cycles.Add(1)
			}
		}(w)
	}
	wg.Wait()
	t.Logf("completed %d RST-mid-upgrade cycles across %d workers; server did not panic", cycles.Load(), workers)
}
