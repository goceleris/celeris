//go:build linux

package iouring

import (
	"context"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/probe"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
)

// The io_uring half of the celeris#844 rig (async_abort_844_linux_test.go is
// the same file on both engines).

const (
	engineName844 = "io_uring"
	pkgDir844     = "iouring"
)

// tier844 names the recv tier the workers run: "high" with a provided buffer
// ring, "base" without (CELERIS_MAX_IOURING_TIER=base, or a kernel without
// it).
func tier844(e *Engine) string {
	e.mu.Lock()
	ws := e.workers
	e.mu.Unlock()
	for _, w := range ws {
		if w.bufRing != nil {
			return "high"
		}
	}
	return "base"
}

// parkRouteReachable844 reports whether the park loop of serveAsync reaches
// application code (the route resolver) under asyncInMu on this engine and
// tier: canRevertToInline short-circuits on w.bufRing == nil, so only the
// base tier calls RouteAsync there.
func parkRouteReachable844(e *Engine) (bool, string) {
	if tier := tier844(e); tier != "base" {
		return false, "tier " + tier + ": canRevertToInline never calls RouteAsync with a buffer ring, so no application code runs under asyncInMu"
	}
	return true, ""
}

// retryStart844 runs an engine start, retrying one that failed only on ring
// ENOMEM: at the runner's 8 MiB RLIMIT_MEMLOCK the kernel gives a closed
// engine's rings back some milliseconds after the close, and a start made in
// that window fails although nothing is wrong (ring_budget_linux_test.go).
func retryStart844(start func() error) error {
	_, _, err := retryRingENOMEM(start)
	return err
}

// unavailable844 is what a test does with a start that still failed: skip only
// if io_uring cannot be used here at all (or memlock is below the floor these
// tests hold themselves to), and fail otherwise, because a skip is not
// coverage and the CI step for this package forbids any.
func unavailable844(t *testing.T, err error) {
	t.Helper()
	skipUnlessIOUringUnusable(t, err)
}

// The route-resolver window: canRevertToInline calls the application's
// RouteAsync under asyncInMu in the park loop, on a worker without a
// provided-buffer ring (the default; CELERIS_IOURING_MULTISHOT_RECV=1 gives
// the worker one, and the arm is then absent: it skips, and CI does not set
// that variable for this package).

func TestAbortParkRoutePanicHoldingAsyncInMu844(t *testing.T) {
	runArm844(t, arm844ParkRoute, true, "closed")
}

func TestAbortParkRouteControl844(t *testing.T) {
	runArm844(t, arm844ParkRoute, false, "alive")
}

// Item 4, the peer-FIN interleaving, deterministically: the handler is running
// when the peer's FIN reaches the worker, so closeConn finds detachMu held with
// the goroutine busy and leaves the close to it (closeOwed). Then the handler
// panics or calls runtime.Goexit; the abort must release the lock and hand the
// conn back, and the worker's drain must close it, once. The wait for "the
// FIN was handled" is the condition cs.closeOwed under asyncInMu, not a sleep
// (the review's scratch arm slept 300 ms).

type holdHandler844 struct {
	how     string
	entered chan struct{}
	release chan struct{}
}

func (h *holdHandler844) HandleStream(_ context.Context, _ *stream.Stream) error {
	h.entered <- struct{}{}
	<-h.release
	if h.how == "goexit" {
		runtime.Goexit()
	}
	panic("celeris844: handler panic after the peer closed")
}
func (*holdHandler844) RouteAsync(_, _ string) bool { return true }
func (*holdHandler844) HasAsyncRoutes() bool        { return true }

func TestAbortAfterPeerCloseHandsTheConnBack844(t *testing.T) {
	for _, how := range []string{"panic", "goexit"} {
		t.Run(how, func(t *testing.T) {
			f := newFDLFixture(t, true)
			h := &holdHandler844{how: how, entered: make(chan struct{}, 1), release: make(chan struct{})}
			var once sync.Once
			rel := func() { once.Do(func() { close(h.release) }) }
			t.Cleanup(rel)
			f.w.handler = h
			f.cs.asyncPromoted.Store(true)
			f.armFirstRecv()
			f.deliver("GET /hold HTTP/1.1\r\nHost: x\r\n\r\n")
			select {
			case <-h.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("PREMISE: the handler never started")
			}
			f.process(f.recvCQE(0)) // the peer's FIN
			f.cs.asyncInMu.Lock()
			owed := f.cs.closeOwed
			f.cs.asyncInMu.Unlock()
			if !owed || f.w.conns[f.fd] != f.cs {
				t.Fatalf("PREMISE: after the FIN closeOwed=%v slot-kept=%v, want the close left to the running handler",
					owed, f.w.conns[f.fd] == f.cs)
			}
			rel()
			exited := make(chan struct{})
			go func() { f.w.asyncWG.Wait(); close(exited) }()
			select {
			case <-exited:
			case <-time.After(5 * time.Second):
				t.Fatalf("celeris#844: the dispatch goroutine did not exit after its handler ended (%s)", how)
			}
			if !f.cs.detachMu.TryLock() {
				t.Fatalf("celeris#844: detachMu is still held after the dispatch goroutine exited (%s)", how)
			}
			f.cs.detachMu.Unlock()
			f.w.drainDetachQueue()
			if f.w.conns[f.fd] != nil || f.w.closeCount.Load() != 1 {
				t.Errorf("celeris#844: the owed close did not run exactly once after the abort (%s): slot=%p closeCount=%d",
					how, f.w.conns[f.fd], f.w.closeCount.Load())
			}
			t.Logf("celeris844 RESULT engine=io_uring arm=peer-fin-then-%s closeOwed_before_release=%v closeCount=%d slot_cleared=%v",
				how, owed, f.w.closeCount.Load(), f.w.conns[f.fd] == nil)
		})
	}
}

// TestReportTier844 prints what the probe makes of this host, so that a run's
// log says which tier the io_uring arms ran on. It asserts nothing.
func TestReportTier844(t *testing.T) {
	p := probe.Probe()
	t.Logf("celeris844 PROBE kernel=%s io_uring_tier=%v CELERIS_MAX_IOURING_TIER=%q", p.KernelVersion, p.IOUringTier, os.Getenv("CELERIS_MAX_IOURING_TIER"))
}
