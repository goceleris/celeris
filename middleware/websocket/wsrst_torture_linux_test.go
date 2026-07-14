//go:build linux

package websocket_test

import (
	"context"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/websocket"
)

// TestWSRSTMidUpgradeTorture reproduces the v1.5.7 weekend-soak crash over
// REAL TCP on the native engines with async handlers: many concurrent clients
// upgrade to WebSocket and then abort with a TCP RST (SO_LINGER=0) either
// mid-upgrade or just after, while inbound frame bytes are in flight. That
// drives the engine's OnError -> reader.closeWith concurrently with the
// UpgradeWebSocket data callback's chanReader.Append, AND drives closeConn ->
// CloseH1 concurrently with the async upgrade goroutine's Context.Detach ->
// WSRawWriteFn. On the buggy code this panics ("send on closed channel", then
// a Context/stream use-after-recycle nil-deref) and cascades into pooled-object
// -race corruption; with the fixes it runs clean.
//
// Run with -race (and ideally -gcflags=all=-d=checkptr) to match the soak
// build. Skipped under -short (it runs for several seconds under load). Runs on
// BOTH native engines — the epoll-only variant is what let the io_uring copy of
// the use-after-recycle race slip through review.
func TestWSRSTMidUpgradeTorture(t *testing.T) {
	if testing.Short() {
		t.Skip("torture test; skipped under -short")
	}
	for _, tc := range []struct {
		name   string
		engine celeris.EngineType
	}{
		{"epoll", celeris.Epoll},
		{"iouring", celeris.IOUring},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wsrstTorture(t, tc.engine)
		})
	}
}

func wsrstTorture(t *testing.T, engine celeris.EngineType) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()

	srv := celeris.New(celeris.Config{Engine: engine, AsyncHandlers: true})
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
	var startErr atomic.Pointer[error]
	done := make(chan struct{})
	go func() {
		defer close(done)
		if e := srv.StartWithListenerAndContext(ctx, ln); e != nil {
			startErr.Store(&e)
		}
	}()
	time.Sleep(500 * time.Millisecond) // SO_REUSEPORT rebind settle
	if p := startErr.Load(); p != nil {
		// Docker / minimal-kernel runners may lack io_uring — skip rather
		// than fail (feature-gated path, not a celeris bug).
		msg := (*p).Error()
		if strings.Contains(msg, "io_uring") || strings.Contains(msg, "not available") {
			t.Skipf("engine unavailable on this runner: %v", *p)
		}
		t.Fatalf("server start: %v", *p)
	}
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
	t.Logf("%s: completed %d RST-mid-upgrade cycles across %d workers; no panic", t.Name(), cycles.Load(), workers)
}
