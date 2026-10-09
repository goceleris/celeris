//go:build linux

package iouring

import (
	"runtime"
	"sync"
	"testing"
	"time"
)

// celeris#880 (from celeris#845, item outside-813, and #845 item 3): a send
// completion the worker holds for the async dispatch goroutine (celeris#750)
// has been read and has not been applied, so the state the close paths read
// to decide what the kernel still owes is behind the kernel's:
//
//   - fdOps counted a held SEND_ZC first completion as an op that names the
//     descriptor. The send is done. The close kept the socket open for it,
//     and, with the peer stalled and the notification never coming, the 5 s
//     backstop closed the descriptor and counted CloseFDForced (must stay 0).
//   - the closing-drain sweep in checkTimeouts tore a conn down without
//     applying a held notification. The send was still taken for owed
//     (zcSendOwed), and with a recv owed too the connState was taken for one
//     that owes nothing else, and released.
//
// The rigs build the state a late hand-back leaves: the conn is closing (its
// close deferred behind the send), the completion is held, and the sweep runs
// before the hand-back's drain. The first rig is a real kernel, a real
// SEND_ZC to a peer that stopped reading.

// asyncGoroutine880 makes cs an async conn whose dispatch goroutine is inside
// a handler holding detachMu: the state in which handleSend holds a
// completion. It returns the release, which also marks the goroutine gone.
func asyncGoroutine880(t *testing.T, cs *connState) (release func()) {
	t.Helper()
	if cs.detachMu == nil {
		cs.detachMu = &sync.Mutex{}
	}
	cs.asyncCond.L = &cs.asyncInMu
	rel := holdAsHandler704(t, cs, true)
	return func() {
		rel()
		cs.asyncInMu.Lock()
		cs.asyncRun = false
		cs.asyncParked = false
		cs.asyncInMu.Unlock()
	}
}

// markClosing880 puts cs in the state closeConn leaves a conn whose close it
// deferred behind a send in flight, a long time ago.
func markClosing880(w *Worker, cs *connState) {
	cs.closing = true
	cs.asyncClosed.Store(true)
	cs.detachClosed = true
	cs.lastActivity = time.Now().UnixNano() - 2*w.closingDrainBound()
}

// holdZCFirstCompletion880 places a SEND_ZC on cs, a conn whose dispatch
// goroutine is inside a handler, to a peer that reads nothing, and delivers its
// first completion the way the loop does (staleConnCQE, then handleSend), which
// holds it. The notification is still owed afterwards.
func holdZCFirstCompletion880(t *testing.T, w *Worker, cs *connState) {
	t.Helper()
	fd := cs.fd
	payload := make([]byte, zcPayload)
	cs.writeBuf = payload
	if w.flushSend(cs) || !cs.sendIsZC || cs.kernelInflight != 1 {
		t.Fatalf("no SEND_ZC placed: sendIsZC=%v kernelInflight=%d", cs.sendIsZC, cs.kernelInflight)
	}
	if _, err := w.ring.Submit(); err != nil {
		t.Fatalf("submit: %v", err)
	}
	var c *completionEntry
	for end := time.Now().Add(2 * time.Second); c == nil; {
		if head, tail := w.ring.BeginCQ(); head != tail {
			c = w.ring.cqeAt(head)
			break
		}
		if time.Now().After(end) {
			t.Fatal("no completion for the SEND_ZC within 2s")
		}
		time.Sleep(time.Millisecond)
	}
	if c.UserData&udMask != udSend || !cqeHasMore(c.Flags) || c.Res <= 0 {
		t.Fatalf("first completion ud=%#x flags=%#x res=%d, want the SEND_ZC's result (F_MORE, res > 0)", c.UserData, c.Flags, c.Res)
	}
	head, _ := w.ring.BeginCQ()
	held := *c
	if !w.staleConnCQE(&held, fd, held.UserData) {
		w.handleSend(&held, fd, time.Now().UnixNano())
	}
	w.ring.EndCQ(head + 1)
	if len(cs.heldSends) != 1 || cs.zcNotifPending || !cs.sending || cs.kernelInflight != 1 {
		t.Fatalf("the first completion was not held: held=%d zcNotifPending=%v sending=%v kernelInflight=%d",
			len(cs.heldSends), cs.zcNotifPending, cs.sending, cs.kernelInflight)
	}
	// The peer reads nothing, so the notification stays owed.
	time.Sleep(100 * time.Millisecond)
	if hd, tl := w.ring.BeginCQ(); tl != hd {
		t.Fatalf("%d completions in the ring 100 ms after the send: the notification arrived, so the case did not form", tl-hd)
	}
}

// TestHeldSendZCFirstCompletionDoesNotKeepTheDescriptor is the first bullet,
// with the kernel: the SEND_ZC's first completion is read while a handler
// runs, and held; the conn is closed by the sweep. The descriptor must be
// closed at once, not at the backstop.
func TestHeldSendZCFirstCompletionDoesNotKeepTheDescriptor(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	w, cs, _ := newZCCloseWorker(t)
	fd := cs.fd
	target := fdTarget(fd)
	release := asyncGoroutine880(t, cs)
	holdZCFirstCompletion880(t, w, cs)

	// The handler returned; the close was deferred behind the send; the
	// hand-back has not been drained yet. The sweep runs.
	release()
	markClosing880(w, cs)
	w.checkTimeouts()

	closedAtSweep := fdTarget(fd) != target
	forcedBefore := w.handoffLoss.closeFDForced.Load()
	// And what the backstop does if the descriptor was kept.
	for i := range w.pendingRelease {
		w.pendingRelease[i].releaseAtNanos = 1
	}
	w.cachedNow = time.Now().UnixNano()
	w.drainPendingRelease()
	forced := w.handoffLoss.closeFDForced.Load()
	t.Logf("celeris880 FD held=%d closed_at_sweep=%v closeFDOwed=%d CloseFDForced_after_backstop=%d (before %d)",
		len(cs.heldSends), closedAtSweep, w.closeFDOwed, forced, forcedBefore)
	if !closedAtSweep {
		t.Errorf("fd %d was still open after the sweep tore the conn down: the held SEND_ZC first completion, "+
			"an op that has ended, was counted as one that names the descriptor", fd)
	}
	if forced != 0 {
		t.Errorf("CloseFDForced = %d after the backstop, want 0", forced)
	}
	if w.closeFDOwed != 0 {
		t.Errorf("closeFDOwed = %d, want 0", w.closeFDOwed)
	}
}

// TestFdOpsLeavesOutAHeldSendZCFirstCompletion pins fdOps itself, for the
// callers that are not the sweep (a close that finds a completion held again,
// worker shutdown's drain): a held first completion is not an op on the
// descriptor, a held notification is not counted twice, and a plain held
// completion is.
func TestFdOpsLeavesOutAHeldSendZCFirstCompletion(t *testing.T) {
	first := completionEntry{Flags: cqeFMore}
	notif := completionEntry{Flags: cqeFNotif}
	plain := completionEntry{}
	for _, tc := range []struct {
		name       string
		inflight   int32
		zcNotif    bool
		held       []completionEntry
		wantFDOps  int32
		wantReason string
	}{
		{"nothing held", 2, false, nil, 2, "both ops name the descriptor"},
		{"notification pending", 2, true, nil, 1, "celeris#798"},
		{"first completion held", 2, false, []completionEntry{first}, 1, "celeris#880"},
		{"first held, nothing else owed", 1, false, []completionEntry{first}, 0, "celeris#880"},
		{"first applied, notification held", 2, true, []completionEntry{notif}, 1, "the held notification is terminal: kernelInflight has lost it already"},
		{"plain completion held", 1, false, []completionEntry{plain}, 1, "a plain send's completion is its terminal one"},
	} {
		cs := &connState{kernelInflight: tc.inflight, zcNotifPending: tc.zcNotif, heldSends: tc.held}
		if got := fdOps(cs); got != tc.wantFDOps {
			t.Errorf("%s: fdOps = %d, want %d (%s)", tc.name, got, tc.wantFDOps, tc.wantReason)
		}
	}
}

// TestClosingSweepAppliesAHeldSendNotification is the second bullet (#845
// item 3): a conn closing with a SEND_ZC's notification held and a recv owed
// too. The sweep must apply the notification before it reads what is owed. Not
// applied, the send is still taken for a SEND_ZC owed, the identity for one
// that owes nothing else, and the backstop starts a hold, CloseZCNotifHeld,
// for a notification that has already arrived.
func TestClosingSweepAppliesAHeldSendNotification(t *testing.T) {
	rig := newStallRig704(t)
	w, cs := rig.w, rig.cs
	w.handoffLoss = &handoffLossStats{}
	w.recvArm = &recvArmStats{}

	const sent = "the rest of response 1"
	// A SEND_ZC whose first completion has been applied, and a recv armed
	// behind it: the keep-alive shape.
	cs.sendBuf = append(cs.sendBuf[:0], sent...)
	cs.sending, cs.sendIsZC = true, true
	cs.zcNotifPending, cs.zcSentBytes = true, int32(len(sent))
	cs.recvArmed = true
	cs.kernelInflight = 2

	release := asyncGoroutine880(t, cs)
	notif := sendCQE750(rig, 0, cqeFNotif)
	if !w.staleConnCQE(notif, rig.local, notif.UserData) {
		w.handleSend(notif, rig.local, time.Now().UnixNano())
	}
	if len(cs.heldSends) != 1 || !cs.zcNotifPending || cs.kernelInflight != 1 {
		t.Fatalf("the notification was not held: held=%d zcNotifPending=%v kernelInflight=%d (want 1 true 1)",
			len(cs.heldSends), cs.zcNotifPending, cs.kernelInflight)
	}
	release()
	markClosing880(w, cs)
	w.checkTimeouts()
	for i := range w.pendingRelease {
		w.pendingRelease[i].releaseAtNanos = 1
	}
	w.cachedNow = time.Now().UnixNano()
	w.drainPendingRelease()

	held := w.handoffLoss.zcNotifHeld.Load()
	t.Logf("celeris880 NOTIF held_left=%d slot_owned=%v CloseZCNotifHeld=%d zcHolds=%d", len(cs.heldSends), w.conns[rig.local] == cs, held, len(w.zcHolds))
	if w.conns[rig.local] != nil {
		t.Errorf("the sweep did not finish the close")
	}
	if held != 0 || len(w.zcHolds) != 0 {
		t.Errorf("CloseZCNotifHeld = %d (holds %d) for a conn whose notification had arrived: the sweep tore it down "+
			"without applying the held notification", held, len(w.zcHolds))
	}
}

// TestClosingSweepDoesNotWaitForARunningAsyncHandler is the deadlock check of
// the sweep's replay (it runs handleSend, which takes detachMu): with the
// dispatch goroutine inside a handler, the sweep must return at once, leave
// the conn whole with its completion held, and finish the close once the
// goroutine has handed the conn back.
func TestClosingSweepDoesNotWaitForARunningAsyncHandler(t *testing.T) {
	rig := newStallRig704(t)
	w, cs := rig.w, rig.cs
	w.handoffLoss = &handoffLossStats{}
	w.recvArm = &recvArmStats{}
	const tail = "the rest of response 1"
	inFlight750(rig, tail, true)
	cs.kernelInflight = 1

	release := holdAsHandler704(t, cs, true)
	first := sendCQE750(rig, int32(len(tail)), cqeFMore)
	if !w.staleConnCQE(first, rig.local, first.UserData) {
		w.handleSend(first, rig.local, time.Now().UnixNano())
	}
	if len(cs.heldSends) != 1 {
		t.Fatalf("the completion was not held (%d)", len(cs.heldSends))
	}
	markClosing880(w, cs)
	if !returnsWhileHeld704(t, release, w.checkTimeouts) {
		t.Fatalf("celeris#880: the closing sweep waited %v on a running handler's detachMu", stallWait704)
	}
	if w.conns[rig.local] != cs || len(cs.heldSends) != 1 {
		t.Errorf("a sweep that found the completion held again must leave the conn for the hand-back: slot_owned=%v held=%d",
			w.conns[rig.local] == cs, len(cs.heldSends))
	}
	release()
	cs.asyncInMu.Lock()
	cs.asyncRun = false
	cs.asyncParked = false
	cs.asyncInMu.Unlock()
	// The hand-back is drained, then the sweep runs again.
	w.replayHeldSends(cs)
	if len(cs.heldSends) != 0 || !cs.zcNotifPending {
		t.Fatalf("after the hand-back: held=%d zcNotifPending=%v, want 0 true", len(cs.heldSends), cs.zcNotifPending)
	}
	w.checkTimeouts()
	rig.expectClosedOnce(t)
}

// TestShutdownDoesNotWaitForAHeldSendZCFirstCompletion is the same count at
// worker shutdown (endOwedOpsAtShutdown, celeris#798's other site): the drain
// waits for the ops that name a live connection's descriptor, and a held
// first completion names none. Counted as one, it ran the drain's whole 250 ms
// bound for a notification a stalled peer holds back.
func TestShutdownDoesNotWaitForAHeldSendZCFirstCompletion(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	w, cs, _ := newZCCloseWorker(t)
	release := asyncGoroutine880(t, cs)
	holdZCFirstCompletion880(t, w, cs)
	defer release()
	start := time.Now()
	w.endOwedOpsAtShutdown()
	took := time.Since(start)
	t.Logf("celeris880 SHUTDOWN held=%d took=%v closeFDOwed=%d", len(cs.heldSends), took.Round(time.Microsecond), w.closeFDOwed)
	if took > zcShutdownBound {
		t.Fatalf("endOwedOpsAtShutdown took %v (bound %v) with only a held SEND_ZC first completion owed", took, zcShutdownBound)
	}
}
