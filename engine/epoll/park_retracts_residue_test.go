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
// connection at the same deadline and differs only in which side of sweep()
// the close lands on. The timerfd is disarmed so the header-deadline close
// can only come from the tick gate: post-sweep by construction, no search.

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
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

// parkResidue drives one standby loop through the park with its last
// connection, a slowloris one counted Busy, closed on the side of sweep() that
// postSweep names, and asserts that a loop that has parked holding nothing
// publishes nothing.
func parkResidue(t *testing.T, postSweep bool) {
	const rht = 400 * time.Millisecond
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
	}, parkResidueHandler{})
	if err != nil {
		t.Skipf("epoll engine unavailable: %v", err)
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
	if !parkWait(8*time.Second, func() bool { return e.Addr() != nil }) {
		t.Skip("epoll engine did not bind")
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

	m0 := e.Metrics()
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	const partial = "GET /slow HTTP/1.1\r\nHost: x\r\n" // no blank line: mid-headers until the deadline
	if _, err := c.Write([]byte(partial)); err != nil {
		t.Fatalf("write: %v", err)
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
	if !postSweep {
		// The client leaves before its deadline: the loop learns it from
		// an event, and the event path runs before sweep().
		time.Sleep(50 * time.Millisecond)
		_ = c.Close()
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
	t.Logf("celeris711 post_sweep=%v busy_parked=%d busy_hold=%d active=%d all_parked=%v closes=%d disconnects=%d adopted=%d",
		postSweep, busyParked, m.TransplantResidualBusy, m.ActiveConnections, allParked(),
		m.CloseCount-m0.CloseCount, disc.Load(), tgt.adopted.Load())
	if m.ActiveConnections != 0 || !allParked() || m.CloseCount-m0.CloseCount != 1 || disc.Load() != 1 {
		t.Fatalf("celeris711 PREMISE: active=%d all_parked=%v closes=%d disconnects=%d",
			m.ActiveConnections, allParked(), m.CloseCount-m0.CloseCount, disc.Load())
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
func TestParkedLoopRetractsResidueOfAPostSweepClose(t *testing.T) { parkResidue(t, true) }

// TestParkedLoopRetractsResidueOfAPreSweepClose is its control: the same
// connection closed on the event path, before sweep(), which retracted before
// this fix as well.
func TestParkedLoopRetractsResidueOfAPreSweepClose(t *testing.T) { parkResidue(t, false) }
