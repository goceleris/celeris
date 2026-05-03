//go:build linux

package websocket

import (
	"context"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	celerisengine "github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/probe"
)

// engineKinds enumerates the native engines under test on Linux. The std
// engine is exercised by the cross-platform tests in websocket_test.go.
//
// IOUring inclusion policy:
//   - kernel < 5.10 (tier=None): skipped — io_uring not usable
//   - kernel 5.10–5.18 (tier=Base/Mid): skipped — celeris falls back to
//     Base tier which lacks multishot recv + provided buffers, the
//     primary code path for v1.3.4's pause/resume; legacy-tier support
//     is best-effort for this release
//   - kernel 5.19+ with provided buffers (tier=High/Optional): included
//
// Note: on kernel 5.15 the probe returns IOUringTier=Mid based on
// version detection, but the Mid tier ring setup itself fails at
// runtime (io_uring_setup: EINVAL) and celeris falls back to Base at
// runtime. We gate on (tier >= High && ProvidedBuffers) which captures
// "tier that actually works reliably with v1.3.4's new code paths".
func engineKinds(t *testing.T) []celeris.EngineType {
	t.Helper()
	kinds := []celeris.EngineType{celeris.Epoll}
	p := probe.Probe()
	if p.IOUringTier >= celerisengine.High && p.ProvidedBuffers {
		kinds = append(kinds, celeris.IOUring)
		t.Logf("io_uring tier=%s kernel=%s — including in test matrix",
			p.IOUringTier.String(), p.KernelVersion)
	} else {
		t.Logf("io_uring tier=%s kernel=%s — skipping IOUring sub-tests (need High+ for v1.3.4 validation)",
			p.IOUringTier.String(), p.KernelVersion)
	}
	return kinds
}

// startNativeServer launches a celeris server with the given native engine
// on a freshly bound listener and returns its address + a shutdown closure.
// Uses StartWithListener so the test owns the port lifecycle.
func startNativeServer(tb testing.TB, kind celeris.EngineType, cfg Config) (string, func()) {
	tb.Helper()
	s := celeris.New(celeris.Config{Engine: kind})
	s.GET("/ws", New(cfg))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	serverCtx, serverCancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.StartWithListenerAndContext(serverCtx, ln) }()

	// 30s deadline (5s tripped on slow GitHub Actions Azure runners with
	// kernel 6.17 io_uring; same pattern as the adaptive H2 dial test).
	addr := waitForReady(tb, s, 30*time.Second)
	return addr, func() {
		serverCancel()
		<-done
	}
}

// TestNativeEngineEcho exercises the engine-integrated WebSocket path
// against each available native engine on Linux. This is the test that
// validates the rewritten chanReader, error propagation, and pause/resume
// plumbing end-to-end against a real event loop.
func TestNativeEngineEcho(t *testing.T) {
	for _, kind := range engineKinds(t) {
		kind := kind
		t.Run(kind.String(), func(t *testing.T) {
			addr, shutdown := startNativeServer(t, kind, Config{
				Handler: func(c *Conn) {
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
			})
			defer shutdown()

			client := dialRaw(t, addr)
			defer client.close()
			client.upgrade(t, "/ws")

			// 100 sequential round-trips.
			for i := 0; i < 100; i++ {
				msg := []byte("native-" + strconv.Itoa(i))
				if err := client.writeClientFrame(true, OpText, msg); err != nil {
					t.Fatalf("iter %d: write: %v", i, err)
				}
				_, op, resp := client.readServerFrame(t)
				if op != OpText || string(resp) != string(msg) {
					t.Fatalf("iter %d: op=%d resp=%q want %q", i, op, resp, msg)
				}
			}

			// Ping/pong.
			if err := client.writeClientFrame(true, OpPing, []byte("hi")); err != nil {
				t.Fatalf("ping write: %v", err)
			}
			_, op, data := client.readServerFrame(t)
			if op != OpPong || string(data) != "hi" {
				t.Fatalf("ping reply: op=%d data=%q", op, data)
			}

			// Binary echo.
			bin := []byte{0, 1, 2, 3, 0xff}
			if err := client.writeClientFrame(true, OpBinary, bin); err != nil {
				t.Fatalf("binary write: %v", err)
			}
			_, op, data = client.readServerFrame(t)
			if op != OpBinary || len(data) != len(bin) {
				t.Fatalf("binary: op=%d len=%d", op, len(data))
			}
		})
	}
}

// TestNativeEngineLargePayload sends a 256 KiB binary frame through the
// native engine path. Stresses the chanReader's chunk-buffering and
// the engine's send queue under multi-buffer payloads.
func TestNativeEngineLargePayload(t *testing.T) {
	for _, kind := range engineKinds(t) {
		kind := kind
		t.Run(kind.String(), func(t *testing.T) {
			addr, shutdown := startNativeServer(t, kind, Config{
				Handler: func(c *Conn) {
					mt, msg, err := c.ReadMessage()
					if err != nil {
						return
					}
					_ = c.WriteMessage(mt, msg)
				},
			})
			defer shutdown()

			client := dialRaw(t, addr)
			defer client.close()
			client.upgrade(t, "/ws")

			payload := make([]byte, 256*1024)
			for i := range payload {
				payload[i] = byte(i)
			}
			if err := client.writeClientFrame(true, OpBinary, payload); err != nil {
				t.Fatalf("write: %v", err)
			}
			_, _, resp := client.readServerFrame(t)
			if len(resp) != len(payload) {
				t.Fatalf("got %d bytes, want %d", len(resp), len(payload))
			}
			for i := range payload {
				if resp[i] != payload[i] {
					t.Fatalf("byte %d differs: got %d, want %d", i, resp[i], payload[i])
				}
			}
		})
	}
}

// TestNativeEngineCompression verifies permessage-deflate works on the
// native engine path (compression negotiation, RSV1 framing, decompress).
func TestNativeEngineCompression(t *testing.T) {
	for _, kind := range engineKinds(t) {
		kind := kind
		t.Run(kind.String(), func(t *testing.T) {
			addr, shutdown := startNativeServer(t, kind, Config{
				EnableCompression: true,
				Handler: func(c *Conn) {
					mt, msg, err := c.ReadMessage()
					if err != nil {
						return
					}
					_ = c.WriteMessage(mt, msg)
				},
			})
			defer shutdown()

			client := dialRaw(t, addr)
			defer client.close()
			client.upgradeWithCompression(t, "/ws")

			text := strings.Repeat("compress me native! ", 50)
			if err := client.writeClientFrame(true, OpText, []byte(text)); err != nil {
				t.Fatalf("write: %v", err)
			}
			_, op, _ := client.readServerFrame(t)
			if op != OpText {
				t.Errorf("op = %d", op)
			}
		})
	}
}

// TestNativeEngineIdleTimeout verifies that IdleTimeout actually fires on
// the native engine path. The middleware sets SetWSIdleDeadline after each
// frame; the engine's checkTimeouts loop closes the conn when expired.
func TestNativeEngineIdleTimeout(t *testing.T) {
	for _, kind := range engineKinds(t) {
		kind := kind
		t.Run(kind.String(), func(t *testing.T) {
			handlerDone := make(chan struct{})
			addr, shutdown := startNativeServer(t, kind, Config{
				IdleTimeout: 200 * time.Millisecond,
				Handler: func(c *Conn) {
					defer close(handlerDone)
					for {
						_, _, err := c.ReadMessage()
						if err != nil {
							return
						}
					}
				},
			})
			defer shutdown()

			client := dialRaw(t, addr)
			defer client.close()
			client.upgrade(t, "/ws")

			// Don't send any frames — the handler should exit when the
			// engine closes the conn after IdleTimeout expires.
			select {
			case <-handlerDone:
				// success
			case <-time.After(3 * time.Second):
				t.Fatal("idle timeout did not fire")
			}
		})
	}
}

// TestNativeEngineWriteErrorPropagation verifies that engine-side I/O
// failures (peer reset / EPIPE) are surfaced to the WS handler via Read
// returning a non-EOF error.
func TestNativeEngineWriteErrorPropagation(t *testing.T) {
	for _, kind := range engineKinds(t) {
		kind := kind
		t.Run(kind.String(), func(t *testing.T) {
			readErr := make(chan error, 1)
			addr, shutdown := startNativeServer(t, kind, Config{
				Handler: func(c *Conn) {
					_, _, err := c.ReadMessage()
					readErr <- err
				},
			})
			defer shutdown()

			client := dialRaw(t, addr)
			client.upgrade(t, "/ws")
			// Hard close the client without sending close frame.
			if tc, ok := client.conn.(*net.TCPConn); ok {
				_ = tc.SetLinger(0)
			}
			_ = client.conn.Close()

			select {
			case err := <-readErr:
				if err == nil {
					t.Fatal("expected non-nil error after peer RST")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("handler did not see read error")
			}
		})
	}
}

// TestNativeEngineBackpressure verifies that a slow consumer applies
// real TCP-level backpressure on the native engine path. Without the
// pause/resume wiring, the chanReader would overflow and the connection
// would be killed; with backpressure, the kernel TCP window closes and
// the producer naturally slows down.
func TestNativeEngineBackpressure(t *testing.T) {
	for _, kind := range engineKinds(t) {
		kind := kind
		t.Run(kind.String(), func(t *testing.T) {
			var (
				processed atomic.Uint32
				dropped   atomic.Uint64
				wg        sync.WaitGroup
			)
			wg.Add(1)
			addr, shutdown := startNativeServer(t, kind, Config{
				MaxBackpressureBuffer: 32, // small buffer to exercise pause faster
				Handler: func(c *Conn) {
					defer wg.Done()
					for {
						_, _, err := c.ReadMessage()
						if err != nil {
							dropped.Store(c.BackpressureDropped())
							return
						}
						processed.Add(1)
						time.Sleep(2 * time.Millisecond) // slow consumer
					}
				},
			})
			defer shutdown()

			client := dialRaw(t, addr)
			defer client.close()
			client.upgrade(t, "/ws")

			// Flood with 200 small messages — far above the 32-slot buffer.
			const total = 200
			for i := 0; i < total; i++ {
				if err := client.writeClientFrame(true, OpText, []byte("flood")); err != nil {
					t.Fatalf("flood write %d: %v", i, err)
				}
			}
			_ = client.bw.Flush()

			// Give the slow handler time to process them all.
			deadline := time.Now().Add(5 * time.Second)
			for processed.Load() < total && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}

			// Close the client cleanly so the handler returns and we can
			// inspect the dropped counter.
			_ = client.conn.Close()
			wg.Wait()

			if processed.Load() != total {
				t.Errorf("processed %d/%d messages — connection was dropped under backpressure", processed.Load(), total)
			}
			if dropped.Load() > 0 {
				t.Errorf("backpressure should not drop chunks: dropped=%d", dropped.Load())
			}
		})
	}
}
