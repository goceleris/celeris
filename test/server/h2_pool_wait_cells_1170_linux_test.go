//go:build linux

package celeris_test

import (
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"github.com/goceleris/celeris"
)

// celeris#1170, the raw-client cells: what a pool handler's wait for window does
// with a client that reads nothing, a bit, in big steps, or never again.

// pool1170 is the routes of these cells. /pool is an async Blob of n bytes (a body
// the connection's 4 MiB output budget cannot buffer waits in sendRest); /stream is
// an async StreamWriter that writes chunk bytes at a time up to n, and when a Write
// fails notes it in *streamErr and sleeps sleep ms before it returns; /hold takes mu
// across its Blob and /locked takes mu (the wedge of #906's review).
type pool1170 struct {
	streamErr atomic.Value // the error of the last failed Write
	mu        sync.Mutex
}

func (p *pool1170) server(t *testing.T, eng celeris.EngineType, read, idle, write time.Duration) string {
	t.Helper()
	cfg := celeris.Config{Engine: eng, ReadTimeout: read, IdleTimeout: idle, WriteTimeout: write}
	return startServerConfig761(t, cfg, func(s *celeris.Server) {
		s.GET("/pool", func(c *celeris.Context) error {
			n, _ := strconv.Atoi(c.Query("n"))
			return c.Blob(200, "application/octet-stream", make([]byte, n))
		}).Async()
		s.GET("/stream", func(c *celeris.Context) error {
			n, _ := strconv.Atoi(c.Query("n"))
			chunk, _ := strconv.Atoi(c.Query("chunk"))
			sleep, _ := strconv.Atoi(c.Query("sleep"))
			sw := c.StreamWriter()
			if err := sw.WriteHeader(200, [][2]string{{"content-type", "application/octet-stream"}}); err != nil {
				return err
			}
			buf := make([]byte, chunk)
			for sent := 0; sent < n; sent += chunk {
				if _, err := sw.Write(buf); err != nil {
					p.streamErr.Store(err.Error())
					time.Sleep(time.Duration(sleep) * time.Millisecond)
					return nil
				}
			}
			return sw.Close()
		}).Async()
		s.GET("/hold", func(c *celeris.Context) error {
			n, _ := strconv.Atoi(c.Query("n"))
			p.mu.Lock()
			defer p.mu.Unlock()
			return c.Blob(200, "application/octet-stream", make([]byte, n))
		})
		s.GET("/locked", func(c *celeris.Context) error {
			p.mu.Lock()
			p.mu.Unlock() //nolint:staticcheck // waiting for the lock is the point
			return c.String(200, "ok")
		})
		s.GET("/ping", func(c *celeris.Context) error { return c.String(200, "pong") })
		s.GET("/sleep", func(c *celeris.Context) error {
			ms, _ := strconv.Atoi(c.Query("ms"))
			time.Sleep(time.Duration(ms) * time.Millisecond) // a blocking call on the loop
			return c.String(200, "slept")
		})
	})
}

// TestH2PoolWaiterThatGrantsNothingIsReset1170: a client that reads nothing and
// PINGs. The pool handler's wait is reset by the same bound as a buffered tail: window
// 1 for the default window, window 5 for a 1 MiB one (the bytes in flight are the seed).
// The connection stays open: the bound resets the stream.
func TestH2PoolWaiterThatGrantsNothingIsReset1170(t *testing.T) {
	for _, e := range nativeEngines889() {
		for _, tc := range []struct {
			iw     uint32
			lo, hi time.Duration
		}{{0, 900 * time.Millisecond, 2500 * time.Millisecond}, {1 << 20, 4500 * time.Millisecond, 8 * time.Second}} {
			t.Run(fmt.Sprintf("%s/iw%d", e.name, tc.iw>>10), func(t *testing.T) {
				t.Parallel()
				var p pool1170
				c := dialH2raw889(t, p.server(t, e.eng, -1, -1, time.Second), h2dial889{initialWindow: tc.iw})
				c.window889(0, 1<<30-65535) // the connection window is not the limit: the stream's is
				c.request(1, "GET", "/pool?n=8388608", true, true)
				defer c.pingEvery889(200 * time.Millisecond)()
				ev := c.await889(1, 12*time.Second)
				t.Logf("WT1170 grants nothing %s/iw%d: %s", e.name, tc.iw>>10, ev)
				wantWriteReset889(t, ev, tc.lo, tc.hi)
			})
		}
	}
}

// TestH2PoolWaiterDribblerIsReset1170: WINDOW_UPDATE(1) every 100 ms moves nothing.
// Window 2 for the default window, window 5 for a 1 MiB one.
func TestH2PoolWaiterDribblerIsReset1170(t *testing.T) {
	for _, e := range nativeEngines889() {
		for _, tc := range []struct {
			iw     uint32
			lo, hi time.Duration
		}{{0, 1800 * time.Millisecond, 3500 * time.Millisecond}, {1 << 20, 4500 * time.Millisecond, 8 * time.Second}} {
			t.Run(fmt.Sprintf("%s/iw%d", e.name, tc.iw>>10), func(t *testing.T) {
				t.Parallel()
				var p pool1170
				c := dialH2raw889(t, p.server(t, e.eng, -1, -1, time.Second), h2dial889{initialWindow: tc.iw})
				c.window889(0, 1<<30-65535) // the connection window is not the limit: the stream's is
				c.request(1, "GET", "/pool?n=8388608", true, true)
				defer every889(100*time.Millisecond, func() { c.window889(1, 1); c.window889(0, 1) })()
				ev := c.await889(1, 12*time.Second)
				t.Logf("WT1170 dribbler %s/iw%d: %s", e.name, tc.iw>>10, ev)
				wantWriteReset889(t, ev, tc.lo, tc.hi)
			})
		}
	}
}

// TestH2PoolStreamWriterDribblerIsReset1170 (review r2 F2): a pool StreamWriter
// writes 64 KiB at a time, and the client grants each Write's bytes once in five
// windows. A fresh seed of everything in flight for every Write held that for ever,
// at a fifth of the floor. The seed counts what was sent since the last record.
func TestH2PoolStreamWriterDribblerIsReset1170(t *testing.T) {
	for _, e := range nativeEngines889() {
		t.Run(e.name, func(t *testing.T) {
			t.Parallel()
			var p pool1170
			c := dialH2raw889(t, p.server(t, e.eng, -1, -1, time.Second), h2dial889{initialWindow: 1 << 20})
			c.window889(0, 1<<30-65535)
			c.request(1, "GET", "/stream?n=67108864&chunk=65536", true, true)
			defer c.pingEvery889(300 * time.Millisecond)()
			// The client takes the first megabyte at once (its window), then grants 64 KiB
			// every 5 s: a fifth of the 64 KiB per second the floor asks.
			defer every889(5*time.Second, func() { c.window889(1, 65536); c.window889(0, 65536) })()
			ev := c.await889(1, 40*time.Second)
			t.Logf("WT1170 stream dribbler %s: %s", e.name, ev)
			if ev.timeout || !ev.reset || ev.rst != http2.ErrCodeInternal {
				t.Fatalf("want the stream reset with INTERNAL_ERROR (a fifth of the floor is not the floor), got %s", ev)
			}
		})
	}
}

// TestH2PoolWaiterBurstyReaderIsNotClosedByReadTimeout1170 (review r2 decision 2): a
// client that takes a whole 1 MiB window at once and grants it back every 2 s
// (512 KB/s on average, eight times the floor of 64 KiB per 5 s window; a half-window
// updater like curl or nghttp2 with a think time), so its frames are 2 s apart,
// longer than ReadTimeout (1 s), and nothing is queued for it in between. The wait is
// bounded by its rate, so the connection is exempt from the clocks while it lives.
// Measured on the previous head: this client is NOT closed there either (also with
// gaps of 4 s): a conn with a pool handler in flight is shielded by the engines' pool
// gate, whatever ServerOwes says. So the cell is a guard (it is green on main), not
// the red the review expected; the ServerOwes term is pinned by the unit test.
func TestH2PoolWaiterBurstyReaderIsNotClosedByReadTimeout1170(t *testing.T) {
	for _, e := range nativeEngines889() {
		t.Run(e.name, func(t *testing.T) {
			t.Parallel()
			var p pool1170
			addr := p.server(t, e.eng, time.Second, time.Second, 5*time.Second)
			c := dialH2raw889(t, addr, h2dial889{initialWindow: 1 << 20})
			c.window889(0, 1<<30-65535)
			const total = 6 << 20
			c.request(1, "GET", "/pool?n="+strconv.Itoa(total), true, true)
			var got, acked int
			_ = c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
			t0 := time.Now()
			tick := time.Now().Add(2 * time.Second)
			end := ""
			for end == "" {
				// Read as fast as the server sends, until the next grant is due.
				_ = c.conn.SetReadDeadline(tick)
				f, err := c.fr.ReadFrame()
				if err != nil {
					if !isTimeout889(err) {
						end = "conn error: " + err.Error()
						break
					}
					if got > acked {
						c.window889(1, uint32(got-acked))
						c.window889(0, uint32(got-acked))
						acked = got
					}
					tick = time.Now().Add(2 * time.Second)
					if time.Since(t0) > 60*time.Second {
						end = "still open at the limit"
					}
					continue
				}
				switch f := f.(type) {
				case *http2.DataFrame:
					if f.StreamID == 1 {
						got += len(f.Data())
						if f.StreamEnded() {
							end = "complete"
						}
					}
				case *http2.RSTStreamFrame:
					end = "RST " + f.ErrCode.String()
				case *http2.GoAwayFrame:
					end = "GOAWAY " + f.ErrCode.String()
				}
			}
			t.Logf("RT1170 %s: %s, %d bytes at %v", e.name, end, got, time.Since(t0).Round(time.Millisecond))
			if end != "complete" || got != total {
				t.Fatalf("a client granting 1 MiB every 2 s ended %q after %d of %d bytes at %v", end, got, total, time.Since(t0).Round(time.Millisecond))
			}
		})
	}
}

// TestH2PoolWaiterOfASilentClientEndsTheConnection1170: the other side of the same
// decision. A silent client (nothing granted, no frame but a PING-less silence) with
// a waiting pool handler: the handler's wait is reset in window 1+k (5 with a 1 MiB
// window), the stream leaves the connection at the reset, and the connection's
// clocks (all 1 s) close it. Bound: 5 WriteTimeouts + ReadTimeout + slack. Also with
// a handler that goes on sleeping 3 s after the error: a pool handler in flight
// shields its connection from the clocks (the engines' pool gate), as on main, so the
// connection closes when the handler returns (measured: red on main only for that
// reason, it is the same after).
func TestH2PoolWaiterOfASilentClientEndsTheConnection1170(t *testing.T) {
	for _, e := range nativeEngines889() {
		for _, pc := range []struct {
			path  string
			sleep time.Duration // what the handler goes on doing after its error
		}{{"/pool?n=8388608", 0}, {"/stream?n=8388608&chunk=1048576&sleep=3000", 3 * time.Second}} {
			path := pc.path
			t.Run(e.name+path[:5], func(t *testing.T) {
				t.Parallel()
				var p pool1170
				addr := p.server(t, e.eng, time.Second, time.Second, time.Second)
				c := dialH2raw889(t, addr, h2dial889{initialWindow: 1 << 20})
				c.window889(0, 1<<30-65535)
				c.request(1, "GET", path, true, true)
				ev := c.await889(1, 20*time.Second)
				closedAt, closed := c.closedAfter889(20 * time.Second)
				t.Logf("SC1170 %s %s: stream %s; connection closed=%v at %v", e.name, path[:5], ev, closed, closedAt.Round(time.Millisecond))
				if !ev.reset || ev.rst != http2.ErrCodeInternal {
					t.Fatalf("the stream was not reset with INTERNAL_ERROR: %s", ev)
				}
				if lim := 10*time.Second + pc.sleep; !closed || closedAt > lim {
					t.Fatalf("the connection was closed=%v at %v, want within 5 WriteTimeouts + ReadTimeout + the handler's sleep + slack (%v)", closed, closedAt, lim)
				}
			})
		}
	}
}

// TestH2PoolWaiterWithoutAWriteTimeoutKeepsTheOldClocks1170: WriteTimeout -1 is no
// bound on the wait and no exemption: a silent client's connection is closed by
// ReadTimeout, as before.
func TestH2PoolWaiterWithoutAWriteTimeoutKeepsTheOldClocks1170(t *testing.T) {
	for _, e := range nativeEngines889() {
		t.Run(e.name, func(t *testing.T) {
			t.Parallel()
			var p pool1170
			c := dialH2raw889(t, p.server(t, e.eng, time.Second, time.Second, -1), h2dial889{initialWindow: 1 << 20})
			c.window889(0, 1<<30-65535)
			c.request(1, "GET", "/pool?n=8388608", true, true)
			_ = c.await889(1, 20*time.Second)
			closedAt, closed := c.closedAfter889(20 * time.Second)
			t.Logf("NW1170 %s: connection closed=%v at %v", e.name, closed, closedAt.Round(time.Millisecond))
			if !closed || closedAt > 6*time.Second {
				t.Fatalf("closed=%v at %v, want closed by ReadTimeout (1 s) within a few seconds", closed, closedAt)
			}
		})
	}
}

// TestH2PoolWaiterHoldingALockDoesNotWedgeTheServer1170: the wedge of #906's review,
// with a client that reads at a compliant rate. /hold writes 8 MiB while it holds a
// mutex that /locked (sync, on the event loops) waits for; the client has a 1 MiB
// window and reads at 128 KB/s, twice the floor at WriteTimeout 1 s, so the rule alone
// would hold /hold for the whole download (64 s) and the loops with it. The loops are
// stuck in a batch for more than half a WriteTimeout: the wait falls back to
// WriteTimeout in all, and every /locked is answered within 1.5 WriteTimeouts plus
// slack.
func TestH2PoolWaiterHoldingALockDoesNotWedgeTheServer1170(t *testing.T) {
	const timeout = time.Second
	for _, e := range nativeEngines889() {
		t.Run(e.name, func(t *testing.T) {
			var p pool1170
			addr := p.server(t, e.eng, -1, -1, timeout)
			done := make(chan string, 1)
			go func() {
				end, _, at := batched889(t, addr, "/hold?n=8388608", 1<<20, 128<<10, 128e3, 40*time.Second)
				done <- fmt.Sprintf("%s at %v", end, at.Round(time.Millisecond))
			}()
			time.Sleep(300 * time.Millisecond) // /hold is waiting for window with the lock
			t0 := time.Now()
			lockers := get893(8, "http://"+addr+"/locked", timeout+6*time.Second)
			took := time.Since(t0)
			n, sl := answered893(lockers)
			t.Logf("WG1170 %s: /locked %s in %v", e.name, sl, took.Round(time.Millisecond))
			if n != 8 {
				t.Errorf("/locked answered %d/8: the loops waited for the lock a compliant reader's download held", n)
			}
			if took > 3*timeout {
				t.Errorf("the lockers waited %v, want about 1.5 x WriteTimeout (%v) plus slack", took, timeout)
			}
			select {
			case r := <-done:
				t.Logf("WG1170 %s: the /hold download ended: %s", e.name, r)
			case <-time.After(45 * time.Second):
			}
		})
	}
}

// TestH2PoolWaiterIsNotCutWhileALoopIsBlockedBelowTheThreshold1170 and ...Above...:
// the fallback is server-wide and has a threshold (half a WriteTimeout, at least 250
// ms). A route that blocks one loop for 0.3 s at a time (under it) leaves Go's
// Transport 4 x 8 MiB at WriteTimeout 1 s as green as without it; one that blocks 0.8
// s (over) switches the fallback on, and the downloads are bounded by WriteTimeout in
// all, as before celeris#1170: some are reset, as documented.
func TestH2PoolWaiterLoopBlockedThreshold1170(t *testing.T) {
	for _, e := range nativeEngines889() {
		if e.name == "io_uring" {
			continue // its own send clock at WriteTimeout 1 s
		}
		for _, tc := range []struct {
			name  string
			block int // ms a batch blocks
			cut   bool
		}{{"under", 300, false}, {"over", 800, true}} {
			t.Run(e.name+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				var p pool1170
				addr := p.server(t, e.eng, -1, -1, time.Second)
				stop := make(chan struct{})
				var wg sync.WaitGroup
				wg.Add(1)
				go func() { // one connection at a time: one loop is blocked, then not
					defer wg.Done()
					for {
						select {
						case <-stop:
							return
						case <-time.After(300 * time.Millisecond):
						}
						_ = get893(1, "http://"+addr+"/sleep?ms="+strconv.Itoa(tc.block), 5*time.Second)
					}
				}()
				ends, at := downloads1170(t, addr, "/pool", 4, 8<<20, 1e6, 60*time.Second)
				close(stop)
				wg.Wait()
				cut := 0
				for _, r := range ends {
					if r != "complete" {
						cut++
					}
				}
				t.Logf("LB1170 %s/%s: %d cut: %q at %v", e.name, tc.name, cut, ends, at)
				if tc.cut && cut == 0 {
					t.Fatal("every download completed although a loop was blocked for 0.8 s at a time: the fallback to WriteTimeout in all was not applied")
				}
				if !tc.cut && cut != 0 {
					t.Fatalf("%d downloads were cut although the loops were blocked for only 0.3 s at a time (under the threshold of 0.5 s)", cut)
				}
			})
		}
	}
}
