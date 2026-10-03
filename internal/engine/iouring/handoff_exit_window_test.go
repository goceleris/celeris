//go:build linux

package iouring

import (
	"context"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
)

// celeris#780 (a follow-up of celeris#758/#765). A promoted async conn's
// dispatch goroutine leaves in more ways than the park that claims its own
// hand-off, and each of them has the shape #765 made rerunHandOff respect for
// the claim: the goroutine publishes asyncRun=false under asyncInMu, unlocks,
// and only then enqueues cs for the worker.
//
//   - the processErr exit (a handler error, a write error, "Connection:
//     close") and the panic exit set asyncClosed first; the queued entry runs
//     closeConn(cs.fd);
//   - the h2c-upgrade exit switches the conn to H2C first; the queued entry
//     (asyncH2Promoted) puts cs.fd on h2Conns and cs on the dirty list.
//
// A hand-off re-run in that window (a reap retry, or a reap that lands before
// the queue is drained) read only asyncRun and the claim, so it handed off a
// conn its goroutine had asked to close, or an H2C conn, to the HTTP/1 epoll
// target. The queued close then ran closeConn on the number handOff had just
// closed, which the next accept can hold: another client's connection. The
// queued h2c entry put a stale fd on h2Conns and the handed-off connState on
// the dirty list (the celeris#527 class). tryTransplant's async branch, run
// after every completion while a drain is set, had the same gap for the close
// exits; its protocol gate already refuses an H2C conn.
//
// Every arm drives the real drain, reap, retry and hand-off code on one
// promoted conn of an fdlFixture (the kernel taken out of the loop, so the
// window is an input, not a race to win).

// exitWindowFixture is a promoted async conn whose request the feed path
// answered by arming a recv, with an io_uring->epoll drain set.
func exitWindowFixture(t *testing.T) *fdlFixture {
	t.Helper()
	f := newFDLFixture(t, true)
	f.armFirstRecv() // the recv the feed path armed after the last request
	f.cs.asyncPromoted.Store(true)
	f.startDrain()
	return f
}

// closeExitUpToUnlock is the dispatch goroutine's processErr (and panic) exit
// up to the point where it has published asyncRun=false and released
// asyncInMu, but has not enqueued cs yet.
func closeExitUpToUnlock(f *fdlFixture) {
	f.cs.asyncClosed.Store(true)
	f.cs.asyncInMu.Lock()
	f.cs.asyncInBuf = f.cs.asyncInBuf[:0]
	f.cs.endDispatch() // enqueued after the unlock: that is the hand-back
	f.cs.asyncInMu.Unlock()
}

// h2cExitUpToUnlock is the h2c-upgrade exit to the same point: switchToH2Local
// has made the conn H2C (under detachMu, before the goroutine clears
// asyncRun), and asyncH2Promoted is stored only after the unlock.
func h2cExitUpToUnlock(f *fdlFixture) {
	f.cs.protocol.Store(int32(engine.H2C))
	f.cs.asyncInMu.Lock()
	f.cs.asyncInBuf = f.cs.asyncInBuf[:0]
	f.cs.endDispatch()
	f.cs.asyncInMu.Unlock()
}

// landReaps takes the SQEs placed since the last call and, when they include
// a reap of cs's recv, lands it the way the kernel does when the cancel hits:
// the recv completes with -ECANCELED (reapOutcome, which re-runs the
// hand-off). It returns how many reaps were placed.
func landReaps(t *testing.T, f *fdlFixture, step string) int {
	t.Helper()
	reaps := 0
	for _, s := range takeSQEs(f.w.ring) {
		if f.isReap(s) {
			reaps++
		}
	}
	t.Logf("celeris780 %s placed %d reap(s)", step, reaps)
	if reaps > 0 && f.w.conns[f.fd] == f.cs {
		f.process(f.recvCQE(-int32(unix.ECANCELED)))
	}
	return reaps
}

// strangerOnTheNumber models the next accept on this worker reusing the
// descriptor number a hand-off closed: a live connState on a real socket,
// dup'd onto that number so the test does not depend on which free number the
// kernel picks.
func strangerOnTheNumber(t *testing.T, f *fdlFixture) *connState {
	t.Helper()
	a, b := socketPairFDs(t)
	t.Cleanup(func() { _ = unix.Close(b) })
	if a != f.fd {
		if err := unix.Dup3(a, f.fd, unix.O_CLOEXEC); err != nil {
			t.Fatalf("dup3: %v", err)
		}
		_ = unix.Close(a)
	}
	next := acquireConnState(context.Background(), f.fd, 4096, true)
	next.writeFn = f.w.makeWriteFn(next)
	next.protocol.Store(int32(engine.HTTP1))
	next.detected = true
	f.w.initProtocol(next)
	f.w.conns[f.fd] = next
	f.w.connCount++
	f.w.addLiveConn(next)
	f.w.activeConns.Add(1)
	return next
}

// TestReapRerunLeavesAnExitingDispatchToItsExit pins rerunHandOff's two sites,
// the reap retry (retryReaps, at the head of drainDetachQueue) and the reap
// landing (reapOutcome), against the goroutine's close and h2c exits.
func TestReapRerunLeavesAnExitingDispatchToItsExit(t *testing.T) {
	// The ordering control: the exit's enqueue lands before the drain that
	// runs the retry. The retry still runs first (retryReaps is at the head
	// of drainDetachQueue), but whatever it starts, the queued close runs in
	// the same drain, before a reap can land. Passes on every tree: it pins
	// the outcome the window arms below must match.
	t.Run("close_exit_enqueued_before_the_retry_drain_CONTROL", func(t *testing.T) {
		f := exitWindowFixture(t)
		f.w.queueReapRetry(f.cs) // a reap that missed while the goroutine ran the request
		closeExitUpToUnlock(f)
		f.w.enqueueDetach(f.cs)
		f.w.drainDetachQueue()
		reaps := landReaps(t, f, "control retry+drain")
		t.Logf("celeris780 CONTROL adopted=%d reaps=%d slot_owned=%v", f.tgt.adopted.Load(), reaps, f.w.conns[f.fd] == f.cs)
		if n := f.tgt.adopted.Load(); n != 0 {
			t.Errorf("a conn its goroutine exited to close was handed off %d time(s)", n)
		}
		if f.w.conns[f.fd] == f.cs {
			t.Errorf("the queued close did not close the conn")
		}
	})

	t.Run("close_exit_retry_between_unlock_and_enqueue", func(t *testing.T) {
		f := exitWindowFixture(t)
		f.w.queueReapRetry(f.cs)
		closeExitUpToUnlock(f)
		f.w.drainDetachQueue() // the retry runs; the exit's entry is not on the queue yet
		reaps := landReaps(t, f, "the retry")
		handed := f.tgt.adopted.Load()
		var stranger *connState
		if f.w.conns[f.fd] == nil {
			stranger = strangerOnTheNumber(t, f)
		}
		f.w.enqueueDetach(f.cs) // the exit's enqueue
		f.w.drainDetachQueue()  // asyncClosed -> closeConn(cs.fd)
		strangerClosed := stranger != nil && (f.w.conns[f.fd] != stranger || stranger.closing)
		t.Logf("celeris780 CLOSE-RETRY adopted=%d reaps=%d stranger_closed=%v", handed, reaps, strangerClosed)
		if handed != 0 {
			t.Errorf("celeris#780: a conn whose dispatch goroutine exited to CLOSE it (asyncClosed) was handed "+
				"off %d time(s) by a reap retry in the exit's unlock->enqueue window", handed)
		}
		if strangerClosed {
			t.Errorf("celeris#780: the queued asyncClosed entry then ran closeConn(%d) on the connection that "+
				"reused the number", f.fd)
		}
		if stranger == nil && f.w.conns[f.fd] == f.cs {
			t.Errorf("the queued close did not close the conn")
		}
		if stranger != nil && f.w.conns[f.fd] == stranger {
			f.w.conns[f.fd] = nil // the fixture's cleanup closes f.fd once
			_ = unix.Close(f.fd)
		}
	})

	t.Run("h2c_exit_retry_between_unlock_and_enqueue", func(t *testing.T) {
		f := exitWindowFixture(t)
		f.w.queueReapRetry(f.cs)
		h2cExitUpToUnlock(f)
		f.w.drainDetachQueue() // the retry runs; the exit's entry is not on the queue yet
		reaps := landReaps(t, f, "the retry")
		handed := f.tgt.adopted.Load()
		f.cs.asyncH2Promoted.Store(true) // the exit's store after the unlock
		f.w.enqueueDetach(f.cs)
		f.w.drainDetachQueue() // asyncH2Promoted -> h2Conns += cs.fd; markDirty(cs)
		staleDirty := handed != 0 && f.cs.dirty
		t.Logf("celeris780 H2C-RETRY adopted=%d reaps=%d h2Conns=%v stale_dirty=%v", handed, reaps, f.w.h2Conns, staleDirty)
		if handed != 0 {
			t.Errorf("celeris#780: an h2c-upgraded conn (protocol H2C) was handed to the HTTP/1 epoll target "+
				"%d time(s) by a reap retry in the upgrade exit's window", handed)
		}
		if staleDirty {
			t.Errorf("celeris#780: the queued asyncH2Promoted entry then put the handed-off connState on the "+
				"dirty list and fd %d on h2Conns", f.fd)
		}
		if f.cs.dirty {
			f.w.removeDirty(f.cs)
		}
	})

	// The landing site: a reap placed for the previous claim lands
	// (-ECANCELED) after the goroutine that the next request respawned has
	// exited to close the conn, in the same CQE batch, before the drain.
	for _, tc := range []struct {
		name string
		exit func(*fdlFixture)
	}{
		{"close_exit_reap_lands_before_the_drain", closeExitUpToUnlock},
		{"h2c_exit_reap_lands_before_the_drain", h2cExitUpToUnlock},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := exitWindowFixture(t)
			f.w.finishAsyncTransplant(f.cs) // the previous claim, drained: it reaps the recv
			if sqes := takeSQEs(f.w.ring); len(sqes) != 1 || !f.isReap(sqes[0]) {
				t.Fatalf("apparatus: finishing the previous claim placed %v, want one reap", sqes)
			}
			tc.exit(f)
			f.process(f.recvCQE(-int32(unix.ECANCELED))) // lands in the CQE batch, before the drain
			handed := f.tgt.adopted.Load()
			t.Logf("celeris780 LANDING arm=%s adopted=%d", tc.name, handed)
			if handed != 0 {
				t.Errorf("celeris#780: a conn whose goroutine had exited (%s) was handed off %d time(s) at a reap "+
					"landing", tc.name, handed)
			}
		})
	}
}

// TestTryTransplantLeavesAClosingAsyncConnAlone pins tryTransplant's async
// branch, which every recv and send completion runs while a drain is set,
// against the close exit: the conn is its goroutine's to close. Arm
// recv_armed would place a reap for it (and hand it off when the reap lands);
// arm recv_dropped, a conn whose recv arm the SQ ring dropped, would be handed
// off on the spot. Either way the queued close then acts on a number the
// hand-off gave up.
func TestTryTransplantLeavesAClosingAsyncConnAlone(t *testing.T) {
	for _, armed := range []bool{true, false} {
		name := "recv_armed"
		if !armed {
			name = "recv_dropped"
		}
		t.Run(name, func(t *testing.T) {
			f := newFDLFixture(t, true)
			if armed {
				f.armFirstRecv()
			} else {
				f.cs.needsRecv = true // the feed path's re-arm found the SQ ring full
			}
			f.cs.asyncPromoted.Store(true)
			f.startDrain()
			closeExitUpToUnlock(f)
			f.w.tryTransplant(f.fd) // a completion of the conn, in the exit's window
			reaps := landReaps(t, f, name)
			handed := f.tgt.adopted.Load()
			var stranger *connState
			if f.w.conns[f.fd] == nil {
				stranger = strangerOnTheNumber(t, f)
			}
			f.w.enqueueDetach(f.cs)
			f.w.drainDetachQueue()
			strangerClosed := stranger != nil && (f.w.conns[f.fd] != stranger || stranger.closing)
			t.Logf("celeris780 TRYTRANSPLANT arm=%s adopted=%d reaps=%d stranger_closed=%v", name, handed, reaps, strangerClosed)
			if handed != 0 || reaps != 0 {
				t.Errorf("celeris#780: tryTransplant acted on a conn whose dispatch goroutine exited to close it: "+
					"%d hand-off(s), %d reap(s); want none", handed, reaps)
			}
			if strangerClosed {
				t.Errorf("celeris#780: the queued asyncClosed entry then ran closeConn(%d) on the connection "+
					"that reused the number", f.fd)
			}
			if stranger == nil && f.w.conns[f.fd] == f.cs {
				t.Errorf("the queued close did not close the conn")
			}
			if stranger != nil && f.w.conns[f.fd] == stranger {
				f.w.conns[f.fd] = nil
				_ = unix.Close(f.fd)
			}
		})
	}
}
