//go:build linux

package iouring

import (
	"runtime"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// celeris#880, review round 1: fdOps with a SEND_ZC's completions held for the
// dispatch goroutine (celeris#750).
//
// staleConnCQE takes a terminal completion off kernelInflight when it READS it,
// held or not, and a SEND_ZC's notification (IORING_CQE_F_NOTIF) is terminal.
// So once the notification has been read, kernelInflight no longer counts the
// SEND_ZC at all, and fdOps must not subtract the zero-copy op a second time:
// the second subtraction makes a recv that is still armed vanish from the
// count, and a conn with an armed recv reads as owing nothing (fdOwed false).
// The one caller that reads fdOps on a live conn with held completions is
// worker shutdown's drain (endOwedOpsAtShutdown), which would then neither
// cancel nor wait for that recv before it closes the descriptor (celeris#685).
//
// Two states form:
//
//   - both: the first completion (F_MORE) and the notification are both read
//     while the handler runs, so both are held; zcNotifPending is still false.
//   - after: the first completion was applied (zcNotifPending), then the
//     notification is read while a handler runs, so it is held.

// deliver880 feeds one completion to the worker the way the loop does.
func deliver880(w *Worker, rig *stallRig704, c *completionEntry) {
	if !w.staleConnCQE(c, rig.local, c.UserData) {
		w.handleSend(c, rig.local, time.Now().UnixNano())
	}
}

// TestFdOpsBothSendZCCompletionsHeld: the first completion and the
// notification are both held, a recv is armed. The recv is the only op that
// names the descriptor.
func TestFdOpsBothSendZCCompletionsHeld(t *testing.T) {
	rig := newStallRig704(t)
	w, cs := rig.w, rig.cs
	w.handoffLoss = &handoffLossStats{}
	w.recvArm = &recvArmStats{}
	inFlight750(rig, "tail", true)
	cs.recvArmed = true
	cs.kernelInflight = 2 // the armed recv and the SEND_ZC
	release := holdAsHandler704(t, cs, true)
	defer release()
	deliver880(w, rig, sendCQE750(rig, 4, cqeFMore))
	deliver880(w, rig, sendCQE750(rig, 0, cqeFNotif))
	got := fdOps(cs)
	t.Logf("celeris880 BOTH held=%d kernelInflight=%d zcNotifPending=%v fdOps=%d fdOwed=%v",
		len(cs.heldSends), cs.kernelInflight, cs.zcNotifPending, got, fdOwed(cs))
	if len(cs.heldSends) != 2 || cs.kernelInflight != 1 {
		t.Fatalf("the state did not form: held=%d kernelInflight=%d (want 2 and 1)", len(cs.heldSends), cs.kernelInflight)
	}
	if got != 1 || !fdOwed(cs) {
		t.Errorf("fdOps = %d, fdOwed = %v with the recv still armed, want 1 and true", got, fdOwed(cs))
	}
}

// TestFdOpsSendZCNotificationHeldAfterFirstApplied: the first completion is
// applied, the notification is held, a recv is armed.
func TestFdOpsSendZCNotificationHeldAfterFirstApplied(t *testing.T) {
	rig := newStallRig704(t)
	w, cs := rig.w, rig.cs
	w.handoffLoss = &handoffLossStats{}
	w.recvArm = &recvArmStats{}
	inFlight750(rig, "tail", true)
	cs.recvArmed = true
	cs.kernelInflight = 2
	deliver880(w, rig, sendCQE750(rig, 4, cqeFMore))
	release := holdAsHandler704(t, cs, true)
	defer release()
	deliver880(w, rig, sendCQE750(rig, 0, cqeFNotif))
	got := fdOps(cs)
	t.Logf("celeris880 AFTER held=%d kernelInflight=%d zcNotifPending=%v fdOps=%d fdOwed=%v",
		len(cs.heldSends), cs.kernelInflight, cs.zcNotifPending, got, fdOwed(cs))
	if len(cs.heldSends) != 1 || cs.kernelInflight != 1 || !cs.zcNotifPending {
		t.Fatalf("the state did not form: held=%d kernelInflight=%d zcNotifPending=%v (want 1, 1, true)",
			len(cs.heldSends), cs.kernelInflight, cs.zcNotifPending)
	}
	if got != 1 || !fdOwed(cs) {
		t.Errorf("fdOps = %d, fdOwed = %v with the recv still armed, want 1 and true", got, fdOwed(cs))
	}
}

// TestShutdownDrainEndsARecvOwedBesideHeldSendZCCompletions is the consequence
// at the shutdown drain, with a real recv in the ring: the drain must count the
// conn as owing it, cancel it and wait for its terminal completion before the
// descriptor is closed (celeris#685).
func TestShutdownDrainEndsARecvOwedBesideHeldSendZCCompletions(t *testing.T) {
	rig := newStallRig704(t)
	w, cs := rig.w, rig.cs
	w.handoffLoss = &handoffLossStats{}
	w.recvArm = &recvArmStats{}
	if !w.prepareRecv(cs, cs.buf) {
		t.Fatal("prepareRecv failed")
	}
	if _, err := w.ring.Submit(); err != nil {
		t.Fatalf("submit: %v", err)
	}
	inFlight750(rig, "tail", true)
	cs.kernelInflight++ // the SEND_ZC, fed below
	release := holdAsHandler704(t, cs, true)
	defer release()
	deliver880(w, rig, sendCQE750(rig, 4, cqeFMore))
	deliver880(w, rig, sendCQE750(rig, 0, cqeFNotif))
	before := cs.kernelInflight
	w.endOwedOpsAtShutdown()
	t.Logf("celeris880 SHUTDOWNRECV held=%d kernelInflight_before=%d after=%d recvArmed_after=%v",
		len(cs.heldSends), before, cs.kernelInflight, cs.recvArmed)
	if before != 1 {
		t.Fatalf("the state did not form: kernelInflight = %d before the drain, want 1 (the recv)", before)
	}
	if cs.recvArmed || cs.kernelInflight != 0 {
		t.Errorf("the shutdown drain left the conn's armed recv owed (recvArmed=%v kernelInflight=%d): fdOps did not count it",
			cs.recvArmed, cs.kernelInflight)
	}
}

// TestFdOpsBothSendZCCompletionsHeldByTheKernel is the "both" state formed by a
// real kernel: a SEND_ZC to a peer that reads everything, its two completions
// read while the handler runs. A recv owed beside them is the only op that
// names the descriptor.
func TestFdOpsBothSendZCCompletionsHeldByTheKernel(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	w, cs, peer := newZCCloseWorker(t)
	fd := cs.fd
	release := asyncGoroutine880(t, cs)
	defer release()

	_ = unix.SetNonblock(peer, true)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		buf := make([]byte, 1<<16)
		for {
			select {
			case <-stop:
				return
			default:
			}
			n, err := unix.Read(peer, buf)
			if err == unix.EAGAIN || err == unix.EINTR {
				time.Sleep(time.Millisecond)
				continue
			}
			if err != nil || n == 0 {
				return
			}
		}
	}()

	cs.writeBuf = make([]byte, zcPayload)
	if w.flushSend(cs) || !cs.sendIsZC || cs.kernelInflight != 1 {
		t.Fatalf("no SEND_ZC placed: sendIsZC=%v kernelInflight=%d", cs.sendIsZC, cs.kernelInflight)
	}
	if _, err := w.ring.Submit(); err != nil {
		t.Fatalf("submit: %v", err)
	}
	for sawNotif, end := false, time.Now().Add(5*time.Second); !sawNotif; {
		head, tail := w.ring.BeginCQ()
		for ; head != tail; head++ {
			c := *w.ring.cqeAt(head)
			if c.UserData&udMask == udSend && c.Flags&cqeFNotif != 0 {
				sawNotif = true
			}
			if !w.staleConnCQE(&c, fd, c.UserData) {
				w.handleSend(&c, fd, time.Now().UnixNano())
			}
		}
		w.ring.EndCQ(head)
		if time.Now().After(end) {
			t.Fatal("no SEND_ZC notification within 5s")
		}
		if !sawNotif {
			_ = w.ring.SubmitAndWaitTimeout(10 * time.Millisecond)
		}
	}
	t.Logf("celeris880 KERNELBOTH held=%d kernelInflight=%d zcNotifPending=%v sending=%v fdOps=%d",
		len(cs.heldSends), cs.kernelInflight, cs.zcNotifPending, cs.sending, fdOps(cs))
	if len(cs.heldSends) != 2 || cs.kernelInflight != 0 || cs.zcNotifPending {
		t.Fatalf("the state did not form: held=%d kernelInflight=%d zcNotifPending=%v (want 2, 0, false)",
			len(cs.heldSends), cs.kernelInflight, cs.zcNotifPending)
	}
	if got := fdOps(cs); got != 0 {
		t.Errorf("both completions held, nothing else owed: fdOps = %d, want 0", got)
	}
	cs.kernelInflight, cs.recvArmed = 1, true // a recv owed too
	if got := fdOps(cs); got != 1 {
		t.Errorf("both completions held, a recv owed: fdOps = %d, want 1", got)
	}
	cs.kernelInflight, cs.recvArmed = 0, false
}

// TestClosingSweepDoesNotCutOffASendTheReplayMoved: the sweep replays a held
// completion before it reads the drain bound again (celeris#761's rule): a
// partial send that completed means the peer is reading, and the replay both
// restamps lastActivity and places the rest of the response. Tearing the conn
// down anyway cancels a send that is making progress.
func TestClosingSweepDoesNotCutOffASendTheReplayMoved(t *testing.T) {
	rig := newStallRig704(t)
	w, cs := rig.w, rig.cs
	w.handoffLoss = &handoffLossStats{}
	w.recvArm = &recvArmStats{}
	const tail = "the rest of response 1, longer than what the completion says was sent"
	inFlight750(rig, tail, false)
	cs.kernelInflight = 1
	release := holdAsHandler704(t, cs, true)
	deliver880(w, rig, sendCQE750(rig, 10, 0))
	if len(cs.heldSends) != 1 {
		t.Fatalf("the completion was not held (%d)", len(cs.heldSends))
	}
	release()
	cs.asyncInMu.Lock()
	cs.asyncRun = false
	cs.asyncParked = false
	cs.asyncInMu.Unlock()
	markClosing880(w, cs)
	w.checkTimeouts()
	t.Logf("celeris880 BOUND slot_owned=%v sending=%v held=%d idle=%v bound=%v",
		w.conns[rig.local] == cs, cs.sending, len(cs.heldSends),
		time.Duration(time.Now().UnixNano()-cs.lastActivity).Round(time.Millisecond), w.closingDrainBound())
	if w.conns[rig.local] != cs {
		t.Errorf("the sweep tore a conn down whose replayed completion had just moved bytes (the peer is reading)")
	}
	if !cs.sending {
		t.Errorf("the rest of the response was not placed after the replayed partial send")
	}
}
