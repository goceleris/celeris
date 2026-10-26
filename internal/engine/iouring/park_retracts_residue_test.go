//go:build linux

package iouring

// celeris#711, the io_uring half. A worker's sweep() runs before its only
// checkTimeouts call (the tick gate) and before the DRAINING→SUSPENDED park,
// and io_uring has no second, pre-sweep checkTimeouts site. So every
// Read/Idle/Write-timeout close of a draining worker's last connection lands
// after sweep() in the iteration that parks it, and the residue the worker
// published last used to stand in the engine-wide gauges until the next
// ResumeAccept: no manipulation needed, a legal config reaches it every time.
// A CQE is dispatched before sweep(), which is why a close its CQE causes
// retracted: the control is the client's own close (the recv CQE that reads
// EOF), the one close of the last connection that cannot reach the park after
// sweep(). The header timer is not the control: its CQE closes before sweep(),
// but checkTimeouts closes on the same deadline (hdrDL) after it, and
// whichever gets there first closes, so a 400 ms header timer left the
// control's side to the scheduler (celeris#885 item 11).
//
// What the post-sweep arm guards today is the retraction and the closeFDOwed
// gate together (celeris#685): a ReadTimeout close of a conn with its recv
// armed leaves the descriptor owed to the cancel's CQE, and a worker with a
// close owed does not park, so the next iteration's sweep() retracts. The arm
// goes red with both removed, not with the retraction alone (checked in
// celeris#885 item 11); the retraction is what a close with nothing in flight
// would need. TestParkedWorkerRetractsResidueOfACloseWithNothingInFlight884 is
// that close (celeris#884 item 80): the conn's recv is ended and reaped inside
// the sweep's own pass, so its close lands after sweep() with nothing owed to
// the kernel, the gate does not hold the worker, and only the retraction at
// the park clears the gauges (red with it removed).

import (
	"context"
	"io"
	"log/slog"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// startParkEngine711 is startFDLEngine with its ring ENOMEM retried
// (startRingRetried662): at CI's 8 MiB memlock the kernel gives a closed
// ring's pages back 12-23 ms after the close, so an engine started right
// after another ring closed can fail on memory nothing holds any more. No
// probe dial: the engine is idle when it returns.
func startParkEngine711(t *testing.T, h stream.Handler, mut func(*resource.Config)) (*Engine, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	e, cancel, done := startRingRetried662(t, func() (*Engine, error) {
		cfg := resource.Config{
			Addr:      addr,
			Protocol:  engine.HTTP1,
			Resources: resource.Resources{Workers: 2},
			Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
		if mut != nil {
			mut(&cfg)
		}
		return New(cfg, h)
	})
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("engine did not stop within 5s")
		}
	})
	t.Logf("celeris657 engine workers=%d", e.NumWorkers())
	return e, addr
}

// residueSum adds the five residual gauges: without IORING_ASYNC_CANCEL flags
// a mid-request connection with its recv armed is counted Pinned, not Busy,
// and this test is about the gauges together, not the class.
func residueSum(m engine.EngineMetrics) uint64 {
	return m.TransplantResidualDetached + m.TransplantResidualH2 + m.TransplantResidualPinned +
		m.TransplantResidualUnstarted + m.TransplantResidualBusy
}

func parkWaitIou(d time.Duration, f func() bool) bool {
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

// parkResidueIou drives a draining worker through the park with its last
// connection, a partial request, closed by the ReadTimeout branch of
// checkTimeouts (postSweep) or by the client, right after PauseAccept
// (!postSweep), and asserts that a worker that has parked holding nothing
// publishes nothing. The header timeout is 60 s in both arms: far from either.
func parkResidueIou(t *testing.T, postSweep bool) { parkResidueIouMode(t, postSweep, false) }

// parkResidueIouMode is parkResidueIou with a third kind of close: nothingOwed
// replaces ReadTimeout's close by one made after sweep() with no op owed to the
// kernel (see afterSweepNothingOwed884).
func parkResidueIouMode(t *testing.T, postSweep, nothingOwed bool) {
	var disc atomic.Int64
	e, addr := startParkEngine711(t, fdlHandler{}, func(c *resource.Config) {
		c.OnDisconnect = func(string) { disc.Add(1) }
		// No TCP_DEFER_ACCEPT, so no pause linger (celeris#662): the
		// listeners close as PauseAccept is called, well inside the
		// connection's deadline, and the close lands on a worker with no
		// listener, the only kind that parks.
		c.DisableDeferAccept = true
		c.ReadHeaderTimeout = 60 * time.Second // neither the header timer nor its checkTimeouts close can come first
		if postSweep && !nothingOwed {
			c.ReadTimeout = 300 * time.Millisecond
			c.IdleTimeout = 10 * time.Minute
		}
	})
	e.mu.Lock()
	ws := append([]*Worker(nil), e.workers...)
	e.mu.Unlock()
	allParked := func() bool {
		for _, w := range ws {
			if !w.suspended.Load() {
				return false
			}
		}
		return true
	}
	if !parkWaitIou(3*time.Second, func() bool {
		m := e.Metrics()
		return m.ActiveConnections == 0 && m.AcceptCount == m.CloseCount
	}) {
		t.Fatalf("celeris711 PREMISE: the engine is not idle")
	}
	d0 := disc.Load()
	m0 := e.Metrics()
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	const partial = "GET /slow HTTP/1.1\r\nHost: x\r\n" // no blank line: mid-headers until a deadline
	if _, err := c.Write([]byte(partial)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !parkWaitIou(2*time.Second, func() bool {
		m := e.Metrics()
		return m.ActiveConnections == 1 && m.BytesRead >= m0.BytesRead+uint64(len(partial))
	}) {
		t.Fatalf("celeris711 PREMISE: the worker never read the partial request")
	}
	tgt := &fdlTarget{}
	e.StartTransplant(tgt)
	defer e.StopTransplant()
	if !parkWaitIou(2*time.Second, func() bool { return residueSum(e.Metrics()) == 1 }) {
		t.Fatalf("celeris711 PREMISE: the sweep never published a residue of 1 (sum=%d adopted=%d)",
			residueSum(e.Metrics()), tgt.adopted.Load())
	}
	var manufactured atomic.Int32
	if nothingOwed {
		hook := afterSweepNothingOwed884(&manufactured)
		testHookCheckTimeouts.Store(&hook)
		t.Cleanup(func() { testHookCheckTimeouts.Store(nil) })
	}
	if err := e.PauseAccept(); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if n := e.Metrics().ActiveConnections; n != 1 {
		t.Fatalf("celeris711 PREMISE: the connection closed (active=%d) before PauseAccept returned, so not "+
			"on a worker without a listener", n)
	}
	if !postSweep && !nothingOwed {
		// The client leaves, far before any deadline: the worker reads the
		// EOF from a CQE, and CQEs are dispatched before sweep().
		_ = c.Close()
	}
	if !parkWaitIou(10*time.Second, func() bool { return e.Metrics().ActiveConnections == 0 }) {
		t.Fatalf("celeris711 PREMISE: the connection was never closed")
	}
	if nothingOwed && manufactured.Load() != 1 {
		t.Fatalf("celeris884 PREMISE: the sweep hook ended the conn's recv %d times, want 1", manufactured.Load())
	}
	if !parkWaitIou(3*time.Second, allParked) {
		t.Fatalf("celeris711 PREMISE: not every worker parked")
	}
	// A worker sets suspended after everything it does before the park, so
	// what it published is visible now. Hold briefly to show it stands.
	resParked := residueSum(e.Metrics())
	time.Sleep(200 * time.Millisecond)
	m := e.Metrics()
	t.Logf("celeris711 iouring post_sweep=%v workers=%d res_parked=%d res_hold=%d active=%d all_parked=%v closes=%d disconnects=%d adopted=%d",
		postSweep, len(ws), resParked, residueSum(m), m.ActiveConnections, allParked(),
		m.CloseCount-m0.CloseCount, disc.Load()-d0, tgt.adopted.Load())
	if m.ActiveConnections != 0 || !allParked() || m.CloseCount-m0.CloseCount != 1 || disc.Load()-d0 != 1 {
		t.Fatalf("celeris711 PREMISE: active=%d all_parked=%v closes=%d disconnects=%d",
			m.ActiveConnections, allParked(), m.CloseCount-m0.CloseCount, disc.Load()-d0)
	}
	if resParked != 0 || residueSum(m) != 0 {
		t.Errorf("celeris711 STALE: the residual gauges sum to %d (at the park %d) with 0 connections and every "+
			"worker parked. A parked worker holds nothing, and sweep() does not run again until it wakes",
			residueSum(m), resParked)
	}
}

// TestParkedWorkerRetractsResidueOfAPostSweepClose is the defect: ReadTimeout
// closes the worker's last connection in checkTimeouts, after sweep(), in the
// iteration that parks it.
func TestParkedWorkerRetractsResidueOfAPostSweepClose(t *testing.T) { parkResidueIou(t, true) }

// TestParkedWorkerRetractsResidueOfAPreSweepClose is its control: the same
// connection closed by the client's EOF, a CQE dispatched before sweep(),
// which retracted before this fix as well.
func TestParkedWorkerRetractsResidueOfAPreSweepClose(t *testing.T) { parkResidueIou(t, false) }

// afterSweepNothingOwed884 is a checkTimeouts hook (it runs on the worker's
// thread, after sweep() in the same pass) that gives a worker with one live
// conn, no listener and a recv armed on it the close celeris#884 item 80 asks
// for: it cancels the recv, submits, and reaps the cancelled recv's CQE the way
// the loop does, which closes the conn (a failed recv closes it) with nothing
// left in flight. The close is therefore after sweep() and owes the kernel
// nothing, so closeFDOwed stays 0 and the worker parks in the same pass; once.
func afterSweepNothingOwed884(done *atomic.Int32) func(*Worker) {
	return func(w *Worker) {
		if done.Load() != 0 || w.listenFD >= 0 || !w.acceptPaused.Load() || len(w.liveConns) != 1 {
			return
		}
		cs := w.conns[w.liveConns[0]]
		if cs == nil || !cs.recvArmed || cs.closing {
			return
		}
		fd := cs.fd
		if w.cancelConnOps(fd, cs) {
			return // the cancel was not placed; the next sweep tries again
		}
		done.Store(1)
		for end := time.Now().Add(2 * time.Second); w.conns[fd] == cs && time.Now().Before(end); {
			if err := w.ring.SubmitAndWaitTimeout(10 * time.Millisecond); err != nil && !ringWaitRetryable(err) {
				return
			}
			head, tail := w.ring.BeginCQ()
			for ; head != tail; head++ {
				w.processCQE(context.Background(), w.ring.cqeAt(head), time.Now().UnixNano())
			}
			w.ring.EndCQ(head)
		}
	}
}

// TestParkedWorkerRetractsResidueOfACloseWithNothingInFlight884 is the arm item
// 80 asks for: the last conn closed after sweep() with nothing owed to the
// kernel, which the closeFDOwed gate does not hold off the park. It goes red
// with the park's sweepRetract removed.
func TestParkedWorkerRetractsResidueOfACloseWithNothingInFlight884(t *testing.T) {
	parkResidueIouMode(t, true, true)
}
