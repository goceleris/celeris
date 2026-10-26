//go:build linux

package iouring

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// celeris#884 item 72: ringWaitContinues had no ceiling. A ring wait that
// answers a retryable error (EBUSY, EAGAIN) for good, with nothing in the CQ
// ring to reap, is a ring that is failing, not one whose CQ ring is full; the
// loop retried it for ever (a 50 us sleep per pass), where before PR #1026 it
// shut the worker down on the first error. The ceiling restores that end for a
// run of about a second of retries that reaped nothing, and nothing shorter or
// less stuck: a retry that finds a completion in the CQ ring (the loop's own
// pass reaps it: the overloaded case the retry exists for) or one that follows
// a wait that succeeded is not part of the run.

// ceilingErr884 is the error of a stuck ring.
func ceilingErr884() error { return fmt.Errorf("io_uring_enter submit+wait: %w", unix.EBUSY) }

// TestRingWaitContinuesGivesUpOnAStuckRing884: retryable errors, one after the
// other, on a CQ ring with nothing in it end the worker after about a second.
func TestRingWaitContinuesGivesUpOnAStuckRing884(t *testing.T) {
	ring := newTestRing(t)
	logs := &warnLog884{}
	w := &Worker{ring: ring, id: 3, logger: slog.New(logs)}

	start := time.Now()
	retries := 0
	var gaveUpAfter time.Duration
	for time.Since(start) < 5*time.Second {
		if !w.ringWaitContinues(waitModeTimed, ceilingErr884()) {
			gaveUpAfter = time.Since(start)
			break
		}
		retries++
	}
	t.Logf("celeris884: %d consecutive EBUSY waits with nothing to reap; gave up after %v", retries, gaveUpAfter)
	if gaveUpAfter == 0 {
		t.Fatalf("%d consecutive EBUSY waits over %v, with nothing in the CQ ring to reap, and the worker is still retrying: a failing ring is never given up on",
			retries, time.Since(start).Round(time.Millisecond))
	}
	if gaveUpAfter < 500*time.Millisecond {
		t.Errorf("the worker gave up on a ring after %v of retries (%d): too soon for a wait that only meets a full CQ ring", gaveUpAfter, retries)
	}
	if gaveUpAfter > 3*time.Second {
		t.Errorf("the worker gave up on a ring after %v of retries: the ceiling is about a second", gaveUpAfter)
	}
	var errs int
	logs.mu.Lock()
	for _, r := range logs.recs {
		if r.Level >= slog.LevelError {
			errs++
		}
	}
	logs.mu.Unlock()
	if errs == 0 {
		t.Error("the worker stopped on a failing ring and logged no error: its operator would see a stopped worker with no reason")
	}
}

// TestRingWaitContinuesNeverGivesUpWhileItReaps884: the same error with a
// completion waiting in the CQ ring at each retry is the loop being behind its
// ring, not a failing ring: the loop's pass reaps, and the retry goes on for as
// long as that lasts, well past the ceiling.
func TestRingWaitContinuesNeverGivesUpWhileItReaps884(t *testing.T) {
	ring := newTestRing(t)
	w := &Worker{ring: ring, id: 3, logger: slog.New(&warnLog884{})}
	postNOP884(t, ring)
	start := time.Now()
	n := 0
	for time.Since(start) < 2500*time.Millisecond {
		if !w.ringWaitContinues(waitModeSubmitSends, ceilingErr884()) {
			t.Fatalf("the worker gave up after %v and %d retries, each of which found a completion in the CQ ring to reap",
				time.Since(start).Round(time.Millisecond), n)
		}
		n++
		time.Sleep(time.Millisecond)
	}
}

// TestRingWaitContinuesCountsOnlyConsecutiveRetries884: retries that come
// apart, each after waits that succeeded, are not a stuck ring: a worker that
// meets an EBUSY now and then, with nothing to reap each time, runs on. The
// loop pass count (tickCounter) moves once per pass, so a retry that follows
// the last by more than one pass has a wait that succeeded between them: the
// test models it by advancing the count, with the time between the retries a
// stuck run's own (50 us sleeps) so that only the count tells the two apart.
func TestRingWaitContinuesCountsOnlyConsecutiveRetries884(t *testing.T) {
	ring := newTestRing(t)
	w := &Worker{ring: ring, id: 3, logger: slog.New(&warnLog884{})}
	start := time.Now()
	n := 0
	for time.Since(start) < 2500*time.Millisecond {
		if !w.ringWaitContinues(waitModeTimed, ceilingErr884()) {
			t.Fatalf("the worker gave up after %v and %d retries, each apart from the last by a pass whose wait succeeded",
				time.Since(start).Round(time.Millisecond), n)
		}
		n++
		w.tickCounter += 2 // the retried pass, then a pass whose wait succeeded
	}
	t.Logf("celeris884: %d retries, every other pass, over %v: the worker runs on", n, time.Since(start).Round(time.Millisecond))
}

// TestRunStopsOnAStuckRingWait884: the engine's own arm. One worker's waits
// answer EBUSY for good (the seam) on an engine with nothing to do, so there is
// nothing to reap; it stops within a few seconds, as it does for any other ring
// failure (TestRunStopsOnARingWaitFailure884), and the other worker is left to
// serve.
func TestRunStopsOnAStuckRingWait884(t *testing.T) {
	inj := &ebusyInject884{err: ebusyErr884(), n: 1 << 40, firstOnly: true}
	inj.only.Store(-1)
	setRingWaitSeam884(t, inj.seam)
	e, addr := startFDLEngine(t, transplantTestHandler{}, nil)

	time.Sleep(200 * time.Millisecond)
	armedAt := time.Now()
	inj.armed.Store(true)
	var dead *Worker
	for deadline := time.Now().Add(6 * time.Second); time.Now().Before(deadline) && dead == nil; time.Sleep(10 * time.Millisecond) {
		for _, w := range e.workers {
			if w.exited.Load() {
				dead = w
			}
		}
	}
	t.Logf("celeris884 stuck ring: injected=%d%s", inj.total(-1), inj.summary())
	if inj.total(-1) == 0 {
		t.Fatal("the injected EBUSY never reached a wait: the test would prove nothing")
	}
	if dead == nil {
		t.Fatalf("a worker whose every ring wait answered EBUSY for %v, with nothing to reap, was still running: the retry has no ceiling", time.Since(armedAt).Round(time.Millisecond))
	}
	t.Logf("celeris884 stuck ring: worker %d stopped %v after the injection began", dead.id, time.Since(armedAt).Round(time.Millisecond))
	if dead != inj.chosen.Load() {
		t.Errorf("worker %d stopped, but the EBUSY was answered to worker %d", dead.id, inj.chosen.Load().id)
	}
	for _, w := range e.workers {
		if w != dead && w.exited.Load() {
			t.Errorf("worker %d stopped though only worker %d met the failure", w.id, dead.id)
		}
	}
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("dial after the stuck worker stopped: %v", err)
	}
	defer func() { _ = c.Close() }()
	if _, err := io.WriteString(c, "GET / HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := http.ReadResponse(bufio.NewReader(c), nil); err != nil {
		t.Errorf("the surviving worker did not answer: %v", err)
	}
}
