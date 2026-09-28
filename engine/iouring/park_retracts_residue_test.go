//go:build linux

package iouring

// celeris#711, the io_uring half. A worker's sweep() runs before its only
// checkTimeouts call (the tick gate) and before the DRAINING→SUSPENDED park,
// and io_uring has no second, pre-sweep checkTimeouts site. So every
// Read/Idle/Write-timeout close of a draining worker's last connection lands
// after sweep() in the iteration that parks it, and the residue the worker
// published last used to stand in the engine-wide gauges until the next
// ResumeAccept: no manipulation needed, a legal config reaches it every time.
// The header-timer CQE is dispatched before sweep(), which is why the default
// slowloris defence retracted; that close is the control.

import (
	"io"
	"log/slog"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
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
// checkTimeouts (postSweep) or by its header timer's CQE (!postSweep), and
// asserts that a worker that has parked holding nothing publishes nothing.
func parkResidueIou(t *testing.T, postSweep bool) {
	var disc atomic.Int64
	e, addr := startParkEngine711(t, fdlHandler{}, func(c *resource.Config) {
		c.OnDisconnect = func(string) { disc.Add(1) }
		// No TCP_DEFER_ACCEPT, so no pause linger (celeris#662): the
		// listeners close as PauseAccept is called, well inside the
		// connection's deadline, and the close lands on a worker with no
		// listener, the only kind that parks.
		c.DisableDeferAccept = true
		if postSweep {
			c.ReadHeaderTimeout = 60 * time.Second // the header timer stays far away
			c.ReadTimeout = 300 * time.Millisecond
			c.IdleTimeout = 10 * time.Minute
		} else {
			c.ReadHeaderTimeout = 400 * time.Millisecond
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
	if err := e.PauseAccept(); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if n := e.Metrics().ActiveConnections; n != 1 {
		t.Fatalf("celeris711 PREMISE: the connection closed (active=%d) before PauseAccept returned, so not "+
			"on a worker without a listener", n)
	}
	if !parkWaitIou(10*time.Second, func() bool { return e.Metrics().ActiveConnections == 0 }) {
		t.Fatalf("celeris711 PREMISE: the connection was never closed")
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
// connection closed by its header timer's CQE, before sweep(), which
// retracted before this fix as well.
func TestParkedWorkerRetractsResidueOfAPreSweepClose(t *testing.T) { parkResidueIou(t, false) }
