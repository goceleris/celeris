//go:build linux

package epoll

// celeris#711. The residual gauges are a statement about what the engine
// HOLDS, and while a loop runs, sweep()'s empty-set retraction is what keeps
// them honest. A standby loop can lose its last connection AFTER sweep() in
// the iteration that then takes the DRAINING→SUSPENDED park. The park is
// indefinite, sweep() does not run again until something wakes the loop, and
// the residue the loop published last used to stand in the engine-wide gauge
// with nothing behind it: TransplantResidualBusy = 1, ActiveConnections = 0,
// every loop parked, sweep passes frozen. Nightly 36247560882 carried it for
// the last 70 s of an adaptive cell.
//
// checkTimeouts has two call sites on this engine: the timerfd event, BEFORE
// sweep(), and the tick gate, AFTER it. The pair below closes the same
// connection and differs only in which side of sweep() the close lands on.
// The timerfd is disarmed so the header-deadline close (400 ms) can only come
// from the tick gate: post-sweep by construction, no search. The control's
// close is the client's, right after PauseAccept, with a 10 s header
// deadline that no step of the test can reach (celeris#885 item 11).
//
// The third arm reaches the post-sweep close through a production path and
// disarms nothing (celeris#885 item 10): an async route is blocked in its
// handler, so the conn is counted Busy, the request carries Connection: close,
// PauseAccept parks the listeners, and the handler is released. The dispatch
// goroutine sets asyncClosed and enqueues the conn, and drainDetachQueue, which
// runs after sweep(), closes it: the park follows in the same iteration.

import (
	"context"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// parkResidueHandler marks every route async, as the refapps run. On the sync
// path a mid-headers connection is MOVED by the sweep (AtRequestBoundary holds
// mid-headers), so it is not residue; on the async path
// "l.async && HasPendingData()" refuses it and it is counted Busy.
type parkResidueHandler struct{}

func (parkResidueHandler) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "text/plain"}, {"content-length", "2"}}, []byte("ok"))
}
func (parkResidueHandler) RouteAsync(_, _ string) bool { return true }
func (parkResidueHandler) HasAsyncRoutes() bool        { return true }

var _ stream.AsyncRouteResolver = parkResidueHandler{}

// parkResidueBlockHandler is parkResidueHandler whose handler waits for
// release before it answers, so the conn it serves is inside a handler (counted
// Busy by the sweep, not movable) for as long as the test wants.
type parkResidueBlockHandler struct {
	parkResidueHandler
	entered chan struct{}
	release chan struct{}
}

func (h *parkResidueBlockHandler) HandleStream(ctx context.Context, s *stream.Stream) error {
	select {
	case h.entered <- struct{}{}:
	default:
	}
	<-h.release
	return h.parkResidueHandler.HandleStream(ctx, s)
}

// closingTarget closes whatever it is handed. Nothing should be handed to it
// here: the only connection is refused as Busy.
type closingTarget struct{ adopted atomic.Int64 }

func (c *closingTarget) AdoptConn(fd int, _ engine.Carryover) error {
	c.adopted.Add(1)
	_ = unix.Close(fd)
	return nil
}

func parkWait(d time.Duration, f func() bool) bool {
	for dl := time.Now().Add(d); ; {
		if f() {
			return true
		}
		if time.Now().After(dl) {
			return false
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// parkArm is how parkResidue ends the loop's last connection.
type parkArm int

const (
	// armTickGate: the 400 ms header deadline closes a slowloris conn at the
	// tick gate's checkTimeouts, after sweep(); the timerfd is disarmed, so
	// no other call site can.
	armTickGate parkArm = iota
	// armEvent: the client of a slowloris conn closes it, right after
	// PauseAccept, and the loop learns it from an event, before sweep().
	// This is the control arm: it has no premise check that the loop had
	// already closed its listener when the FIN arrived, and it stays green
	// either way, because the event path retracts in the same iteration.
	armEvent
	// armDetachQueue: the dispatch goroutine of a conn whose request said
	// Connection: close hands it back through the detach queue, and
	// drainDetachQueue closes it, after sweep(). Nothing is disarmed.
	armDetachQueue
)

func (a parkArm) String() string {
	return [...]string{"tick_gate", "event", "detach_queue"}[a]
}

// postSweep reports whether the arm's close lands after sweep().
func (a parkArm) postSweep() bool { return a != armEvent }

// parkResidue drives one standby loop through the park with its last
// connection, counted Busy, closed the way arm names, and asserts that a loop
// that has parked holding nothing publishes nothing.
func parkResidue(t *testing.T, arm parkArm) {
	postSweep := arm.postSweep()
	// The header deadline is what closes the connection in the post-sweep arm,
	// and only there: 400 ms, a deadline the test does not outwait. The
	// pre-sweep arm closes it from the client's side, right after PauseAccept,
	// and its deadline is 10 s, far enough that no slow step of the test can
	// let the deadline close it first, which would land on the tick gate,
	// after sweep(), the path this arm is the control for (celeris#885 item 11).
	rht := 10 * time.Second
	if arm == armTickGate {
		rht = 400 * time.Millisecond
	}
	blocker := &parkResidueBlockHandler{entered: make(chan struct{}, 1), release: make(chan struct{})}
	var handler stream.Handler = parkResidueHandler{}
	if arm == armDetachQueue {
		handler = blocker
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	var disc atomic.Int64
	e, err := New(resource.Config{
		Addr:              addr,
		Protocol:          engine.HTTP1,
		Resources:         resource.Resources{Workers: 2},
		AsyncHandlers:     true,
		ReadHeaderTimeout: rht,
		// No TCP_DEFER_ACCEPT, so no pause linger (celeris#662): the
		// listeners close as PauseAccept is called, well inside the
		// connection's header deadline, and the close lands on a loop with
		// no listener, the only kind that parks.
		DisableDeferAccept: true,
		OnDisconnect:       func(string) { disc.Add(1) },
	}, handler)
	if err != nil {
		t.Fatalf("epoll engine unavailable: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Listen(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("engine did not stop within 5s")
		}
	})
	if err := awaitBound(e, done, 8*time.Second); err != nil {
		t.Fatalf("epoll engine did not bind: %v", err)
	}
	// Every loop's timerfd was created before its ready signal, and Addr is
	// stored after every ready: reading timerFD here is ordered after it.
	e.mu.Lock()
	loops := append([]*Loop(nil), e.loops...)
	e.mu.Unlock()
	for _, l := range loops {
		if l.timerFD < 0 {
			t.Fatalf("celeris711 PREMISE: loop %d has no timerfd with ReadHeaderTimeout %v", l.id, rht)
		}
		if arm == armDetachQueue {
			continue // production's own timers: nothing here is disarmed
		}
		// Disarm it: the tick gate, after sweep(), becomes the only
		// checkTimeouts left on this loop.
		if err := unix.TimerfdSettime(l.timerFD, 0, &unix.ItimerSpec{}, nil); err != nil {
			t.Fatalf("celeris711 PREMISE: disarm timerfd: %v", err)
		}
	}
	allParked := func() bool {
		for _, l := range loops {
			if !l.suspended.Load() {
				return false
			}
		}
		return true
	}

	var closeSeen func() (immediate, deferred int64)
	m0 := e.Metrics()
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	partial := "GET /slow HTTP/1.1\r\nHost: x\r\n" // no blank line: mid-headers until the deadline
	if arm == armDetachQueue {
		// A whole request, answered once the handler is released, and the
		// conn closed after it.
		partial = "GET /block HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n"
	}
	if _, err := c.Write([]byte(partial)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if arm == armDetachQueue {
		select {
		case <-blocker.entered:
		case <-time.After(2 * time.Second):
			t.Fatalf("celeris711 PREMISE: the handler never started")
		}
		// The close, once the handler is released, must be one drainDetachQueue
		// makes at once (closeWhenFlushed with nothing queued): a response still
		// queued would defer it to EPOLLOUT, an event, before sweep().
		var immediate atomic.Int64
		var deferred atomic.Int64
		hook := func(cs *connState) {
			if csWritePending(cs) {
				deferred.Add(1)
			} else {
				immediate.Add(1)
			}
		}
		testHookCloseWhenFlushed.Store(&hook)
		t.Cleanup(func() { testHookCloseWhenFlushed.Store(nil) }) // runs after the engine is stopped
		closeSeen = func() (int64, int64) { return immediate.Load(), deferred.Load() }
	}
	if !parkWait(2*time.Second, func() bool {
		m := e.Metrics()
		return m.ActiveConnections == 1 && m.BytesRead >= m0.BytesRead+uint64(len(partial))
	}) {
		t.Fatalf("celeris711 PREMISE: the loop never read the partial request")
	}

	tgt := &closingTarget{}
	e.StartTransplant(tgt)
	defer e.StopTransplant()
	if !parkWait(2*time.Second, func() bool { return e.Metrics().TransplantResidualBusy == 1 }) {
		m := e.Metrics()
		t.Fatalf("celeris711 PREMISE: the sweep never published Busy=1 (busy=%d adopted=%d active=%d)",
			m.TransplantResidualBusy, tgt.adopted.Load(), m.ActiveConnections)
	}
	if err := e.PauseAccept(); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if n := e.Metrics().ActiveConnections; n != 1 {
		t.Fatalf("celeris711 PREMISE: the connection closed (active=%d) before PauseAccept returned, so not "+
			"on a loop without a listener", n)
	}
	switch arm {
	case armEvent:
		// The client leaves, well before its deadline: the loop learns it
		// from an event, and the event path runs before sweep().
		_ = c.Close()
	case armDetachQueue:
		close(blocker.release)
	}
	if !parkWait(rht+6*time.Second, func() bool { return e.Metrics().ActiveConnections == 0 }) {
		t.Fatalf("celeris711 PREMISE: the connection was never closed (active=%d)", e.Metrics().ActiveConnections)
	}
	if !parkWait(3*time.Second, allParked) {
		t.Fatalf("celeris711 PREMISE: not every loop parked")
	}
	// A loop sets suspended after everything it does before the park, so
	// what it published is visible now. Hold briefly to show it stands.
	busyParked := e.Metrics().TransplantResidualBusy
	time.Sleep(200 * time.Millisecond)
	m := e.Metrics()
	if arm == armDetachQueue {
		// The close went through the detach queue, and the response went out
		// first: a client that got no 200 saw a conn closed with it still queued.
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 64)
		n, _ := io.ReadFull(c, buf[:len("HTTP/1.1 200")])
		if got := string(buf[:n]); got != "HTTP/1.1 200" {
			t.Errorf("celeris711 PREMISE: the client read %q, want the 200 response the close followed", got)
		}
		imm, def := closeSeen()
		t.Logf("celeris711 detach_queue closeWhenFlushed immediate=%d deferred=%d", imm, def)
		if imm != 1 || def != 0 {
			t.Fatalf("celeris711 PREMISE: closeWhenFlushed ran with nothing queued %d times and with a response "+
				"still queued %d times, want 1 and 0: the close did not land on drainDetachQueue's immediate branch", imm, def)
		}
	}
	t.Logf("celeris711 arm=%v post_sweep=%v busy_parked=%d busy_hold=%d active=%d all_parked=%v closes=%d disconnects=%d adopted=%d",
		arm, postSweep, busyParked, m.TransplantResidualBusy, m.ActiveConnections, allParked(),
		m.CloseCount-m0.CloseCount, disc.Load(), tgt.adopted.Load())
	if m.ActiveConnections != 0 || !allParked() || m.CloseCount-m0.CloseCount != 1 || disc.Load() != 1 || tgt.adopted.Load() != 0 {
		t.Fatalf("celeris711 PREMISE: active=%d all_parked=%v closes=%d disconnects=%d adopted=%d",
			m.ActiveConnections, allParked(), m.CloseCount-m0.CloseCount, disc.Load(), tgt.adopted.Load())
	}
	if busyParked != 0 || m.TransplantResidualBusy != 0 {
		t.Errorf("celeris711 STALE: TransplantResidualBusy = %d (at the park %d) with 0 connections and every "+
			"loop parked. A parked loop holds nothing, and sweep() does not run again until it wakes",
			m.TransplantResidualBusy, busyParked)
	}
}

// TestParkedLoopRetractsResidueOfAPostSweepClose is the defect: the header
// deadline closes the loop's last connection at the tick gate, after sweep(),
// in the iteration that parks it.
func TestParkedLoopRetractsResidueOfAPostSweepClose(t *testing.T) { parkResidue(t, armTickGate) }

// TestParkedLoopRetractsResidueOfAPreSweepClose is its control: the same
// connection closed on the event path, before sweep(), which retracted before
// this fix as well.
func TestParkedLoopRetractsResidueOfAPreSweepClose(t *testing.T) { parkResidue(t, armEvent) }

// TestParkedLoopRetractsResidueOfADetachQueueClose is the defect again, reached
// without touching a timer: a dispatch goroutine's Connection: close hand-back
// is closed by drainDetachQueue, after sweep(), in the iteration that parks the
// loop. It fails with the park's retraction removed and passes on main
// (celeris#885 item 10).
func TestParkedLoopRetractsResidueOfADetachQueueClose(t *testing.T) { parkResidue(t, armDetachQueue) }
