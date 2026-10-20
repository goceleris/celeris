//go:build linux

package iouring

import (
	"testing"
	"time"
)

// TestIouringDirtyPassGiveUpSetsTheSendWatch811: the give-up leaves a parked
// worker with no spin to reach its timeout sweep, so it sets sendWatch; the
// loop then sweeps about every 1 to 2 s (checked on every 8th iteration),
// and a sweep that finds no conn with a SEND outstanding ends the watch.
func TestIouringDirtyPassGiveUpSetsTheSendWatch811(t *testing.T) {
	const tail = "the rest of response 1"
	rig := newStallRig704(t)
	w, cs := rig.w, rig.cs
	w.cfg.WriteTimeout = time.Hour
	w.cfg.IdleTimeout = time.Hour
	w.cfg.ReadTimeout = time.Hour
	inFlight750(rig, tail, false)
	takeSQEs(w.ring)
	if w.sendWatch {
		t.Fatal("apparatus: the watch is set before any give-up")
	}
	w.markDirty(cs)
	w.flushDirty()
	if !w.sendWatch {
		t.Fatalf("celeris#811: the dirty pass gave up a conn with a SEND outstanding and did not set sendWatch: " +
			"a parked worker would sweep for its WriteTimeout only at the idle gate, up to ~100 s apart")
	}

	for _, tc := range []struct {
		name     string
		tick     uint32
		sweptAgo time.Duration
		want     bool
	}{
		{"due", 8, 2 * time.Second, true},
		{"swept_just_now", 8, 10 * time.Millisecond, false},
		{"not_an_8th_iteration", 9, 2 * time.Second, false},
	} {
		w.tickCounter = tc.tick
		w.lastSweep = time.Now().Add(-tc.sweptAgo).UnixNano()
		if got := w.sweepWatchDue(); got != tc.want {
			t.Errorf("sweepWatchDue %s (tick %d, last sweep %v ago) = %v, want %v", tc.name, tc.tick, tc.sweptAgo, got, tc.want)
		}
	}

	w.checkTimeouts() // the SEND is still outstanding: the watch stays
	if !w.sendWatch || w.lastSweep == 0 {
		t.Errorf("a sweep that finds a conn with a SEND outstanding: sendWatch=%v lastSweep=%d, want true and set", w.sendWatch, w.lastSweep)
	}
	w.tickCounter = 8
	if w.sweepWatchDue() {
		t.Errorf("sweepWatchDue right after a sweep")
	}

	w.handleSend(sendCQE750(rig, int32(len(tail)), 0), rig.local, time.Now().UnixNano())
	if cs.sending {
		t.Fatalf("apparatus: the SEND is still outstanding after its completion")
	}
	w.checkTimeouts()
	if w.sendWatch {
		t.Errorf("a sweep that finds no conn with a SEND outstanding left sendWatch set: every sweep would stay on the 1 s cadence")
	}
}

// TestIouringRemainderSendSetsTheWatches884: sendWatch alone cannot keep the
// worker out of its untimed wait (mode 3a, waitUntimed): every sweep derives it
// from any SEND in flight, so under load it is nearly always on, and mode 3a
// would be lost for good. The re-send of a partial send's remainder, a SEND
// the client's closed window keeps outstanding, is what flushSend marks
// (cs.remainderOut, w.remainderWatch, and sendWatch with them), and so is a
// first SEND of longSendBytes or more; the SEND of a small response is not
// marked, so the common request/response cycle keeps the untimed wait. The
// marks end with the SEND's completion, and a sweep re-derives the worker's
// from the live conns.
func TestIouringRemainderSendSetsTheWatches884(t *testing.T) {
	rig := newStallRig704(t)
	w, cs := rig.w, rig.cs
	w.cfg.WriteTimeout = time.Hour
	w.cfg.IdleTimeout = time.Hour
	w.cfg.ReadTimeout = time.Hour

	// A fresh response's SEND: not a remainder.
	cs.writeBuf = append(cs.writeBuf[:0], "a response"...)
	if w.flushSend(cs) {
		t.Fatalf("apparatus: flushSend found no SQE")
	}
	takeSQEs(w.ring)
	if !cs.sending || cs.remainderOut || w.remainderWatch {
		t.Fatalf("a response's first SEND: sending=%v remainderOut=%v remainderWatch=%v, want true false false: "+
			"the untimed wait must stay available for the common cycle", cs.sending, cs.remainderOut, w.remainderWatch)
	}
	if !waitUntimed(true, true, false, w.remainderWatch) {
		t.Errorf("waitUntimed is false with only a first SEND in flight")
	}
	w.handleSend(sendCQE750(rig, int32(len("a response")), 0), rig.local, time.Now().UnixNano())

	// A long first SEND (an async handler's short direct write leaves the
	// rest of a large response to the ring, as a first SEND into a socket that
	// may be full) is marked too, and its completion ends the marks.
	cs.writeBuf = append(cs.writeBuf[:0], make([]byte, longSendBytes)...)
	if w.flushSend(cs) {
		t.Fatalf("apparatus: flushSend found no SQE")
	}
	takeSQEs(w.ring)
	if !cs.sending || !cs.remainderOut || !w.remainderWatch || !w.sendWatch {
		t.Fatalf("a first SEND of %d bytes: sending=%v remainderOut=%v remainderWatch=%v sendWatch=%v, want all true",
			longSendBytes, cs.sending, cs.remainderOut, w.remainderWatch, w.sendWatch)
	}
	w.handleSend(sendCQE750(rig, int32(longSendBytes), 0), rig.local, time.Now().UnixNano())
	if cs.remainderOut {
		t.Fatalf("the long SEND's completion left remainderOut set")
	}
	w.checkTimeouts()
	if w.remainderWatch || w.sendWatch {
		t.Fatalf("apparatus: the watches are still set after the long SEND completed")
	}

	// A partial completion: 8 of 16 bytes. The remainder goes out as a SEND.
	const first = "0123456789abcdef"
	inFlight750(rig, first, false)
	w.sendWatch = false
	w.handleSend(sendCQE750(rig, 8, 0), rig.local, time.Now().UnixNano())
	if sqes := takeSQEs(w.ring); len(sqes) != 1 {
		t.Fatalf("apparatus: %d SQEs after a partial completion, want the remainder's SEND", len(sqes))
	}
	if !cs.sending || string(cs.sendBuf) != first[8:] {
		t.Fatalf("apparatus: sending=%v sendBuf=%q, want the remainder %q in flight", cs.sending, cs.sendBuf, first[8:])
	}
	if !cs.remainderOut || !w.remainderWatch || !w.sendWatch {
		t.Fatalf("celeris#884 item 62: the remainder SEND left remainderOut=%v remainderWatch=%v sendWatch=%v, want all true: "+
			"the worker would wait for it with no timeout and no sweep would run for WriteTimeout",
			cs.remainderOut, w.remainderWatch, w.sendWatch)
	}
	if waitUntimed(true, w.sendsPending, false, w.remainderWatch) {
		t.Errorf("waitUntimed is true while a remainder SEND is outstanding")
	}

	// The sweep keeps the marks while the remainder is outstanding.
	w.checkTimeouts()
	if !w.remainderWatch || !w.sendWatch || !cs.remainderOut {
		t.Errorf("a sweep that finds the remainder outstanding: remainderWatch=%v sendWatch=%v remainderOut=%v, want all true",
			w.remainderWatch, w.sendWatch, cs.remainderOut)
	}

	// Its completion ends them: the conn's flag at once, the worker's at the sweep.
	w.handleSend(sendCQE750(rig, int32(len(first)-8), 0), rig.local, time.Now().UnixNano())
	if cs.sending || cs.remainderOut {
		t.Fatalf("after the remainder's completion sending=%v remainderOut=%v, want both false", cs.sending, cs.remainderOut)
	}
	w.checkTimeouts()
	if w.remainderWatch || w.sendWatch {
		t.Errorf("a sweep that finds no SEND outstanding: remainderWatch=%v sendWatch=%v, want both false", w.remainderWatch, w.sendWatch)
	}

	// A flag a completion path that skips completeSend left behind must not
	// hold the untimed wait off for the conn's life.
	cs.remainderOut = true
	w.checkTimeouts()
	if cs.remainderOut || w.remainderWatch {
		t.Errorf("a sweep left remainderOut=%v remainderWatch=%v for a conn with no SEND outstanding", cs.remainderOut, w.remainderWatch)
	}
}

// TestIouringLongSendCompletionEndsRemainderWatchAtOnce884: the mode-3a ban
// (remainderWatch) lasts as long as a marked SEND is outstanding, not until
// the next sweep. A large response to a client that reads normally is marked
// (longSendBytes) and completes within a pass; if the worker stayed out of its
// untimed wait for the 1 to 2 s until the sweep re-derived the mark, a worker
// that serves one such response per sweep interval would never get mode 3a
// back and every wait would pay the ext_arg and the hrtimer.
func TestIouringLongSendCompletionEndsRemainderWatchAtOnce884(t *testing.T) {
	rig := newStallRig704(t)
	w, cs := rig.w, rig.cs
	w.cfg.WriteTimeout = time.Hour
	w.cfg.IdleTimeout = time.Hour
	w.cfg.ReadTimeout = time.Hour

	cs.writeBuf = append(cs.writeBuf[:0], make([]byte, longSendBytes)...)
	if w.flushSend(cs) {
		t.Fatalf("apparatus: flushSend found no SQE")
	}
	takeSQEs(w.ring)
	if !cs.remainderOut || !w.remainderWatch {
		t.Fatalf("apparatus: the long SEND left remainderOut=%v remainderWatch=%v, want both true", cs.remainderOut, w.remainderWatch)
	}
	w.handleSend(sendCQE750(rig, int32(longSendBytes), 0), rig.local, time.Now().UnixNano())
	if cs.remainderOut {
		t.Fatalf("the long SEND's completion left remainderOut set")
	}
	if w.remainderWatch {
		t.Errorf("the only marked SEND completed and remainderWatch is still set: the worker stays out of mode 3a until the next sweep")
	}
	if !waitUntimed(true, true, false, w.remainderWatch) {
		t.Errorf("waitUntimed is false after the only marked SEND completed")
	}
}

// TestIouringRemainderWatchOutlivesTheFirstOfTwoMarkedSends884: the worker's
// watch ends with the LAST marked SEND, not the first: one conn's long SEND
// completing must not let the worker wait with no timeout while another conn's
// remainder is still outstanding.
func TestIouringRemainderWatchOutlivesTheFirstOfTwoMarkedSends884(t *testing.T) {
	w := &Worker{}
	a, b := &connState{}, &connState{}
	w.markLongSend(a, longSendBytes)
	w.markLongSend(b, longSendBytes)
	w.markRemainder(b) // a second mark of the same SEND (a re-send) is one conn
	if w.remainderN != 2 || !w.remainderWatch {
		t.Fatalf("two conns marked: remainderN=%d remainderWatch=%v, want 2 true", w.remainderN, w.remainderWatch)
	}
	w.clearRemainder(a)
	if !w.remainderWatch || w.remainderN != 1 {
		t.Errorf("one of two marked SENDs completed: remainderN=%d remainderWatch=%v, want 1 true", w.remainderN, w.remainderWatch)
	}
	if waitUntimed(true, true, false, w.remainderWatch) {
		t.Errorf("waitUntimed is true while one conn's marked SEND is outstanding")
	}
	w.clearRemainder(a) // a conn with no mark: a no-op, never a negative count
	w.clearRemainder(b)
	if w.remainderWatch || w.remainderN != 0 {
		t.Errorf("both marked SENDs completed: remainderN=%d remainderWatch=%v, want 0 false", w.remainderN, w.remainderWatch)
	}
	w.markLongSend(a, longSendBytes-1) // below the figure: not marked
	if a.remainderOut || w.remainderN != 0 || w.remainderWatch {
		t.Errorf("a SEND of %d bytes was marked: remainderOut=%v remainderN=%d", longSendBytes-1, a.remainderOut, w.remainderN)
	}
}
