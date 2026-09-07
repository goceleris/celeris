//go:build linux

package websocket

import (
	"fmt"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// TestDetachedCloseLeavesNoResidualHeap is the regression guard for the
// residual heap growth the v1.5.11 24h soak surfaced as I-MEM-1.
//
// The soak failed the absolute gate on exactly one intersection: the
// auth_session_ratelimit refapp -- the ONLY cell of 48 that ever detaches a
// connection -- on io_uring. The same refapp on epoll and std was clean, and
// every other refapp on io_uring was clean. Fitted post-GC trough slope was
// 2.4 KB/s (amd64) and 2.7 KB/s (arm64) against a 1.0 KB/s budget. The growth
// did NOT scale with request volume (amd64 served 5.4x arm64's requests yet
// had the LOWER slope) but did track the ticker-paced WS/SSE establishment
// rate, identical on both hosts: ~156 bytes retained per detach.
//
// The teardown SHAPE matters and an earlier version of this test missed it.
// A client-initiated close after a clean handshake retains ~0.4 B/detach on
// io_uring -- nothing. The validator instead drives six RFC-6455 abuse modes
// that force the SERVER to close (1002/1007), plus a ping flood, plus SSE
// streams held open and then reset mid-stream. Those are different teardown
// paths, and they are what the failing cell actually ran.
//
// Each mode is measured separately so a violation attributes to one shape
// rather than to "WebSockets" in general. Runs on every available engine, so
// epoll -- which the soak established as clean -- is a built-in control.
func TestDetachedCloseLeavesNoResidualHeap(t *testing.T) {
	if testing.Short() {
		t.Skip("drives tens of thousands of upgrade/abuse/teardown cycles")
	}
	// MEASURED BASELINES (msa2-server, amd64, kernel 7.0.0-30, 3000 detaches
	// per mode, server pre-warmed). HeapAlloc B/detach:
	//
	//	mode                    epoll   io_uring
	//	invalid-utf8             56.5*    2.7
	//	unmasked-client          14.6    14.0
	//	continuation-no-start     7.3     3.5
	//	fragmented-reserved      -2.5    -2.6
	//	ping-flood               -8.1    -4.6
	//	  (* first mode on a cold server -- the artifact this warm-up removes)
	//
	// io_uring is at or below epoll on every mode, an order of magnitude
	// under the 156 B/detach the v1.5.11 soak measured. That REFUTES a
	// per-detach retention in the protocol-error teardown path as the cause
	// of that soak's I-MEM-1 failure; this test stands as the guard that it
	// stays refuted.
	const (
		warmCycles           = 500
		cycles               = 3000
		allocBudgetPerDetach = 48
		inuseBudgetPerDetach = 96
	)

	// Each mode returns the bytes to write after a completed handshake. All
	// of them are protocol violations the server must answer by closing, so
	// teardown is server-initiated -- the shape the soak exercised.
	modes := []struct {
		name  string
		frame func() []byte
	}{
		// Text frame carrying invalid UTF-8: RFC 6455 8.1 -> close 1007.
		{"invalid-utf8", func() []byte {
			m := [4]byte{0x11, 0x22, 0x33, 0x44}
			payload := []byte{0xC3, 0x28, 0xA0, 0xA1}
			f := []byte{0x81, 0x80 | byte(len(payload)), m[0], m[1], m[2], m[3]}
			for i, b := range payload {
				f = append(f, b^m[i%4])
			}
			return f
		}},
		// Client frame with the mask bit clear: RFC 6455 5.1 -> close 1002.
		{"unmasked-client", func() []byte {
			return []byte{0x81, 0x03, 'a', 'b', 'c'}
		}},
		// Opcode 0 with no preceding non-final frame -> close 1002.
		{"continuation-no-start", func() []byte {
			m := [4]byte{0x11, 0x22, 0x33, 0x44}
			return []byte{0x80, 0x81, m[0], m[1], m[2], m[3], 'x' ^ m[0]}
		}},
		// Reserved bits set on a fragmented frame -> close 1002.
		{"fragmented-reserved", func() []byte {
			m := [4]byte{0x11, 0x22, 0x33, 0x44}
			return []byte{0x71, 0x81, m[0], m[1], m[2], m[3], 'x' ^ m[0]}
		}},
		// Ping flood: many pings, never reading pongs. The engine must not
		// fan out goroutines or buffer unboundedly.
		{"ping-flood", func() []byte {
			m := [4]byte{0x11, 0x22, 0x33, 0x44}
			var f []byte
			for i := 0; i < 32; i++ {
				f = append(f, 0x89, 0x81, m[0], m[1], m[2], m[3], 'p'^m[0])
			}
			return f
		}},
	}

	for _, kind := range engineKinds(t) {
		kind := kind
		t.Run(kind.String(), func(t *testing.T) {
			addr, shutdown := startNativeServer(t, kind, Config{
				CheckOrigin: func(*celeris.Context) bool { return true },
				Handler: func(c *Conn) {
					for {
						if _, _, err := c.ReadMessage(); err != nil {
							return
						}
					}
				},
			})
			defer shutdown()
			hostPort := strings.TrimSuffix(strings.TrimPrefix(addr, "ws://"), "/ws")

			// Warm the SERVER once, before any mode is measured. Without
			// this the first mode's baseline is taken while the engine is
			// still growing its arenas and worker pools, and that one-time
			// cost is misattributed to it as per-detach retention -- it
			// showed up as 56 B/detach on epoll's first mode with Sys
			// +294 KB, while every later mode on the same engine read under
			// 15 B/detach with Sys flat.
			for i := 0; i < 1500; i++ {
				c, err := net.DialTimeout("tcp", hostPort, 3*time.Second)
				if err != nil {
					continue
				}
				_ = wsHandshake(c, hostPort)
				_ = c.Close()
			}

			settle := func() {
				// io_uring defers some detached-conn release behind a
				// wall-clock backstop; give it room before sampling.
				time.Sleep(2 * time.Second)
				runtime.GC()
				runtime.GC()
			}

			for _, mode := range modes {
				mode := mode
				t.Run(mode.name, func(t *testing.T) {
					cycle := func(n int) int {
						ok := 0
						for i := 0; i < n; i++ {
							c, err := net.DialTimeout("tcp", hostPort, 3*time.Second)
							if err != nil {
								continue
							}
							if err := wsHandshake(c, hostPort); err == nil {
								_ = c.SetWriteDeadline(time.Now().Add(2 * time.Second))
								if _, err := c.Write(mode.frame()); err == nil {
									ok++
								}
								// Let the server observe the violation and
								// initiate its own close before we go away. A
								// prompt close lands well inside this; the
								// deadline only bounds the pathological case,
								// so keep it small -- at these cycle counts a
								// 250ms bound would run for hours.
								_ = c.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
								buf := make([]byte, 256)
								for {
									if _, err := c.Read(buf); err != nil {
										break
									}
								}
							}
							_ = c.Close()
						}
						return ok
					}

					if n := cycle(warmCycles); n < warmCycles/2 {
						t.Fatalf("warm-up completed only %d/%d — environment problem, not a verdict", n, warmCycles)
					}
					settle()
					var before runtime.MemStats
					runtime.ReadMemStats(&before)

					done := cycle(cycles)
					if done < cycles/2 {
						t.Fatalf("completed only %d/%d — environment problem, not a verdict", done, cycles)
					}
					settle()
					var after runtime.MemStats
					runtime.ReadMemStats(&after)

					dAlloc := int64(after.HeapAlloc) - int64(before.HeapAlloc)
					dInuse := int64(after.HeapInuse) - int64(before.HeapInuse)
					perAlloc := float64(dAlloc) / float64(done)
					perInuse := float64(dInuse) / float64(done)

					t.Logf("%s/%s: %d detaches | HeapAlloc %+d B (%.1f B/detach) | HeapInuse %+d B (%.1f B/detach) | Sys %+d B | gor %d",
						kind, mode.name, done, dAlloc, perAlloc, dInuse, perInuse,
						int64(after.Sys)-int64(before.Sys), runtime.NumGoroutine())

					if dAlloc > int64(done*allocBudgetPerDetach) {
						t.Errorf("HeapAlloc grew %d B over %d %s detaches (%.1f B/detach, budget %d): "+
							"live memory retained per detached connection", dAlloc, done, mode.name, perAlloc, allocBudgetPerDetach)
					}
					if dInuse > int64(done*inuseBudgetPerDetach) {
						t.Errorf("HeapInuse grew %d B over %d %s detaches (%.1f B/detach, budget %d): "+
							"heap spans not returned after detached teardown", dInuse, done, mode.name, perInuse, inuseBudgetPerDetach)
					}
				})
			}
		})
	}
	_ = fmt.Sprint
}
