//go:build linux

package epoll

// celeris#885 items 27 and 36: onPeerHalfClose read the conn's h1State, its
// write state (csWritePending) and, through markClosing, its pending byte
// count (csPendingBytes) on the loop thread with no detachMu, while a dispatch
// goroutine's switchToH2Local sets h1State to nil, and its writeFn appends to
// the buffers, under that lock: the data race of celeris#865, left unfixed
// there because EPOLLRDHUP is edge-triggered and a skipped half-close does not
// come again. The read now takes the lock with TryLock; when a running handler
// holds it the half-close is owed (halfCloseOwed), the conn is given up
// (relink) and the goroutine's hand-back runs onPeerHalfClose again.
//
// The race tests drive onPeerHalfClose on the test goroutine, standing in for
// the loop thread, against a goroutine that performs switchToH2Local's writes
// and a writeFn's appends under the lock it holds, as
// TestCheckTimeoutsReadsH1StateUnderDetachMu does; they need -race.

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/engine"
)

// wantFlips885 is the floor of mutations the writer goroutine must have made
// before a race arm may pass: a run in which the two never interleaved proves
// nothing.
const wantFlips885 = 200

// halfCloseRace885 runs onPeerHalfClose(fd) on this goroutine against a writer
// that, under cs.detachMu, flips h1State between nil and h1 and rewrites the
// queued bytes, until the writer has run wantFlips885 times. prep runs before
// each call (loop-thread work).
func halfCloseRace885(t *testing.T, l *Loop, cs *connState, h1 *conn.H1State, prep func()) {
	t.Helper()
	stop := make(chan struct{})
	var flips atomic.Int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			// switchToH2Local's writes, in its order, under its lock; the
			// queued bytes stay non-empty, so a correct reader never closes.
			cs.detachMu.Lock()
			cs.h1State = nil
			cs.h2State = &conn.H2State{}
			cs.protocol = engine.H2C
			cs.writeBuf = append(cs.writeBuf[:0], 'y', 'z')
			cs.detachMu.Unlock()
			cs.detachMu.Lock()
			cs.h1State = h1
			cs.h2State = nil
			cs.protocol = engine.HTTP1
			cs.writeBuf = append(cs.writeBuf[:0], 'x')
			cs.detachMu.Unlock()
			flips.Add(1)
		}
	}()
	deadline := time.Now().Add(20 * time.Second)
	calls := 0
	for flips.Load() < wantFlips885 && time.Now().Before(deadline) {
		if prep != nil {
			prep()
		}
		l.onPeerHalfClose(cs.fd)
		calls++
		runtime.Gosched() // never starve the writer this test races
	}
	close(stop)
	wg.Wait()
	if got := flips.Load(); got < wantFlips885 {
		t.Fatalf("celeris885 RACE PREMISE: the writer ran %d times in %d calls, want %d; the injection did "+
			"not fire often enough for the read and the write to interleave", got, calls, wantFlips885)
	}
	t.Logf("celeris885 onPeerHalfClose calls=%d writer_flips=%d", calls, flips.Load())
}

// halfCloseRig885 is a loop with one async conn that has bytes queued.
func halfCloseRig885(t *testing.T) (*Loop, *connState, *conn.H1State) {
	t.Helper()
	l := newLedgerLoop(t)
	_, cs := asyncConn(t, l)
	cs.asyncPromoted = false
	cs.writeBuf = append(cs.writeBuf[:0], 'x')
	return l, cs, cs.h1State
}

// TestOnPeerHalfCloseReadsUnderDetachMu885 is the race with a BOUNDED holder
// (a guarded writeFn, one write): the read waits for the lock, which is short.
func TestOnPeerHalfCloseReadsUnderDetachMu885(t *testing.T) {
	l, cs, h1 := halfCloseRig885(t)
	halfCloseRace885(t, l, cs, h1, func() {
		cs.closeSince = 0 // markClosing counts the queue only the first time
		cs.peerClosed = false
	})
	if l.conns[cs.fd] != cs || l.closeCount.Load() != 0 {
		t.Fatalf("celeris885: onPeerHalfClose closed a conn that had bytes queued (closeCount=%d)", l.closeCount.Load())
	}
}

// TestOnPeerHalfCloseReadsUnderDetachMuOfARunningHandler885 is the same race
// with the dispatch goroutine RUNNING (inside a handler), the only state
// switchToH2Local runs in. The half-close must not be read from at all while
// the lock is held; whichever way each call goes, the reads are under the lock.
func TestOnPeerHalfCloseReadsUnderDetachMuOfARunningHandler885(t *testing.T) {
	l, cs, h1 := halfCloseRig885(t)
	cs.asyncRun = true
	cs.asyncParked = false
	halfCloseRace885(t, l, cs, h1, func() {
		cs.closeSince = 0
		cs.peerClosed = false
		cs.relinkPending = false
	})
	if l.conns[cs.fd] != cs || l.closeCount.Load() != 0 {
		t.Fatalf("celeris885: onPeerHalfClose closed a conn that had bytes queued (closeCount=%d)", l.closeCount.Load())
	}
}

// TestOnPeerHalfCloseDoesNotWaitForARunningHandler885 is the second control:
// the race tests alone are passed by a blocking Lock, which parks the loop
// thread, and every conn on it, behind one slow handler (celeris#593, #669).
// With detachMu held by a running handler onPeerHalfClose must return at once,
// and what it could not read it leaves owed: the conn is given up to its
// goroutine's hand-back (relinkOwed), and the half-close is not turned into a
// close request decided from state it never read (asyncClosed).
func TestOnPeerHalfCloseDoesNotWaitForARunningHandler885(t *testing.T) {
	l, cs, _ := halfCloseRig885(t)
	cs.writeBuf = cs.writeBuf[:0] // nothing queued: unlocked, this would close the conn
	release := holdAsHandler(t, cs, true)
	if !returnsWhileHeld(t, release, func() { l.onPeerHalfClose(cs.fd) }) {
		t.Fatalf("celeris885: onPeerHalfClose waited %v on a detachMu held by a running handler", stallWait)
	}
	cs.asyncInMu.Lock()
	owed := cs.relinkOwed
	cs.asyncInMu.Unlock()
	if !owed {
		t.Errorf("celeris885: the half-close of a conn in a running handler is not owed to its goroutine's hand-back (relinkOwed is false)")
	}
	if cs.asyncClosed.Load() || l.closeCount.Load() != 0 {
		t.Errorf("celeris885: the half-close of a conn in a running handler was decided from state the loop could not read: asyncClosed=%v closeCount=%d",
			cs.asyncClosed.Load(), l.closeCount.Load())
	}
}

// handBack885 does what the dispatch goroutine does at its next park
// (serveAsync) or exit (endDispatch): hand the conn back through the detach
// queue if it was owed one, for a relink or for a close. The loop then drains the queue, as the run loop does after a batch.
func handBack885(l *Loop, cs *connState) {
	cs.asyncInMu.Lock()
	owed := cs.relinkOwed || cs.closeOwed
	cs.relinkOwed = false
	cs.closeOwed = false
	cs.asyncInMu.Unlock()
	if owed {
		l.enqueueDetach(cs)
	}
	l.drainDetachQueue()
}

// TestOwedHalfCloseIsServedAtTheHandBack885: EPOLLRDHUP is edge-triggered, so
// a half-close found while a handler runs does not come again; the hand-back
// must end in a close (nothing queued) or in a deferred close (bytes queued:
// peerClosed, the clock started, the conn back on the dirty list to flush).
func TestOwedHalfCloseIsServedAtTheHandBack885(t *testing.T) {
	t.Run("nothing queued closes", func(t *testing.T) {
		l, cs, _ := halfCloseRig885(t)
		cs.writeBuf = cs.writeBuf[:0]
		fd := cs.fd
		release := holdAsHandler(t, cs, true)
		l.onPeerHalfClose(fd)
		release()
		cs.asyncInMu.Lock()
		cs.asyncRun = false // the goroutine has parked or gone
		cs.asyncInMu.Unlock()
		handBack885(l, cs)
		if l.conns[fd] != nil || l.closeCount.Load() != 1 {
			t.Fatalf("celeris885: the half-close found under a running handler was lost at the hand-back: slot=%p closeCount=%d",
				l.conns[fd], l.closeCount.Load())
		}
		if l.dirtyHead != nil {
			t.Errorf("celeris885: the conn the hand-back closed (and released to the pool) was put on the dirty list")
		}
	})
	t.Run("bytes queued defers the close", func(t *testing.T) {
		l, cs, _ := halfCloseRig885(t)
		fd := cs.fd
		release := holdAsHandler(t, cs, true)
		l.onPeerHalfClose(fd)
		release()
		cs.asyncInMu.Lock()
		cs.asyncRun = false
		cs.asyncInMu.Unlock()
		handBack885(l, cs)
		if l.conns[fd] != cs {
			t.Fatalf("celeris885: a conn with bytes queued was closed at the hand-back")
		}
		if !cs.peerClosed || cs.closeSince == 0 {
			t.Errorf("celeris885: the deferred close was not started at the hand-back: peerClosed=%v closeSince=%d", cs.peerClosed, cs.closeSince)
		}
		if !cs.dirty {
			t.Errorf("celeris885: the conn is not back on the dirty list after the hand-back, so nothing flushes it and the deferred close never completes")
		}
		if cs.closePending != len(cs.writeBuf) {
			t.Errorf("celeris885: closePending = %d, want the %d queued bytes, counted under the lock", cs.closePending, len(cs.writeBuf))
		}
	})
	t.Run("busy again at the hand-back is owed again", func(t *testing.T) {
		l, cs, _ := halfCloseRig885(t)
		cs.writeBuf = cs.writeBuf[:0]
		fd := cs.fd
		release := holdAsHandler(t, cs, true)
		l.onPeerHalfClose(fd)
		// The goroutine hands the conn back at its park, but a pipelined
		// request has started another handler by the time the loop drains.
		cs.asyncInMu.Lock()
		cs.relinkOwed = false
		cs.asyncInMu.Unlock()
		l.enqueueDetach(cs)
		l.drainDetachQueue()
		if l.conns[fd] != cs || l.closeCount.Load() != 0 {
			t.Fatalf("celeris885: the hand-back closed a conn whose handler is running")
		}
		cs.asyncInMu.Lock()
		owed := cs.relinkOwed
		cs.asyncInMu.Unlock()
		if !owed {
			t.Errorf("celeris885: the half-close, still unread because a handler runs, was dropped at the hand-back instead of owed again")
		}
		if cs.dirty {
			t.Errorf("celeris885: a conn given up again is on the dirty list")
		}
		release()
		cs.asyncInMu.Lock()
		cs.asyncRun = false
		cs.asyncInMu.Unlock()
		handBack885(l, cs)
		if l.conns[fd] != nil || l.closeCount.Load() != 1 {
			t.Fatalf("celeris885: the half-close was lost at the second hand-back: slot=%p closeCount=%d", l.conns[fd], l.closeCount.Load())
		}
	})
}

// TestOwedHalfCloseIsServedAtTheH2UpgradeHandBack885: the hand-back of an h2c
// upgrade is the other entry the dispatch goroutine leaves (asyncH2Promoted),
// and drainDetachQueue's branch for it continues before the tail where the
// relink hand-back is settled. A half-close owed to the goroutine that was in
// the handler of the upgrade request must be made there too: nothing else will
// ever make it.
func TestOwedHalfCloseIsServedAtTheH2UpgradeHandBack885(t *testing.T) {
	l, cs, _ := halfCloseRig885(t)
	cs.writeBuf = cs.writeBuf[:0]
	fd := cs.fd
	release := holdAsHandler(t, cs, true)
	l.onPeerHalfClose(fd)
	release()
	// What the goroutine leaves behind after switchToH2Local: h2 state, the
	// promotion flag, one entry on the queue, and no goroutine.
	cs.h1State = nil
	cs.h2State = conn.NewH2State(&asyncReuseHandler{}, conn.H2Config{}, func([]byte) {}, nil)
	cs.protocol = engine.H2C
	cs.asyncInMu.Lock()
	cs.asyncRun = false
	cs.relinkOwed = false
	cs.asyncInMu.Unlock()
	cs.asyncH2Promoted.Store(true)
	l.enqueueDetach(cs)
	l.drainDetachQueue()
	if l.conns[fd] != nil || l.closeCount.Load() != 1 {
		t.Fatalf("celeris885: the half-close owed to the goroutine that upgraded the conn was lost at its hand-back: slot=%p closeCount=%d",
			l.conns[fd], l.closeCount.Load())
	}
	if len(l.h2Conns) != 0 || l.dirtyHead != nil {
		t.Errorf("celeris885: the closed upgrade conn is still on a list: h2Conns=%v dirtyHead=%p", l.h2Conns, l.dirtyHead)
	}
}

// TestOnPeerHalfCloseLeavesAHijackedConnAlone885 pins the early return of
// onPeerHalfClose for a conn whose handler hijacked it after the run loop read
// the slot (celeris#668): the half-close is neither marked on it nor made into
// a close, whether or not bytes are queued. The slot is left holding cs, the
// state the loop thread sees when it read the slot just before hijackConn
// cleared it. The return is defensive (closeConn re-validates ownership under
// the lock), so this is a pin and not a failing-first test.
func TestOnPeerHalfCloseLeavesAHijackedConnAlone885(t *testing.T) {
	for _, tc := range []struct {
		name   string
		queued bool
	}{{"bytes queued", true}, {"nothing queued", false}} {
		t.Run(tc.name, func(t *testing.T) {
			l, cs, _ := halfCloseRig885(t)
			if !tc.queued {
				cs.writeBuf = cs.writeBuf[:0]
			}
			cs.hijacked.Store(true)
			l.onPeerHalfClose(cs.fd)
			if cs.peerClosed || cs.closeSince != 0 {
				t.Errorf("celeris885: a hijacked conn was marked closing: peerClosed=%v closeSince=%d", cs.peerClosed, cs.closeSince)
			}
			if l.closeCount.Load() != 0 || cs.asyncClosed.Load() {
				t.Errorf("celeris885: a hijacked conn was closed from a half-close read before the hijack: closeCount=%d asyncClosed=%v",
					l.closeCount.Load(), cs.asyncClosed.Load())
			}
			if cs.halfCloseOwed {
				t.Errorf("celeris885: a hijacked conn was owed a half-close")
			}
			// The lock was released on the way out.
			if !cs.detachMu.TryLock() {
				t.Fatalf("celeris885: onPeerHalfClose returned with detachMu held")
			}
			cs.detachMu.Unlock()
		})
	}
}
