//go:build linux

package iouring

import (
	"context"
	"errors"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// celeris#896, the second route to a closed listen socket. Worker shutdown's
// drain (endOwedOpsAtShutdown) cancels the accept before the socket is closed
// only when the worker still has a listen socket at that point. A shutdown with
// an HTTP/2 stream handler still running (h2PoolSettled) or a send drain past
// shutdownSendDrainNanos closes the socket earlier, in stopAccepting, and the
// pause closes it in closeListenerAfterDrain; both go through cancelAccept. These
// tests take the engine out of the way, as shutdown_accept_cancel_896_linux_test.go
// does: a ring stays open, so what holds the port is the armed accept itself.

// fillSQ896 places no-ops (user_data 0, which no dispatch acts on) until the SQ
// ring has no room, and returns how many it placed.
func fillSQ896(t *testing.T, w *Worker) int {
	t.Helper()
	n := 0
	for {
		sqe := w.ring.GetSQE()
		if sqe == nil {
			return n
		}
		*(*uint8)(sqe) = opNOP
		n++
	}
}

// closeRoutes896 are the two callers of cancelAccept.
var closeRoutes896 = []struct {
	name  string
	close func(w *Worker)
}{
	{"stopAccepting", func(w *Worker) { w.stopAccepting(context.Background()) }},
	{"pause-close", func(w *Worker) { w.closeListenerAfterDrain(context.Background()) }},
}

// TestStopAcceptingReleasesTheListenSocket896 is the control: with the SQ ring
// free, both routes leave the port free at once (a plain bind, once, right after
// the close returns).
func TestStopAcceptingReleasesTheListenSocket896(t *testing.T) {
	for _, rt := range closeRoutes896 {
		for _, tc := range tiers896 {
			t.Run(rt.name+"/"+tc.name, func(t *testing.T) {
				r := newAcceptRig896(t, tc.tier(), true)
				rt.close(r.w)
				if r.w.listenFD != -1 {
					t.Fatalf("listenFD = %d after the close, want -1", r.w.listenFD)
				}
				if err := plainBind896(r.addr); err != nil {
					t.Fatalf("a plain bind of %s right after %s: %v", r.addr, rt.name, err)
				}
				if g := r.w.handoffLoss.shutdownListenCancelGaveUp.Load(); g != 0 {
					t.Fatalf("%d give-ups counted with a free SQ ring", g)
				}
			})
		}
	}
}

// TestStopAcceptingWithAFullSQRingReleasesTheListenSocket896: the SQ ring has no
// room when the close runs (the loop had placed a full ring's worth of SQEs since
// its last enter). cancelAccept used to place nothing then and close the
// descriptor with the accept still armed, which held the port for as long as
// the ring was open; it now submits what the ring holds and places the cancel.
func TestStopAcceptingWithAFullSQRingReleasesTheListenSocket896(t *testing.T) {
	for _, rt := range closeRoutes896 {
		for _, tc := range tiers896 {
			t.Run(rt.name+"/"+tc.name, func(t *testing.T) {
				r := newAcceptRig896(t, tc.tier(), true)
				placed := fillSQ896(t, r.w)
				if sqe := r.w.ring.GetSQE(); sqe != nil || placed == 0 {
					t.Fatalf("setup: the SQ ring is not full (placed %d)", placed)
				}
				rt.close(r.w)
				if err := plainBind896(r.addr); err != nil {
					t.Fatalf("a plain bind of %s right after %s with a full SQ ring (%d NOPs queued): %v",
						r.addr, rt.name, placed, err)
				}
				if g := r.w.handoffLoss.shutdownListenCancelGaveUp.Load(); g != 0 {
					t.Fatalf("%d give-ups counted: the ring was submitted and the cancel placed", g)
				}
			})
		}
	}
}

// TestStopAcceptingWithCQEsPendingReleasesTheListenSocket896 repeats the close 200
// times with completions already waiting in the CQ ring, so that a wait for "any
// CQE" is satisfied by those and not by the accept's end. It reports how many
// times the port was still taken.
func TestStopAcceptingWithCQEsPendingReleasesTheListenSocket896(t *testing.T) {
	for _, rt := range closeRoutes896 {
		for _, tc := range tiers896 {
			t.Run(rt.name+"/"+tc.name, func(t *testing.T) {
				busy := 0
				for i := range iterations896 {
					r := newAcceptRig896(t, tc.tier(), true)
					for range 8 {
						sqe := r.w.ring.GetSQE()
						*(*uint8)(sqe) = opNOP
					}
					if _, err := r.w.ring.Submit(); err != nil {
						t.Fatalf("iteration %d: submit NOPs: %v", i, err)
					}
					rt.close(r.w)
					if err := plainBind896(r.addr); err != nil {
						if !errors.Is(err, unix.EADDRINUSE) {
							t.Fatalf("iteration %d: plain bind: %v", i, err)
						}
						busy++
					}
					r.closeRing()
				}
				t.Logf("%s/%s: %d closes with CQEs pending, port still taken after the close in %d (%.0f%%)",
					rt.name, tc.name, iterations896, busy, 100*float64(busy)/float64(iterations896))
				if busy != 0 {
					t.Fatalf("the port was still taken after %s in %d of %d closes", rt.name, busy, iterations896)
				}
			})
		}
	}
}

// TestStopAcceptingWaitForTheAcceptIsBounded896 (RULE 10): a kernel that never
// answers must not hold the close open. The ring wait is answered with nothing
// ever submitted, so no CQE arrives; the close gives up at the drain's bound,
// counts it once, and returns.
func TestStopAcceptingWaitForTheAcceptIsBounded896(t *testing.T) {
	orig := shutdownRingWait
	shutdownRingWait = func(_ *Ring, d time.Duration) error { time.Sleep(d); return nil }
	t.Cleanup(func() { shutdownRingWait = orig })
	for _, rt := range closeRoutes896 {
		t.Run(rt.name, func(t *testing.T) {
			r := newAcceptRig896(t, tiers896[0].tier(), true)
			t0 := time.Now()
			rt.close(r.w)
			took := time.Since(t0)
			g := r.w.handoffLoss.shutdownListenCancelGaveUp.Load()
			t.Logf("%s with a ring that never answers: returned after %v, give-ups counted %d (bound %v)",
				rt.name, took, g, time.Duration(shutdownFDDrainNanos))
			if g != 1 {
				t.Fatalf("give-ups counted = %d, want 1", g)
			}
			if took < time.Duration(shutdownFDDrainNanos)/2 {
				t.Fatalf("returned after %v: the close did not wait for the accept at all", took)
			}
			if took > time.Duration(shutdownFDDrainNanos)+2*time.Second {
				t.Fatalf("took %v: the wait is not bounded by %v", took, time.Duration(shutdownFDDrainNanos))
			}
		})
	}
}
