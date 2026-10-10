//go:build linux

package epoll

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/resource"
	"github.com/goceleris/celeris/internal/wakefd"
)

// celeris#863. Loop.shutdown walked liveConns and nothing else, so a deferred
// transplant (tryTransplant detached the fd from epoll and the conn table,
// asked the dispatch goroutine to quiesce, and left the hand-off to
// drainDetachQueue) whose goroutine exit entry was still queued when the
// context was cancelled kept its descriptor open for the life of the process:
// out of liveConns, so phase 3 never closed it, and handed to no engine. The
// same happens on one loop's self-shutdown after a listener re-create failure.
//
// The tests drive the real tryTransplant and the real shutdown on a Loop
// literal (the wakefd_after_shutdown_test shape) with a REAL dispatch
// goroutine, so the exit entry reaches the queue exactly as it does in the
// engine: from the goroutine, after tryTransplant asked it to quiesce, and
// during shutdown's asyncWG.Wait.

// shutdownLoop863 is a bare Loop with everything shutdown touches and its own
// epoll set (shutdown closes it).
func shutdownLoop863(t *testing.T, onDisconnect func(string)) *Loop {
	t.Helper()
	efd, err := unix.Eventfd(0, unix.EFD_NONBLOCK|unix.EFD_CLOEXEC)
	if err != nil {
		t.Skipf("eventfd unavailable: %v", err)
	}
	epfd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		_ = unix.Close(efd)
		t.Skipf("epoll_create1 unavailable: %v", err)
	}
	l := &Loop{
		epollFD:                epfd,
		listenFD:               -1,
		timerFD:                -1,
		wakeFD:                 wakefd.New(efd),
		conns:                  make([]*connState, connTableSize),
		liveConns:              make([]*connState, 0, 4),
		activeConns:            &atomic.Int64{},
		closeCount:             &atomic.Uint64{},
		acceptCount:            &atomic.Uint64{},
		bytesRead:              &atomic.Uint64{},
		bytesWritten:           &atomic.Uint64{},
		reqCount:               &atomic.Uint64{},
		transplantAdopted:      &atomic.Uint64{},
		transplantDetached:     &atomic.Uint64{},
		transplantSlotOccupied: &atomic.Uint64{},
		transplantStranded:     &atomic.Uint64{},
		handler:                okHandler658{},
		async:                  true,
		resolved:               resource.ResolvedResources{BufferSize: 4096},
		cfg:                    resource.Config{OnDisconnect: onDisconnect},
	}
	return l
}

// parkedAsyncConn863 starts a real dispatch goroutine on a socketpair conn,
// lets it answer one request, and returns once it is parked with the response
// flushed: the state tryTransplant accepts for a deferred hand-off.
func parkedAsyncConn863(t *testing.T, l *Loop) (int, *connState) {
	t.Helper()
	fd := socketpairFD(t, l)
	cs := acquireConnState(context.Background(), fd, 4096, true)
	cs.protocol = engine.HTTP1
	cs.detected = true
	cs.remoteAddr = "127.0.0.1:9"
	cs.writeFn = l.makeWriteFn(cs)
	l.conns[fd] = cs
	l.addLiveConn(cs)
	l.connCount++
	l.activeConns.Add(1)
	l.initProtocol(cs)

	cs.asyncInMu.Lock()
	cs.asyncInBuf = append(cs.asyncInBuf, "GET /x HTTP/1.1\r\nHost: x\r\n\r\n"...)
	cs.asyncRun = true
	cs.asyncInMu.Unlock()
	l.asyncWG.Add(1)
	go l.runAsyncHandler(cs)

	deadline := time.Now().Add(5 * time.Second)
	for {
		cs.asyncInMu.Lock()
		parked := cs.asyncParked && cs.asyncRun && len(cs.asyncInBuf) == 0
		cs.asyncInMu.Unlock()
		if parked {
			return fd, cs
		}
		if time.Now().After(deadline) {
			t.Fatal("celeris863 PREMISE: the dispatch goroutine never parked after its first request")
		}
		time.Sleep(time.Millisecond)
	}
}

// deferTransplant863 runs the real tryTransplant on cs and returns the target
// it hands off to. It fails the test unless the transplant is deferred
// (transplantPending, the hand-off owed), which is the state under test.
func deferTransplant863(t *testing.T, l *Loop, fd int, cs *connState) *countingTarget {
	t.Helper()
	target := &countingTarget{}
	t.Cleanup(target.closeAll)
	l.transplant.Store(&transplantState{target: target})
	l.tryTransplant(fd)
	if !cs.transplantPending || l.transplantInFlight != 1 {
		t.Fatalf("celeris863 PREMISE: tryTransplant did not defer the hand-off (transplantPending=%v, "+
			"transplantInFlight=%d)", cs.transplantPending, l.transplantInFlight)
	}
	if l.conns[fd] != nil || l.activeConns.Load() != 0 {
		t.Fatalf("celeris863 PREMISE: the conn is still in the table or the gauge (activeConns=%d)",
			l.activeConns.Load())
	}
	return target
}

// TestShutdownClosesADeferredTransplantsDescriptor is the issue: shutdown
// begins while the goroutine's exit entry is not drained yet. The conn is the
// loop's to close: nobody else will ever hold the descriptor.
func TestShutdownClosesADeferredTransplantsDescriptor(t *testing.T) {
	for _, tc := range []struct {
		name       string
		entryFirst bool // the exit entry is queued before shutdown starts
	}{
		{"entry-arrives-during-the-join", false},
		{"entry-already-queued", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hooks := 0
			l := shutdownLoop863(t, func(string) { hooks++ })
			fd, cs := parkedAsyncConn863(t, l)
			target := deferTransplant863(t, l, fd, cs)
			if tc.entryFirst {
				deadline := time.Now().Add(5 * time.Second)
				for l.detachQPending.Load() == 0 {
					if time.Now().After(deadline) {
						t.Fatal("celeris863 PREMISE: the goroutine never queued its exit entry")
					}
					time.Sleep(time.Millisecond)
				}
			}

			l.shutdown()

			if fdIsOpen(fd) {
				t.Errorf("fd %d is still open after shutdown: a deferred transplant's descriptor is "+
					"owned by no engine and nothing will ever close it (celeris#863)", fd)
				_ = unix.Close(fd)
			}
			if l.transplantInFlight != 0 {
				t.Errorf("transplantInFlight = %d after shutdown, want 0 (the hand-off debt was never settled)",
					l.transplantInFlight)
			}
			if got := target.count(); got != 0 {
				t.Errorf("the target was handed %d descriptor(s) by a loop that is shutting down, want 0", got)
			}
			// Shutdown closes every other conn it holds without a hook or a
			// count; this one is no different, and the ledger keeps its
			// pre-shutdown relation (detached - adopted == 1).
			if hooks != 0 || l.closeCount.Load() != 0 {
				t.Errorf("OnDisconnect fired %d times, closeCount = %d, want 0 and 0 (shutdown counts no close)",
					hooks, l.closeCount.Load())
			}
			if d, a := l.transplantDetached.Load(), l.transplantAdopted.Load(); d != 1 || a != 0 {
				t.Errorf("transplant ledger detached=%d adopted=%d, want 1 and 0", d, a)
			}
		})
	}
}

// TestShutdownLeavesAHandedOffConnAlone is the control against over-closing:
// when the hand-off was finished before shutdown, the descriptor is the
// target's, and the entries still naming the connState (the goroutine's exit
// may have queued more than one) must not make shutdown close it.
func TestShutdownLeavesAHandedOffConnAlone(t *testing.T) {
	hooks := 0
	l := shutdownLoop863(t, func(string) { hooks++ })
	fd, cs := parkedAsyncConn863(t, l)
	target := deferTransplant863(t, l, fd, cs)

	deadline := time.Now().Add(5 * time.Second)
	for l.detachQPending.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("celeris863 PREMISE: the goroutine never queued its exit entry")
		}
		time.Sleep(time.Millisecond)
	}
	l.drainDetachQueue() // the normal path: the target takes the descriptor
	if target.count() != 1 || cs.transplantPending {
		t.Fatalf("celeris863 PREMISE: the hand-off did not complete (target has %d, pending=%v)",
			target.count(), cs.transplantPending)
	}
	// Name the connState again, as a second queued entry would.
	l.detachQMu.Lock()
	l.detachQueue = append(l.detachQueue, cs)
	l.detachQPending.Store(1)
	l.detachQMu.Unlock()

	l.shutdown()

	if !fdIsOpen(fd) {
		t.Fatal("shutdown closed a descriptor the target owns")
	}
	if hooks != 0 || l.closeCount.Load() != 0 {
		t.Errorf("OnDisconnect fired %d times and closeCount = %d, want 0 and 0: the conn moved, it did not end",
			hooks, l.closeCount.Load())
	}
}
