//go:build linux

package iouring

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#884 (umbrella item 30): the worker reads cs.h1State, on an async-mode
// HTTP/1 conn, without taking detachMu at a close site (finishCloseDetached's
// close shape, cs.h1State != nil && !Detached). The dispatch goroutine nils
// cs.h1State, under detachMu, when it turns an h2c upgrade into HTTP/2
// (switchToH2Local). By the code the close reaches that read only with the
// lock held (closeConn leaves the close to the goroutine while it holds the
// lock). This test holds the goroutine in the upgrade, just before its write
// of cs.h1State (h2cUpgradeHook), resets the connection so the worker's recv
// error runs its close paths meanwhile, and lets the race detector decide: a
// worker read of cs.h1State without detachMu in that window is unordered with
// the write whatever the scheduler does (the construction of celeris#884 item
// 83's TestAsyncH2CUpgradeOnFeedLeavesH1StateToTheGoroutine).
//
// Not covered here: the SEND_ZC submit accounting (prepSendSQE reads
// cs.h1State.Detached). It is reached only from flushSend, which the worker
// runs for an async conn under detachMu or leaves to the goroutine
// (dispatchBusy), so a goroutine held in the upgrade never meets it; a probe
// for it was tried and dropped (it never armed a SEND_ZC).

type h1StateProbeHandler884 struct{}

func (h1StateProbeHandler884) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "text/plain"}, {"content-length", "2"}}, []byte("ok"))
}
func (h1StateProbeHandler884) RouteAsync(_, _ string) bool { return true }
func (h1StateProbeHandler884) HasAsyncRoutes() bool        { return true }

func startH1StateProbeEngine884(t *testing.T) (*Engine, string) {
	t.Helper()
	addr := freeAddr639(t)
	e, cancel, done := startRingRetried662(t, func() (*Engine, error) {
		return New(resource.Config{
			Addr: addr, Protocol: engine.HTTP1, AsyncHandlers: true, EnableH2Upgrade: true,
			Resources: resource.Resources{Workers: 2},
		}, h1StateProbeHandler884{})
	})
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("engine did not stop within 5s")
		}
	})
	return e, addr
}

// holdUpgrade884 arms h2cUpgradeHook so the first upgrade stops in
// switchToH2Local until release is closed (or 10 s pass, counted in timedOut),
// and closes held when it has stopped. It returns the two channels and the
// timeout counter.
func holdUpgrade884(t *testing.T) (held chan struct{}, release chan struct{}, timedOut *atomic.Int64) {
	t.Helper()
	held, release, timedOut = make(chan struct{}), make(chan struct{}), new(atomic.Int64)
	var once atomic.Bool
	hook := func(*connState) {
		if !once.CompareAndSwap(false, true) {
			return
		}
		close(held)
		select {
		case <-release:
		case <-time.After(10 * time.Second):
			timedOut.Add(1)
		}
	}
	h2cUpgradeHook.Store(&hook)
	t.Cleanup(func() { h2cUpgradeHook.Store(nil) })
	return held, release, timedOut
}

// TestPeerResetDuringAnH2CUpgradeKeepsTheCloseShapeReadUnderTheLock884: the
// goroutine is held inside the upgrade of the conn's first request (it holds
// detachMu there), the client resets the connection, and the worker's recv
// error reaches closeOnRecvEnd/closeConn while cs.h1State is still set. The
// worker must leave the close to the goroutine (closeOwed): reading cs.h1State at
// the close shape before the goroutine's write, without the lock, is a race
// the detector reports once the goroutine is released. The conn must then be
// closed once the goroutine lets go.
func TestPeerResetDuringAnH2CUpgradeKeepsTheCloseShapeReadUnderTheLock884(t *testing.T) {
	held, release, timedOut := holdUpgrade884(t)
	e, addr := startH1StateProbeEngine884(t)

	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	if _, err := c.Write([]byte(h2cUpgradeHead722("GET", 0))); err != nil {
		t.Fatalf("write upgrade: %v", err)
	}
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("the upgrade never reached switchToH2Local: the conn was not served by a dispatch goroutine")
	}
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetLinger(0) // Close sends an RST
	}
	_ = c.Close()
	// The worker's recv completes with ECONNRESET within microseconds; the
	// wait gives it, and any close path it takes, the time to read cs.h1State
	// before the goroutine's write. The conn is still counted open: the close
	// is the goroutine's to finish.
	time.Sleep(300 * time.Millisecond)
	if n := e.Metrics().ActiveConnections; n != 1 {
		t.Logf("ActiveConnections = %d with the goroutine still in the upgrade (1 expected: the close waits for its lock)", n)
	}
	close(release)
	if !parkWait712(10*time.Second, func() bool { return e.Metrics().ActiveConnections == 0 }) {
		t.Fatalf("the reset conn was not closed after the upgrade finished: ActiveConnections = %d", e.Metrics().ActiveConnections)
	}
	if timedOut.Load() != 0 {
		t.Error("the upgrade hook timed out waiting for the release")
	}
}
