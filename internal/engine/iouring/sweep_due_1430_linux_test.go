//go:build linux

package iouring

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#1430: a connection's deadline is met when it falls due, not at the
// next sweep of the period.
//
// The io_uring worker enforces the idle, read and write timeouts, the HTTP/2
// deadlines and the closing drain in a sweep of its connections. The sweep of an
// idle worker comes about 0.8 s apart (ReadHeaderTimeout above zero: 32 waits of
// 25 ms) or about 1 s apart (ReadHeaderTimeout off: the ceiling, noticed at the
// end of a wait of up to 100 ms), so a timeout fired up to that late. These
// tests state the promise that replaces it: each sweep also computes when the
// next look at a deadline is due, and the loop sweeps then, as well as at the
// sweeps it made before.
//
// The lateness is read from the client's side: the time from the end of the
// response (the server's clock for the connection starts at the last completed
// send) to the close, minus the configured timeout. The bound is the one of the
// epoll fine-sweep tests: no sooner than 50 ms before the due time and no later
// than 200 ms after it, which a loaded CI runner and -race keep (the due sweep
// comes within a wait of 25 ms of its time, or at it).
//
// E1430_TRIALS (a count) raises the number of connections each row measures, and
// every trial is logged as `celeris1430 LATENESS row=... late_ms=...`, so that a
// run on a quiet host can read the distribution from the log.

const (
	early1430 = 50 * time.Millisecond
	late1430  = 200 * time.Millisecond
)

func trials1430(def int) int {
	if v, err := strconv.Atoi(os.Getenv("E1430_TRIALS")); err == nil && v > 0 {
		return v
	}
	return def
}

// closedAfter1430 reads from c until it fails and says how long that took from
// t0, and whether it failed with the connection closed (not a read timeout).
func closedAfter1430(c net.Conn, t0 time.Time, limit time.Duration) (time.Duration, bool) {
	_ = c.SetReadDeadline(t0.Add(limit))
	var b [1]byte
	_, err := c.Read(b[:])
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return 0, false
	}
	return time.Since(t0), true
}

// idleAfterResponse1430: one request on a keep-alive connection, its response
// read, then nothing. Returns the time from the end of the response to the close.
func idleAfterResponse1430(t *testing.T, addr string, wait time.Duration) (time.Duration, bool) {
	t.Helper()
	cl := dialBusy811(t, addr)
	cl.send(t, "/small")
	_ = cl.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(cl.br, nil)
	if err != nil {
		t.Fatalf("first response: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	t0 := time.Now()
	return closedAfter1430(cl.c, t0, wait+3*time.Second)
}

// bodyStalled1430: the head of a POST and a part of its body, then nothing.
// Returns the time from the write to the close.
func bodyStalled1430(t *testing.T, addr string, wait time.Duration) (time.Duration, bool) {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if _, err := io.WriteString(c, "POST /small HTTP/1.1\r\nHost: x\r\nContent-Length: 100\r\n\r\n0123456789"); err != nil {
		t.Fatalf("write: %v", err)
	}
	return closedAfter1430(c, time.Now(), wait+3*time.Second)
}

// headersPartial1430: the first lines of a request, then nothing.
func headersPartial1430(t *testing.T, addr string, wait time.Duration) (time.Duration, bool) {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if _, err := io.WriteString(c, "GET /small HTTP/1.1\r\nHost: x\r\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	return closedAfter1430(c, time.Now(), wait+3*time.Second)
}

// silent1430: a connection that sends nothing, on a server that decides the
// protocol from the first bytes and accepts without the kernel's deferral.
func silent1430(t *testing.T, addr string, wait time.Duration) (time.Duration, bool) {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return closedAfter1430(c, time.Now(), wait+3*time.Second)
}

// TestDeadlineIsMetWhenDue1430 is the state matrix of the promise: one row per
// state of a connection that a timeout ends, each measured on its own
// connections after the worker has been idle (the long waits are engaged). The
// first rows fail where the sweep is the only clock (ReadHeaderTimeout off: the
// sweeps are a second apart; on the default the dense iterations right after a
// request hide it for a timeout below half a second, and a timeout of exactly the
// ceiling lands on a sweep, so that row uses 1.5 s);
// the last two have a kernel timer of their own and pass on the base: they hold
// the promise where it was already kept.
func TestDeadlineIsMetWhenDue1430(t *testing.T) {
	type rowT struct {
		name   string
		wait   time.Duration
		trials int
		cfg    func(c *resource.Config)
		run    func(t *testing.T, addr string, wait time.Duration) (time.Duration, bool)
	}
	off := func(c *resource.Config) {
		c.ReadTimeout, c.IdleTimeout, c.WriteTimeout, c.ReadHeaderTimeout = -1, -1, -1, -1
	}
	rows := []rowT{
		{"idle_read/rht_off", 300 * time.Millisecond, 4, func(c *resource.Config) {
			off(c)
			c.ReadTimeout = 300 * time.Millisecond
		}, idleAfterResponse1430},
		{"idle_idle/rht_off", 300 * time.Millisecond, 4, func(c *resource.Config) {
			off(c)
			c.IdleTimeout = 300 * time.Millisecond
		}, idleAfterResponse1430},
		{"idle_idle/rht_off/async", 300 * time.Millisecond, 3, func(c *resource.Config) {
			off(c)
			c.IdleTimeout = 300 * time.Millisecond
			c.AsyncHandlers = true
		}, idleAfterResponse1430},
		{"idle_read/rht_default", 1500 * time.Millisecond, 3, func(c *resource.Config) {
			off(c)
			c.ReadTimeout = 1500 * time.Millisecond
			c.ReadHeaderTimeout = 0 // the 10 s default
		}, idleAfterResponse1430},
		{"body_stalled/rht_off", 300 * time.Millisecond, 3, func(c *resource.Config) {
			off(c)
			c.ReadTimeout = 300 * time.Millisecond
		}, bodyStalled1430},
		{"headers_partial/kernel_timer", 300 * time.Millisecond, 2, func(c *resource.Config) {
			off(c)
			c.ReadHeaderTimeout = 300 * time.Millisecond
		}, headersPartial1430},
		{"undecided/kernel_timer", 300 * time.Millisecond, 2, func(c *resource.Config) {
			off(c)
			c.Protocol = engine.Auto
			c.DisableDeferAccept = true
			c.ReadHeaderTimeout = 300 * time.Millisecond
		}, silent1430},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			e, addr := startFDLEngine(t, newBusyHandler811(), row.cfg)
			// Idle past the point where the waits are at their longest: 100 empty
			// iterations of 1 and 5 ms, then the long wait.
			time.Sleep(700 * time.Millisecond)
			for i := 0; i < trials1430(row.trials); i++ {
				got, closed := row.run(t, addr, row.wait)
				if !closed {
					t.Fatalf("trial %d: the connection was still open %v after its %v timeout", i, 3*time.Second, row.wait)
				}
				late := got - row.wait
				t.Logf("celeris1430 LATENESS row=%s trial=%d timeout_ms=%d late_ms=%d", row.name, i, row.wait.Milliseconds(), late.Milliseconds())
				if late < -early1430 || late > late1430 {
					t.Errorf("trial %d: closed %v after the end of its activity, %v after its %v due time: want within -%v and +%v "+
						"(the sweep that closes it must come when it falls due, not at the next sweep of the period)",
						i, got, late, row.wait, early1430, late1430)
				}
				// The connection is gone before the next one is measured.
				for end := time.Now().Add(2 * time.Second); e.Metrics().ActiveConnections != 0 && time.Now().Before(end); {
					time.Sleep(5 * time.Millisecond)
				}
			}
		})
	}
}

// TestHeldConnectionsAddNoSweeps1430: 100 idle connections that are not due for
// minutes. Sweeps that fall due are added to the ones the worker made before; a
// held connection that is not due is not a reason to sweep. The bound is the
// rate of today's idle worker (about 1.25 a second) with room for a loaded
// runner: well under the 40 a second of the floor, which a sweep every
// iteration or on every accept would reach. It passes on the base and holds the
// rate where it is.
func TestHeldConnectionsAddNoSweeps1430(t *testing.T) {
	var sweeps atomic.Int64
	hook := func(*Worker) { sweeps.Add(1) }
	testHookCheckTimeouts.Store(&hook)
	t.Cleanup(func() { testHookCheckTimeouts.Store(nil) })

	e, addr := startFDLEngine(t, newBusyHandler811(), func(c *resource.Config) {
		c.ReadTimeout = 10 * time.Minute
		c.IdleTimeout = 10 * time.Minute
		c.WriteTimeout = 10 * time.Minute
	})
	const conns = 100
	var held []net.Conn
	for i := 0; i < conns; i++ {
		c, err := net.DialTimeout("tcp", addr, 3*time.Second)
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		held = append(held, c)
		if _, err := fmt.Fprintf(c, "GET /small HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	t.Cleanup(func() {
		for _, c := range held {
			_ = c.Close()
		}
	})
	for end := time.Now().Add(5 * time.Second); e.Metrics().BytesWritten == 0 && time.Now().Before(end); {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(time.Second) // the answers are out and the loops idle
	const window = 2 * time.Second
	sweeps.Store(0)
	time.Sleep(window)
	n := sweeps.Load()
	workers := int64(e.NumWorkers())
	bound := workers * 4 * int64(window/time.Second) // 4 a second per worker: 3 times today's rate
	t.Logf("celeris1430 HELDSWEEPS workers=%d conns=%d window_ms=%d sweeps=%d bound=%d", workers, conns, window.Milliseconds(), n, bound)
	if n > bound {
		t.Errorf("%d sweeps in %v on %d workers holding %d connections that are not due for 10 minutes, want at most %d",
			n, window, workers, conns, bound)
	}
}

// TestExpiringConnectionsAreSweptAtMostAtTheFloor1430: connections that expire
// all the time (each is idle for 100 ms) make due sweeps, and the rate of those
// is bounded by the floor, as the busy worker's is (TestIouringBusyWorkerSweepIsFloored884):
// a sweep that falls due is never more often than every sweepFloor. It passes on
// the base and holds the sweep rate under variant B's (a sweep per floor) when
// the due sweeps are added.
func TestExpiringConnectionsAreSweptAtMostAtTheFloor1430(t *testing.T) {
	var sweeps atomic.Int64
	hook := func(*Worker) { sweeps.Add(1) }
	testHookCheckTimeouts.Store(&hook)
	t.Cleanup(func() { testHookCheckTimeouts.Store(nil) })

	e, addr := startFDLEngine(t, newBusyHandler811(), func(c *resource.Config) {
		c.ReadTimeout = 100 * time.Millisecond
		c.IdleTimeout = 100 * time.Millisecond
		c.WriteTimeout = -1
		c.ReadHeaderTimeout = -1
	})
	const window = 2 * time.Second
	stop := make(chan struct{})
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		open []net.Conn
	)
	// 4 clients open a connection every 20 ms and leave it; each is closed by the
	// server 100 ms after its request, so connections expire about 200 times a
	// second, spread over the workers.
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				case <-time.After(20 * time.Millisecond):
				}
				c, err := net.DialTimeout("tcp", addr, time.Second)
				if err != nil {
					return
				}
				_, _ = fmt.Fprintf(c, "GET /small HTTP/1.1\r\nHost: x\r\n\r\n")
				mu.Lock()
				open = append(open, c)
				mu.Unlock()
			}
		}()
	}
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range open {
			_ = c.Close()
		}
	})
	time.Sleep(300 * time.Millisecond)
	sweeps.Store(0)
	time.Sleep(window)
	n := sweeps.Load()
	close(stop)
	wg.Wait()
	workers := int64(e.NumWorkers())
	bound := workers * int64(window/(25*time.Millisecond)) * 3
	t.Logf("celeris1430 EXPIRYSWEEPS workers=%d window_ms=%d sweeps=%d bound=%d", workers, window.Milliseconds(), n, bound)
	if n > bound {
		t.Errorf("%d sweeps in %v on %d workers, want at most %d (3 times one per sweepFloor)", n, window, workers, bound)
	}
}
