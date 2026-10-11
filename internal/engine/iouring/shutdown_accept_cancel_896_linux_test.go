//go:build linux

package iouring

import (
	"errors"
	"runtime"
	"testing"
	"time"
	"unsafe"

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
	w          *Worker
	addr       string
	ring       *Ring
	ringClosed bool
}

// closeRing closes the rig's ring now, for a test that makes many rigs; the
// cleanup then leaves it.
func (r *acceptRig896) closeRing() {
	if !r.ringClosed {
		r.ringClosed = true
		_ = r.ring.Close()
	}
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
	rig := &acceptRig896{ring: ring}
	t.Cleanup(func() { rig.closeRing() })
	addr := freeAddr639(t)
	lfd, err := createListenSocket(addr, false)
	if err != nil {
		t.Fatalf("createListenSocket(%s): %v", addr, err)
	}
	w := newLedgerWorker(lfd)
	w.ring = ring
	rig.w, rig.addr = w, addr
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
	return rig
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

// TestCancelAllWithNothingArmedAnswersZero896 pins the kernel fact the drain's
// wait is built on, so a kernel that changes it fails here and not as a shutdown
// that sits out its bound: an ASYNC_CANCEL with CANCEL_FD|CANCEL_ALL that
// matches nothing completes with res 0 (a success, __io_async_cancel returns
// the number cancelled), and the form that skips its success CQE posts
// nothing at all for it. The keep-the-CQE form posts exactly one CQE, res 0,
// and with an accept armed it cancels one (res 1) and the accept ends with
// -ECANCELED and F_MORE clear.
func TestCancelAllWithNothingArmedAnswersZero896(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	ring, err := NewRing(64, 0, 0)
	if err != nil {
		skipOrFail656(t, "io_uring unavailable: %v", err)
	}
	defer func() { _ = ring.Close() }()
	lfd, err := createListenSocket(freeAddr639(t), false)
	if err != nil {
		t.Fatalf("createListenSocket: %v", err)
	}
	defer func() { _ = unix.Close(lfd) }()

	reap := func() []completionEntry {
		var out []completionEntry
		head, tail := ring.BeginCQ()
		for ; head != tail; head++ {
			out = append(out, *ring.cqeAt(head))
		}
		ring.EndCQ(head)
		return out
	}
	submit := func(prep func(unsafe.Pointer, int), ud uint64) []completionEntry {
		sqe := ring.GetSQE()
		prep(sqe, lfd)
		setSQEUserData(sqe, ud)
		if err := ring.SubmitAndWaitTimeout(20 * time.Millisecond); err != nil {
			t.Fatalf("submit: %v", err)
		}
		return reap()
	}

	if got := submit(prepCancelFDSkipSuccess, udListenCancel); len(got) != 0 {
		t.Fatalf("the skip-success cancel of nothing posted %d CQEs %+v, want none", len(got), got)
	}
	got := submit(prepCancelFDDriver, udListenCancel)
	if len(got) != 1 || got[0].UserData != udListenCancel || got[0].Res != 0 {
		t.Fatalf("the keep-the-CQE cancel of nothing posted %+v, want one CQE with res 0", got)
	}

	sqe := ring.GetSQE()
	prepMultishotAccept(sqe, lfd)
	setSQEUserData(sqe, encodeUserData(udAccept, lfd))
	if _, err := ring.Submit(); err != nil {
		t.Fatalf("submit accept: %v", err)
	}
	got = submit(prepCancelFDDriver, udListenCancel)
	var cancelRes int32 = -1
	var acceptEnded bool
	for _, c := range got {
		switch c.UserData & udMask {
		case udListenCancel:
			cancelRes = c.Res
		case udAccept:
			acceptEnded = !cqeHasMore(c.Flags) && c.Res == -int32(unix.ECANCELED)
		}
	}
	if cancelRes != 1 || !acceptEnded {
		t.Fatalf("cancel of an armed accept: cancel res %d (want 1), accept ended with -ECANCELED and no F_MORE %v (want true); CQEs %+v",
			cancelRes, acceptEnded, got)
	}
}
