//go:build linux

package iouring

import (
	"errors"
	"runtime"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// celeris#896, the mechanism with the engine taken out of the way: a ring that
// has an accept armed on a listen socket keeps that socket bound after the
// descriptor is closed, and endOwedOpsAtShutdown (worker shutdown's drain)
// cancels the accept and waits for it to end, so the close is the last
// reference. The ring stays open throughout, so what holds the port is not the
// ring's asynchronous teardown but the armed accept itself: the control below
// is deterministic where the engine-level tests measure a ~2 ms window.

type acceptRig896 struct {
	w    *Worker
	addr string
}

// newAcceptRig896 makes a worker with a ring of the tier's kind and a listen
// socket on a free port, with the accept armed and submitted when arm is set.
// The goroutine is locked to its thread: a SINGLE_ISSUER ring belongs to one.
func newAcceptRig896(t *testing.T, tier TierStrategy, arm bool) *acceptRig896 {
	t.Helper()
	runtime.LockOSThread()
	t.Cleanup(runtime.UnlockOSThread)
	ring, err := NewRing(64, tier.SetupFlags(), 0)
	if err != nil {
		skipOrFail656(t, "io_uring unavailable for tier %v: %v", tier.Tier(), err)
	}
	t.Cleanup(func() { _ = ring.Close() })
	addr := freeAddr639(t)
	lfd, err := createListenSocket(addr, false)
	if err != nil {
		t.Fatalf("createListenSocket(%s): %v", addr, err)
	}
	w := newLedgerWorker(lfd)
	w.ring = ring
	w.tier = tier
	w.listenFD = lfd
	w.handoffLoss = &handoffLossStats{}
	t.Cleanup(func() {
		if w.listenFD >= 0 {
			_ = unix.Close(w.listenFD)
		}
	})
	if arm {
		w.prepareAccept()
		if _, err := w.ring.Submit(); err != nil {
			t.Fatalf("submit accept: %v", err)
		}
	}
	return &acceptRig896{w: w, addr: addr}
}

// closeListener closes the worker's listen descriptor, as shutdown does after
// the drain.
func (r *acceptRig896) closeListener() {
	_ = unix.Close(r.w.listenFD)
	r.w.listenFD = -1
}

var tiers896 = []struct {
	name string
	tier func() TierStrategy
}{
	{"base-single-shot-accept", func() TierStrategy { return &baseTier{} }},
	{"high-multishot-accept", func() TierStrategy { return &highTier{multishotAccept: true} }},
	{"high-deferred-taskrun", func() TierStrategy { return &highTier{multishotAccept: true, deferTaskrun: true} }},
}

// TestArmedAcceptHoldsTheClosedListenSocket896 is the control: with the accept
// armed and no cancel, closing the descriptor leaves the socket bound, and it
// stays so for as long as the ring is open (a plain bind is refused every time
// it is tried over 50 ms). It is why the engine-level tests' oracle can see the
// defect, and that the cancel is what ends it.
func TestArmedAcceptHoldsTheClosedListenSocket896(t *testing.T) {
	for _, tc := range tiers896 {
		t.Run(tc.name, func(t *testing.T) {
			r := newAcceptRig896(t, tc.tier(), true)
			r.closeListener()
			end := time.Now().Add(50 * time.Millisecond)
			for n := 0; time.Now().Before(end); n++ {
				if err := plainBind896(r.addr); err == nil {
					t.Fatalf("a plain bind of %s succeeded %d tries after the close with the accept still armed: "+
						"the oracle cannot see the held socket", r.addr, n)
				} else if !errors.Is(err, unix.EADDRINUSE) {
					t.Fatalf("plain bind: %v, want EADDRINUSE", err)
				}
				time.Sleep(time.Millisecond)
			}
		})
	}
}

// TestShutdownDrainReleasesTheListenSocket896: the same, after the drain
// worker shutdown runs: the first plain bind succeeds, no give-up is counted,
// and the drain took nowhere near its bound.
func TestShutdownDrainReleasesTheListenSocket896(t *testing.T) {
	for _, tc := range tiers896 {
		t.Run(tc.name, func(t *testing.T) {
			r := newAcceptRig896(t, tc.tier(), true)
			t0 := time.Now()
			r.w.endOwedOpsAtShutdown()
			took := time.Since(t0)
			r.closeListener()
			if err := plainBind896(r.addr); err != nil {
				t.Fatalf("a plain bind of %s right after the drain and the close: %v", r.addr, err)
			}
			if g := r.w.handoffLoss.shutdownListenCancelGaveUp.Load(); g != 0 {
				t.Fatalf("the drain gave up on the accept %d times", g)
			}
			if took > time.Duration(shutdownFDDrainNanos)/2 {
				t.Fatalf("the drain took %v with nothing to wait for but the accept", took)
			}
			t.Logf("drain took %v", took)
		})
	}
}

// TestShutdownDrainWithNoAcceptArmedDoesNotWait896: a worker whose accept is
// not armed (it ended on an error and the re-arm is pending, say) has the cancel
// answered 0 (IORING_ASYNC_CANCEL_ALL counts what it cancelled), which says
// nothing holds the socket: the drain is over at once and counts no give-up,
// instead of waiting out its bound for a completion that will never come. A
// cancel that skips its success CQE posts nothing for that answer, which is
// why the drain's cancel keeps it.
func TestShutdownDrainWithNoAcceptArmedDoesNotWait896(t *testing.T) {
	for _, tc := range tiers896 {
		t.Run(tc.name, func(t *testing.T) {
			r := newAcceptRig896(t, tc.tier(), false)
			t0 := time.Now()
			r.w.endOwedOpsAtShutdown()
			took := time.Since(t0)
			r.closeListener()
			if err := plainBind896(r.addr); err != nil {
				t.Fatalf("a plain bind of %s: %v", r.addr, err)
			}
			if g := r.w.handoffLoss.shutdownListenCancelGaveUp.Load(); g != 0 {
				t.Fatalf("the drain gave up %d times on a socket with no accept armed", g)
			}
			if took > time.Duration(shutdownFDDrainNanos)/2 {
				t.Fatalf("the drain waited %v for an accept that was never armed", took)
			}
			t.Logf("drain took %v", took)
		})
	}
}
