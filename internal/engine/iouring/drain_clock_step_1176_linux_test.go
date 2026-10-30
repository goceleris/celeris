//go:build linux

package iouring

import (
	"context"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"github.com/goceleris/celeris/internal/drainclock"
)

// celeris#1176. The shutdown's deadlines (the send drain, the wait for HTTP/2
// pool handlers, the gate's windows, the bounded waits for the kernel's
// completions) were read from the wall clock, so a step of it during a drain
// moved them. These tests step the wall clock of the drainclock seam, which a
// test cannot do to the machine's: the clock the seam supplies has a monotonic
// reading that runs as it does and a wall reading that steps by step every
// `every`, from the first time a shutdown reads it. A forward step of a minute
// and a quarter every 50 ms is more than any drain here lasts, so a deadline
// that is read from the wall clock is crossed within the first 50 ms; a
// backward one pushes it out of reach for as long.
//
// Each case asserts both bounds. "Not cut early" alone is passed by a drain
// that never ends, which is what a clock on the wrong scale does.

var clockEpoch1176 = time.Now()

const (
	stepSize1176  = 75 * time.Second
	stepEvery1176 = 50 * time.Millisecond
)

// stairClock1176 installs the stepped clock for the test and returns the
// function that takes it out again, which a test calls before it waits for an
// engine that a regression would leave in a drain that the stepped clock keeps
// open (it is also a Cleanup).
func stairClock1176(t *testing.T, step time.Duration) (restore func()) {
	t.Helper()
	var first atomic.Int64
	r := drainclock.SetSeam(func() drainclock.Reading {
		now := time.Now()
		mono := int64(now.Sub(clockEpoch1176)) + 1
		first.CompareAndSwap(0, mono)
		n := (mono - first.Load()) / int64(stepEvery1176)
		return drainclock.Reading{Wall: now.UnixNano() + n*int64(step), Mono: mono}
	})
	t.Cleanup(r)
	return r
}

// waitDone1176 waits for Listen to return, up to bound, and reports how long
// after t0 it did and whether it did.
func waitDone1176(run *engineRun896, t0 time.Time, bound time.Duration) (time.Duration, bool) {
	select {
	case <-run.done:
		return time.Since(t0), true
	case <-time.After(bound):
		return time.Since(t0), false
	}
}

// bigRequest1176 dials, asks for the 32 MiB /big response and waits for its
// first byte, so the worker has the rest in flight to a peer that reads no
// more.
func bigRequest1176(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if _, err := c.Write([]byte("GET /big HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(c, make([]byte, 1)); err != nil {
		t.Fatalf("no byte of the /big response within 10 s: %v", err)
	}
	_ = c.SetReadDeadline(time.Time{})
	return c
}

// A slow reader's response is not cut by a forward step of the wall clock: the
// drain runs past its floor (the worker stops accepting then, cancelAccept) and
// the client reads all of it. The budget is 5 s and nothing else bounds the
// drain, so what ends it is the client having read everything.
func TestSendDrainIsNotCutByAForwardClockStep1176(t *testing.T) {
	restore := stairClock1176(t, stepSize1176)
	calls := seam896(t, false)
	h := newHoldHandler896(true)
	run := startHold896(t, freeAddr639(t), 2, 0, h)
	c := bigRequest1176(t, run.e.Addr().String())

	budget, cancelBudget := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelBudget()
	t0 := time.Now()
	_ = run.e.Shutdown(budget)
	run.cancel()
	if !waitSeam896(calls, 0) {
		restore()
		_ = c.Close()
		<-run.done
		t.Fatalf("the drain ended before the worker stopped accepting, %v after the shutdown began: "+
			"the stepped wall clock ended it (gave up %d)", time.Since(t0), run.e.metrics.handoffLoss.shutdownSendDrainGaveUp.Load())
	}
	if n := run.e.metrics.handoffLoss.shutdownSendDrainGaveUp.Load(); n != 0 {
		t.Fatalf("the send drain gave up %d times while the budget had %v left", n, time.Until(t0.Add(5*time.Second)))
	}
	_ = c.SetReadDeadline(time.Now().Add(15 * time.Second))
	got, err := io.Copy(io.Discard, c)
	if err != nil {
		t.Fatalf("reading the response: %v (after %d bytes)", err, got)
	}
	if got < 32<<20 {
		t.Fatalf("the client read %d bytes of a %d byte response: the drain cut it", got, 32<<20)
	}
	if _, ok := waitDone1176(run, t0, 5*time.Second); !ok {
		t.Fatal("Listen did not return after the client had read the response")
	}
	if n := run.e.metrics.handoffLoss.shutdownSendDrainGaveUp.Load(); n != 0 {
		t.Fatalf("the send drain gave up %d times with the whole response taken", n)
	}
}

// A peer that never reads holds the drain to its bound and no shorter and no
// longer, whichever way the wall clock steps: the bound is the budget's
// deadline, or WriteTimeout after the drain began, whichever comes first.
func TestSendDrainBoundIsTheMonotonicClocksWhateverTheWallDoes1176(t *testing.T) {
	const budget = 1500 * time.Millisecond
	for _, tc := range []struct {
		name         string
		step         time.Duration
		budget       func() (context.Context, context.CancelFunc)
		writeTimeout time.Duration
	}{
		{"forward-budget-deadline", stepSize1176, func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), budget)
		}, 0},
		{"backward-budget-deadline", -stepSize1176, func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), budget)
		}, 0},
		{"forward-write-timeout", stepSize1176, func() (context.Context, context.CancelFunc) {
			return context.WithCancel(context.Background())
		}, budget},
		{"backward-write-timeout", -stepSize1176, func() (context.Context, context.CancelFunc) {
			return context.WithCancel(context.Background())
		}, budget},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restore := stairClock1176(t, tc.step)
			h := newHoldHandler896(true)
			run := startHold896(t, freeAddr639(t), 2, tc.writeTimeout, h)
			c := bigRequest1176(t, run.e.Addr().String())
			ctx, cancel := tc.budget()
			defer cancel()
			t0 := time.Now()
			_ = run.e.Shutdown(ctx)
			run.cancel()
			took, ok := waitDone1176(run, t0, budget+3*time.Second)
			if !ok {
				restore()
				_ = c.Close()
				<-run.done
				t.Fatalf("Listen had not returned %v after the shutdown began, for a drain bounded at %v: "+
					"a wall clock stepped %v kept it open", took, budget, tc.step)
			}
			gave := run.e.metrics.handoffLoss.shutdownSendDrainGaveUp.Load()
			t.Logf("%s: Listen returned %v after the shutdown began (bound %v), send drain gave up %d", tc.name, took, budget, gave)
			if took < budget-300*time.Millisecond {
				t.Fatalf("the drain ended after %v, before its %v bound: the stepped wall clock ended it", took, budget)
			}
			if gave != 1 {
				t.Fatalf("the send drain gave up %d times, want 1: the peer took nothing", gave)
			}
		})
	}
}

// The wait for an HTTP/2 stream handler is not ended by a forward step either:
// the handler still runs 600 ms in, past the wait's 250 ms floor and the 50 ms
// steps, and when it ends its response reaches the client, on the connection
// the wait kept open (it used to be closed under the handler).
func TestPoolWaitIsNotCutByAForwardClockStep1176(t *testing.T) {
	restore := stairClock1176(t, stepSize1176)
	h := newHoldHandler896(false)
	release := releaseOnce1176(h)
	defer release()
	run := startHold896(t, freeAddr639(t), 2, 0, h)
	c := h2Request896(t, run.e.Addr().String(), "/hold")
	defer func() { _ = c.Close() }()
	select {
	case <-h.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the /hold handler never ran")
	}
	budget, cancelBudget := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelBudget()
	_ = run.e.Shutdown(budget)
	run.cancel()
	time.Sleep(600 * time.Millisecond)
	select {
	case <-run.done:
		restore()
		t.Fatal("Listen returned with the HTTP/2 handler still running and 4 s of the budget left")
	default:
	}
	release()
	fr := http2.NewFramer(c, c)
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		f, err := fr.ReadFrame()
		if err != nil {
			restore()
			t.Fatalf("the handler's response never reached the client: %v (the wait ended before the handler did)", err)
		}
		if d, ok := f.(*http2.DataFrame); ok && d.StreamID == 1 && d.StreamEnded() {
			break
		}
	}
	if _, ok := waitDone1176(run, time.Now(), 5*time.Second); !ok {
		t.Fatal("Listen did not return after the handler ended")
	}
}

// The gate of the pool wait opens 250 ms after the wait began, and not before:
// an idle HTTP/1 connection is closed by it, so one that is still open 150 ms
// in says the gate has not been raised. A gate read from the wrong clock scale
// is raised at once.
func TestPoolWaitGateWaitsItsWindowWhateverTheWallDoes1176(t *testing.T) {
	for _, step := range []time.Duration{stepSize1176, -stepSize1176} {
		t.Run(step.String(), func(t *testing.T) {
			restore := stairClock1176(t, step)
			h := newHoldHandler896(false)
			release := releaseOnce1176(h)
			defer release()
			run := startHold896(t, freeAddr639(t), 2, 0, h)
			addr := run.e.Addr().String()
			c := h2Request896(t, addr, "/hold")
			defer func() { _ = c.Close() }()
			select {
			case <-h.started:
			case <-time.After(5 * time.Second):
				t.Fatal("the /hold handler never ran")
			}
			// An HTTP/1 keep-alive conn that has been answered and is idle.
			idle, err := net.DialTimeout("tcp", addr, 2*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = idle.Close() }()
			if _, err := idle.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, 4096)
			_ = idle.SetReadDeadline(time.Now().Add(5 * time.Second))
			if n, err := idle.Read(buf); err != nil || n == 0 {
				t.Fatalf("the idle conn's first response: %d bytes, %v", n, err)
			}
			budget, cancelBudget := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancelBudget()
			t0 := time.Now()
			_ = run.e.Shutdown(budget)
			run.cancel()
			_ = idle.SetReadDeadline(t0.Add(150 * time.Millisecond))
			if n, err := idle.Read(buf); err == nil || !isTimeout1176(err) {
				restore()
				t.Fatalf("the idle HTTP/1 conn was closed %v into the pool wait (read %d, %v): the gate was raised before its 250 ms window",
					time.Since(t0), n, err)
			}
			// And the window does end: the gate closes it well before the budget.
			_ = idle.SetReadDeadline(t0.Add(3 * time.Second))
			if _, err := idle.Read(buf); err == nil || isTimeout1176(err) {
				restore()
				t.Fatalf("the idle HTTP/1 conn was still open %v into the pool wait: the gate never opened (%v)", time.Since(t0), err)
			}
			restore()
			release()
			if _, ok := waitDone1176(run, t0, 5*time.Second); !ok {
				t.Fatal("Listen did not return after the handler ended")
			}
		})
	}
}

// releaseOnce1176 is h's release, closed at most once, so a test that fails
// before the handler is released still lets it go.
func releaseOnce1176(h *holdHandler896) func() {
	var once sync.Once
	return func() { once.Do(func() { close(h.release) }) }
}

func isTimeout1176(err error) bool {
	ne, ok := err.(net.Error)
	return ok && ne.Timeout()
}

// The shutdown's waits for the kernel to complete what it owes (the cancel of
// the listen socket's accept, the ops owed on a connection's descriptor) are
// each bounded by shutdownFDDrainNanos on the monotonic clock. The ring wait is
// answered with nothing, as TestShutdownWaitForTheAcceptIsBounded896 does, so
// both waits run to their bounds, one after the other: a stepped wall clock
// ends each in the first 50 ms, so the sum is the witness that neither was.
func TestShutdownFDWaitsAreBoundedByTheMonotonicClock1176(t *testing.T) {
	for _, step := range []time.Duration{stepSize1176, -stepSize1176} {
		t.Run(step.String(), func(t *testing.T) {
			orig := shutdownRingWait
			shutdownRingWait = func(_ *Ring, d time.Duration) error { time.Sleep(d); return nil }
			t.Cleanup(func() { shutdownRingWait = orig })
			restore := stairClock1176(t, step)

			run := start896(t, freeAddr639(t), 2)
			// An answered keep-alive conn, whose recv is armed: an op the kernel owes.
			c, err := net.DialTimeout("tcp", run.e.Addr().String(), 2*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			if _, err := c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
				t.Fatal(err)
			}
			_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
			if n, err := c.Read(make([]byte, 4096)); err != nil || n == 0 {
				t.Fatalf("first response: %d bytes, %v", n, err)
			}
			t0 := time.Now()
			run.cancel()
			bound := time.Duration(shutdownFDDrainNanos)
			took, ok := waitDone1176(run, t0, 2*bound+3*time.Second)
			if !ok {
				restore()
				<-run.done
				t.Fatalf("Listen had not returned %v after the cancel: a wall clock stepped %v kept a wait for the kernel open", took, step)
			}
			t.Logf("Listen returned %v after the cancel, two bounds of %v", took, bound)
			if took < 2*bound-100*time.Millisecond {
				t.Fatalf("Listen returned after %v, before the %v the two waits take: the stepped wall clock ended one of them", took, 2*bound)
			}
			if n := run.e.metrics.handoffLoss.shutdownListenCancelGaveUp.Load(); n == 0 {
				t.Error("the accept's cancel was not counted as given up")
			}
		})
	}
}
