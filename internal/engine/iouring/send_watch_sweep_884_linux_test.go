//go:build linux

package iouring

import (
	"testing"
	"time"
)

// TestIouringSweepKeepsAMarkItsOwnReplayMade884: the timeout sweep re-derives
// remainderN, remainderWatch and sendWatch from the conns it visits and stores
// them at its end. A mark made DURING the sweep, after it had looked at that
// conn, was overwritten: the closing branch's replayHeldSends applies a held
// partial completion, completeSend re-sends the remainder (flushSend,
// markRemainder), and the sweep's own count had the conn as a conn with no
// remainder. The run loop then went back to its untimed wait (mode 3a) with a
// remainder outstanding to a peer that may not take it (celeris#884 item 62).
func TestIouringSweepKeepsAMarkItsOwnReplayMade884(t *testing.T) {
	rig := newStallRig704(t)
	w, cs := rig.w, rig.cs
	w.handoffLoss = &handoffLossStats{}
	w.recvArm = &recvArmStats{}
	w.cfg.WriteTimeout = time.Hour
	w.cfg.IdleTimeout = time.Hour
	w.cfg.ReadTimeout = time.Hour

	// A SEND of 16 bytes in flight, whose completion (8 bytes taken) arrives
	// while the conn's handler runs and is held for it.
	const first = "0123456789abcdef"
	inFlight750(rig, first, false)
	cs.kernelInflight = 1
	release := asyncGoroutine880(t, cs)
	cqe := sendCQE750(rig, 8, 0)
	if !w.staleConnCQE(cqe, rig.local, cqe.UserData) {
		w.handleSend(cqe, rig.local, time.Now().UnixNano())
	}
	if len(cs.heldSends) != 1 {
		t.Fatalf("apparatus: the completion was not held (%d)", len(cs.heldSends))
	}
	if cs.remainderOut || w.remainderWatch || w.sendWatch {
		t.Fatalf("apparatus: marks set before the sweep: remainderOut=%v remainderWatch=%v sendWatch=%v",
			cs.remainderOut, w.remainderWatch, w.sendWatch)
	}
	release()
	markClosing880(w, cs) // a close deferred behind the send, long ago

	w.checkTimeouts() // the replay re-sends the remainder: a mark made mid-sweep
	if len(cs.heldSends) != 0 || !cs.sending || string(cs.sendBuf) != first[8:] {
		t.Fatalf("apparatus: the replay did not put the remainder in flight: held=%d sending=%v sendBuf=%q",
			len(cs.heldSends), cs.sending, cs.sendBuf)
	}
	if !cs.remainderOut {
		t.Fatalf("apparatus: the remainder SEND was not marked")
	}
	if !w.remainderWatch || !w.sendWatch || w.remainderN != 1 {
		t.Errorf("celeris#884 item 62: the sweep overwrote the mark its own replay made: remainderWatch=%v sendWatch=%v remainderN=%d, want true true 1: "+
			"the worker would wait for the remainder with no timeout", w.remainderWatch, w.sendWatch, w.remainderN)
	}
	if waitUntimed(true, w.sendsPending, false, w.remainderWatch) {
		t.Errorf("waitUntimed is true while a remainder SEND is outstanding")
	}

	// The next sweep counts it from the conn; its completion ends the marks.
	w.checkTimeouts()
	if !w.remainderWatch || w.remainderN != 1 {
		t.Errorf("the next sweep: remainderWatch=%v remainderN=%d, want true 1", w.remainderWatch, w.remainderN)
	}
}

// TestIouringSweepKeepsAMarkItsDeferredCloseMade884: the same overwrite by the
// other path. A conn with no SEND outstanding when the sweep looks at it, and
// a response of longSendBytes or more queued, is closed by the sweep (idle
// timeout); closeConn defers the close behind a flush of what is queued
// (flushSend), a long SEND the peer may not take, and the sweep's end stored
// sendWatch = false (no SEND was outstanding when it looked) over the mark.
func TestIouringSweepKeepsAMarkItsDeferredCloseMade884(t *testing.T) {
	rig := newStallRig704(t)
	w, cs := rig.w, rig.cs
	w.handoffLoss = &handoffLossStats{}
	w.recvArm = &recvArmStats{}
	w.cfg.WriteTimeout = time.Hour
	w.cfg.ReadTimeout = time.Hour
	w.cfg.IdleTimeout = time.Millisecond
	cs.writeBuf = append(cs.writeBuf[:0], make([]byte, longSendBytes)...)
	cs.lastActivity = time.Now().Add(-time.Minute).UnixNano()
	if cs.sending || cs.remainderOut || w.remainderWatch || w.sendWatch {
		t.Fatalf("apparatus: a SEND or a mark before the sweep")
	}

	w.checkTimeouts()
	if !cs.closing || !cs.sending || !cs.remainderOut {
		t.Fatalf("apparatus: the sweep did not defer the close behind a long SEND: closing=%v sending=%v remainderOut=%v",
			cs.closing, cs.sending, cs.remainderOut)
	}
	if !w.remainderWatch || !w.sendWatch || w.remainderN != 1 {
		t.Errorf("celeris#884 item 62: the sweep overwrote the mark its own close made: remainderWatch=%v sendWatch=%v remainderN=%d, want true true 1",
			w.remainderWatch, w.sendWatch, w.remainderN)
	}
}
