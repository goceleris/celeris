//go:build linux

package iouring

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/resource"
)

// celeris#884 items 65 and 75: the cadence of io_uring's timeout sweep
// (checkTimeouts) on the real run loop.
//
// The sweep was gated on an iteration count (every 32 iterations with
// ReadHeaderTimeout > 0 or a detached conn, every 1024 otherwise), not on
// time. An idle iteration waits up to 25 / 50 / 100 ms (baseTimeout), so on an
// idle worker the gap between sweeps was 0.8 s, 1.6 s or about 102 s, and
// ReadTimeout and IdleTimeout fired that late with ReadHeaderTimeout off
// (item 65). A busy worker's iterations are microseconds apart, so the same
// gate swept thousands of times a second (item 75, the busy direction; epoll's
// twin was celeris#885 item 29, fixed by a 25 ms floor).
//
// A SEND the worker submits last is waited for with no timeout when it is
// small (mode 3a, waitUntimed): the sweep never runs while it is outstanding,
// so a small SEND into a socket that is full is not bounded by WriteTimeout
// (item 74).

// sweepProbeWait is how long the idle-conn arms wait for the conn to go. The
// bound the assertions use is a few seconds; E884_PROBE_WAIT (seconds) raises
// it to see how late main is (about 93 to 102 s with ReadHeaderTimeout off).
func sweepProbeWait() time.Duration {
	if v, err := strconv.Atoi(os.Getenv("E884_PROBE_WAIT")); err == nil && v > 0 {
		return time.Duration(v) * time.Second
	}
	return 8 * time.Second
}

// TestIouringIdleConnTimeoutIsNotGatedByIterations884 is item 65's remaining
// half on the real path: an idle keep-alive conn, ReadTimeout or IdleTimeout
// 2.5 s, ReadHeaderTimeout off (the gate is then 1024 iterations of 100 ms).
// The conn must be gone within a few seconds of going idle, not about 100 s.
// The rht_default arms are the controls: the gate is 32 iterations of 25 ms
// there (0.8 s), so they pass on main.
func TestIouringIdleConnTimeoutIsNotGatedByIterations884(t *testing.T) {
	// 2.5 s, not less: the response this arm reads is a SEND the worker waited
	// for in mode 3a, which armed the standing sweep timer (armSweepTick), so
	// the first sweep after it comes about a second later whatever the gate
	// says. Only a conn that outlives that sweep tells the ceiling (sweepDue)
	// from the timer.
	const timeout = 2500 * time.Millisecond
	for _, async := range []bool{false, true} {
		for _, tc := range []struct {
			name         string
			read, idle   time.Duration
			rht          time.Duration // 0: the 10 s default; -1: off
			controlsMain bool
		}{
			{"read/rht_off", timeout, -1, -1, false},
			{"idle/rht_off", -1, timeout, -1, false},
			{"read/rht_default", timeout, -1, 0, true},
			{"idle/rht_default", -1, timeout, 0, true},
		} {
			if tc.controlsMain && testing.Short() {
				continue // the controls pass on main; -short keeps the arms that fail there
			}
			name := fmt.Sprintf("async=%v/%s", async, tc.name)
			t.Run(name, func(t *testing.T) {
				e, addr := startFDLEngine(t, newBusyHandler811(), func(c *resource.Config) {
					c.AsyncHandlers = async
					c.ReadTimeout = tc.read
					c.IdleTimeout = tc.idle
					c.ReadHeaderTimeout = tc.rht
					c.WriteTimeout = -1
				})
				cl := dialBusy811(t, addr)
				cl.send(t, "/small")
				_ = cl.c.SetReadDeadline(time.Now().Add(5 * time.Second))
				resp, err := http.ReadResponse(cl.br, nil)
				if err != nil {
					t.Fatalf("first response: %v", err)
				}
				_ = resp.Body.Close()
				t0 := time.Now()
				wait := sweepProbeWait()
				var gone time.Duration
				for time.Since(t0) < wait {
					time.Sleep(20 * time.Millisecond)
					if e.Metrics().ActiveConnections == 0 {
						gone = time.Since(t0)
						break
					}
				}
				t.Logf("celeris884 IDLESWEEP arm=%s timeout_ms=%d gone_ms=%d (0: still open after %d ms)",
					name, timeout.Milliseconds(), gone.Milliseconds(), wait.Milliseconds())
				if gone == 0 {
					t.Errorf("celeris#884 item 65: an idle conn with timeout %v (ReadHeaderTimeout %v) was still open %v after "+
						"it went idle: the sweep's gate is iterations, an idle iteration waits up to 100 ms",
						timeout, tc.rht, wait)
				}
			})
		}
	}
}

// TestIouringBusyWorkerSweepIsFloored884 is item 75: with 16 clients keeping
// two workers busy, the sweep must not run once per gate stride of iterations
// (thousands of times a second), but about once per sweepFloor. The count is of
// checkTimeouts calls, through testHookCheckTimeouts.
func TestIouringBusyWorkerSweepIsFloored884(t *testing.T) {
	for _, rht := range []time.Duration{0, -1} {
		t.Run(fmt.Sprintf("rht=%v", rht), func(t *testing.T) {
			var sweeps atomic.Int64
			hook := func(*Worker) { sweeps.Add(1) }
			testHookCheckTimeouts.Store(&hook)
			t.Cleanup(func() { testHookCheckTimeouts.Store(nil) })

			e, addr := startFDLEngine(t, newBusyHandler811(), func(c *resource.Config) {
				c.ReadHeaderTimeout = rht
				c.ReadTimeout = 30 * time.Second
				c.IdleTimeout = 30 * time.Second
				c.WriteTimeout = 30 * time.Second
			})
			const clients = 16
			const window = 1500 * time.Millisecond
			var served atomic.Int64
			stop := make(chan struct{})
			var wg sync.WaitGroup
			for i := 0; i < clients; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					c, err := net.Dial("tcp", addr)
					if err != nil {
						return
					}
					defer func() { _ = c.Close() }()
					br := bufio.NewReader(c)
					for {
						select {
						case <-stop:
							return
						default:
						}
						if _, err := fmt.Fprintf(c, "GET /small HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
							return
						}
						resp, err := http.ReadResponse(br, nil)
						if err != nil {
							return
						}
						_ = resp.Body.Close()
						served.Add(1)
					}
				}()
			}
			time.Sleep(200 * time.Millisecond) // every conn is up
			sweeps.Store(0)
			s0 := served.Load()
			time.Sleep(window)
			n, s1 := sweeps.Load(), served.Load()-s0
			close(stop)
			wg.Wait()
			workers := e.NumWorkers()
			// One sweep per sweepFloor per worker, with a sweep the idle path
			// owes on top and a generous factor for a loaded runner.
			bound := int64(workers) * int64(window/(25*time.Millisecond)) * 3
			t.Logf("celeris884 BUSYSWEEP rht=%v workers=%d window_ms=%d requests=%d sweeps=%d bound=%d",
				rht, workers, window.Milliseconds(), s1, n, bound)
			if s1 < 1000 {
				t.Fatalf("premise: only %d requests served in the window: the workers were not busy", s1)
			}
			if n > bound {
				t.Errorf("celeris#884 item 75: %d timeout sweeps in %v on %d busy workers (%d requests), want at most %d: "+
					"the gate counts iterations, which are microseconds apart on a busy worker", n, window, workers, s1, bound)
			}
		})
	}
}

// TestIouringSmallSendIntoAFullSocketIsBoundedByWriteTimeout884 is item 74
// end to end, with WriteTimeout 1 s and ReadTimeout, IdleTimeout and
// ReadHeaderTimeout off, so only the write side can end the conn. The conn
// must be gone within the bound of the 811 arms (WriteTimeout, the closing
// drain, the hold).
//
// sync: the server's send buffer is its minimum (4 KiB), the client has a 4 KiB
// receive buffer and never reads, and sends one request of a 1000-byte
// response after another, waiting for each response's SEND to complete (the
// engine's BytesWritten). The kernel takes the first ones; the SEND that finds
// the socket full makes no progress at all (a SEND that took part of its
// response would be a remainder, which is marked). The client stops there, so
// that SEND, far under longSendBytes and so unmarked, is the worker's last
// submission, waited for with no timeout (mode 3a, waitUntimed). On main no
// iteration and no sweep ran for it and the conn stayed open.
//
// async_leftover is the control, and passes on main: the handler's direct write
// takes what the socket takes and the rest (under longSendBytes) is a first
// SEND that makes no progress, but the dirty pass's give-up sets sendWatch for
// an async conn (celeris#811), which has the loop sweep about every second.
func TestIouringSmallSendIntoAFullSocketIsBoundedByWriteTimeout884(t *testing.T) {
	const bound = 20 * time.Second
	cfg := func(async bool) func(c *resource.Config) {
		return func(c *resource.Config) {
			c.AsyncHandlers = async
			c.Resources.SocketSend = 4096
			c.WriteTimeout = time.Second
			c.ReadTimeout = -1
			c.IdleTimeout = -1
			c.ReadHeaderTimeout = -1
		}
	}
	waitGone := func(e *Engine) time.Duration {
		t0 := time.Now()
		for time.Since(t0) < bound {
			time.Sleep(20 * time.Millisecond)
			if e.Metrics().ActiveConnections == 0 {
				return time.Since(t0)
			}
		}
		return 0
	}
	t.Run("sync", func(t *testing.T) {
		// The response sizes are tried in turn, each on an engine of its own
		// that is stopped (its subtest ends) before the next: whether the SEND
		// that finds the socket full makes no progress at all or takes a few
		// bytes (a remainder, which is marked) depends on the kernel's buffer
		// accounting. A size that does not give the zero-progress SEND is
		// logged and left; the arm fails when none does, as a skip would take
		// the witness out of CI silently.
		found := false
		for _, resp := range []int{1000, 300, 4096, 2000, 600} {
			if found {
				break
			}
			t.Run(fmt.Sprintf("resp=%d", resp), func(t *testing.T) {
				e, addr := startFDLEngine(t, &busyHandler811{body: make([]byte, resp)}, cfg(false))
				cl := dialBusy811(t, addr)
				stuckAt, delta := 0, uint64(0)
				for i := 1; i <= 600 && stuckAt == 0; i++ {
					before := e.Metrics().BytesWritten
					cl.send(t, "/big")
					delta = 0
					for d := time.Now().Add(300 * time.Millisecond); time.Now().Before(d); time.Sleep(2 * time.Millisecond) {
						if delta = e.Metrics().BytesWritten - before; delta >= uint64(resp) {
							break
						}
					}
					if delta >= uint64(resp) {
						continue
					}
					// Nothing more in 300 ms is not proof: a worker that was
					// not scheduled on a loaded runner looks the same, and its
					// SEND then completes and leaves an idle conn that no
					// timeout of this config ends. Look again after a second.
					time.Sleep(time.Second)
					if delta = e.Metrics().BytesWritten - before; delta < uint64(resp) {
						stuckAt = i
					}
				}
				if stuckAt == 0 || delta != 0 {
					t.Logf("celeris884 SMALLSEND arm=sync resp_bytes=%d premise not met (stuck_at_request=%d, the stuck SEND took %d bytes)",
						resp, stuckAt, delta)
					return
				}
				found = true
				gone := waitGone(e)
				t.Logf("celeris884 SMALLSEND arm=sync stuck_at_request=%d resp_bytes=%d write_timeout_ms=1000 gone_ms=%d (0: still open after %d ms)",
					stuckAt, resp, gone.Milliseconds(), bound.Milliseconds())
				if gone == 0 {
					t.Errorf("celeris#884 item 74: a conn whose client stopped reading (a %d-byte SEND into a full socket that made "+
						"no progress, the worker's last submission) was still open %v in with WriteTimeout 1s", resp, bound)
				}
			})
		}
		if !found {
			t.Fatal("premise: no response size gave a SEND into a full socket that made no progress on this kernel, " +
				"so this arm would check nothing")
		}
	})
	t.Run("async_leftover", func(t *testing.T) {
		const resp = 48 << 10
		e, addr := startFDLEngine(t, &busyHandler811{body: make([]byte, resp)}, cfg(true))
		cl := dialBusy811(t, addr)
		cl.send(t, "/big")
		gone := waitGone(e)
		t.Logf("celeris884 SMALLSEND arm=async_leftover resp_bytes=%d write_timeout_ms=1000 gone_ms=%d (0: still open after %d ms)",
			resp, gone.Milliseconds(), bound.Milliseconds())
		if gone == 0 {
			t.Errorf("celeris#884 item 74: an async conn whose client reads none of a %d-byte response was still open %v in "+
				"with WriteTimeout 1s", resp, bound)
		}
	})
}

// TestIouringPausedWorkerSweepsAboutEverySecond884: a worker with no listener
// (a paused one: baseTimeout is 1 s whatever its empty-iteration count) must
// sweep about once a second too. A conn that has just been served on such a
// worker, IdleTimeout 2 s, ReadHeaderTimeout, ReadTimeout and WriteTimeout off,
// must be gone within about 4 s. With the clock of the batch taken for the
// first empty iterations (sweepClock) the gap was up to 8 s and the conn went
// at up to 9.9 s (review r2 of #1114).
func TestIouringPausedWorkerSweepsAboutEverySecond884(t *testing.T) {
	const bound = 4500 * time.Millisecond
	// How late the gap is depends on where the iteration counter stands when the
	// worker goes quiet, which the number of requests moves; the unit rows of
	// TestSweepClock884 are the deterministic witness, these arms the real path.
	for _, extra := range []int{0, 3, 4, 7} {
		t.Run(fmt.Sprintf("extra_requests=%d", extra), func(t *testing.T) {
			e, addr := startFDLEngine(t, newBusyHandler811(), func(c *resource.Config) {
				c.DisableDeferAccept = true
				c.IdleTimeout = 2 * time.Second
				c.ReadTimeout = -1
				c.ReadHeaderTimeout = -1
				c.WriteTimeout = -1
			})
			cl := dialBusy811(t, addr)
			time.Sleep(50 * time.Millisecond)
			if err := e.PauseAccept(); err != nil {
				t.Fatalf("PauseAccept: %v", err)
			}
			time.Sleep(300 * time.Millisecond)
			for i := 0; i <= extra; i++ {
				cl.send(t, "/small")
				_ = cl.c.SetReadDeadline(time.Now().Add(5 * time.Second))
				resp, err := http.ReadResponse(cl.br, nil)
				if err != nil {
					t.Fatalf("response %d: %v", i, err)
				}
				_ = resp.Body.Close()
				time.Sleep(time.Duration(37*i) * time.Millisecond)
			}
			t0 := time.Now()
			var gone time.Duration
			for time.Since(t0) < 15*time.Second {
				time.Sleep(20 * time.Millisecond)
				if e.Metrics().ActiveConnections == 0 {
					gone = time.Since(t0)
					break
				}
			}
			t.Logf("celeris884 PAUSEDSWEEP extra_requests=%d idle_timeout_ms=2000 gone_ms=%d (0: still open after 15000 ms)", extra, gone.Milliseconds())
			if gone == 0 || gone > bound {
				t.Errorf("celeris#884: a conn on a paused worker with IdleTimeout 2s was gone after %v (0: still open), want within %v: "+
					"the worker's waits are 1 s, so its sweeps must come about every second", gone, bound)
			}
		})
	}
}
