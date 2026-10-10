//go:build linux

package iouring

import (
	"runtime"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// celeris#880, review round 2: fdOps for a live conn whose SEND_ZC notification
// the SHUTDOWN DRAIN read.
//
// staleConnCQE takes a SEND_ZC off kernelInflight when it READS the
// notification, a terminal CQE. The loop's reads go on to handleSend, which
// then applies the notification (zcNotifPending) or holds it for the dispatch
// goroutine (heldSends). The shutdown drain (endOwedOpsAtShutdown) reads
// through staleConnCQE alone, so a notification it reads is neither: it has
// left kernelInflight and no flag or held entry says so. fdOps subtracted the
// zero-copy op for a first completion (applied, held, or seen by the drain)
// anyway, a second time, and a conn with a recv still owed read as owing
// nothing: the drain returned at once instead of waiting for that recv's
// terminal completion before the descriptor was closed (celeris#685). The
// drain therefore notes the notifications it reads, and its count subtracts
// the SEND_ZC only while none has been read (applied, held or read by the
// drain).
//
// The unit rig stands for the state; the other three use a real ring and a
// real SEND_ZC whose notification is left in the completion queue for the
// drain. The owed op beside the SEND_ZC is a recv count with no SQE behind it,
// so its terminal completion never comes and the drain must wait out its
// bound (shutdownFDDrainNanos) instead of returning at once.

// waitCQs880 waits until n completions sit unread in w's completion ring.
func waitCQs880(t *testing.T, w *Worker, n uint32, d time.Duration) {
	t.Helper()
	for end := time.Now().Add(d); ; {
		head, tail := w.ring.BeginCQ()
		if tail-head >= n {
			return
		}
		if time.Now().After(end) {
			t.Fatalf("only %d completions in the ring after %v, want %d", tail-head, d, n)
		}
		time.Sleep(time.Millisecond)
	}
}

// drainPeer880 reads everything the peer is sent, so a SEND_ZC completes and
// its notification comes. The returned stop ends it.
func drainPeer880(peer int) (stop func()) {
	_ = unix.SetNonblock(peer, true)
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 1<<16)
		for {
			select {
			case <-done:
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
	return func() { close(done); wg.Wait() }
}

// firstZCCQE880 places a SEND_ZC on cs, submits it, waits for its first
// completion (F_MORE) and delivers it the way the loop does. It returns
// with that completion consumed from the ring.
func firstZCCQE880(t *testing.T, w *Worker, cs *connState) {
	t.Helper()
	fd := cs.fd
	cs.writeBuf = make([]byte, zcPayload)
	if w.flushSend(cs) || !cs.sendIsZC || cs.kernelInflight != 1 {
		t.Fatalf("no SEND_ZC placed: sendIsZC=%v kernelInflight=%d", cs.sendIsZC, cs.kernelInflight)
	}
	if _, err := w.ring.Submit(); err != nil {
		t.Fatalf("submit: %v", err)
	}
	waitCQs880(t, w, 1, 2*time.Second)
	head, _ := w.ring.BeginCQ()
	c := *w.ring.cqeAt(head)
	if c.UserData&udMask != udSend || !cqeHasMore(c.Flags) {
		t.Fatalf("first completion ud=%#x flags=%#x, want the SEND_ZC's F_MORE", c.UserData, c.Flags)
	}
	deliver880(w, &stallRig704{w: w, cs: cs, local: fd}, &c)
	w.ring.EndCQ(head + 1)
}

// drainOwed880 runs the shutdown drain with one more op owed on cs (a recv
// count with no SQE: its terminal completion never comes), and returns fdOps
// before and after and how long the drain took. cs is left clean.
func drainOwed880(w *Worker, cs *connState) (before, after int32, took time.Duration) {
	cs.recvArmed = true
	cs.kernelInflight++
	before = fdOps(cs)
	start := time.Now()
	w.endOwedOpsAtShutdown()
	took = time.Since(start)
	after = fdOps(cs)
	cs.recvArmed = false
	return before, after, took
}

// TestFdOpsHeldFirstNotifReadByTheDrain: the first completion is held for the
// dispatch goroutine, then the notification is read the way the drain reads it
// (staleConnCQE alone). A recv is armed and is the only op that names the
// descriptor.
func TestFdOpsHeldFirstNotifReadByTheDrain(t *testing.T) {
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
	notif := sendCQE750(rig, 0, cqeFNotif)
	w.staleConnCQE(notif, rig.local, notif.UserData) // what endOwedOpsAtShutdown does
	got := fdOpsSeen(cs, false, true)
	t.Logf("celeris880 DRAINNOTIF held=%d kernelInflight=%d zcNotifPending=%v recvArmed=%v fdOpsSeen=%d",
		len(cs.heldSends), cs.kernelInflight, cs.zcNotifPending, cs.recvArmed, got)
	if len(cs.heldSends) != 1 || cs.kernelInflight != 1 || cs.zcNotifPending {
		t.Fatalf("the state did not form: held=%d kernelInflight=%d zcNotifPending=%v (want 1, 1, false)",
			len(cs.heldSends), cs.kernelInflight, cs.zcNotifPending)
	}
	if got != 1 {
		t.Errorf("fdOps with the drain's read notification = %d with the recv armed and the SEND_ZC fully ended, want 1", got)
	}
}

// TestShutdownDrainWaitsForARecvBesideAHeldFirstCompletionItReadTheNotifOf:
// the first completion is held, the notification is left in the completion
// queue for the drain, and another op is owed on the descriptor. The drain
// must keep waiting for it to its bound.
func TestShutdownDrainWaitsForARecvBesideAHeldFirstCompletionItReadTheNotifOf(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	w, cs, peer := newZCCloseWorker(t)
	release := asyncGoroutine880(t, cs)
	defer release()
	firstZCCQE880(t, w, cs)
	if len(cs.heldSends) != 1 {
		t.Fatalf("the first completion was not held (%d)", len(cs.heldSends))
	}
	stop := drainPeer880(peer)
	waitCQs880(t, w, 1, 5*time.Second)
	stop()
	if h, _ := w.ring.BeginCQ(); w.ring.cqeAt(h).UserData&udMask != udSend || w.ring.cqeAt(h).Flags&cqeFNotif == 0 {
		t.Fatalf("the completion left for the drain is not the notification")
	}
	before, after, took := drainOwed880(w, cs)
	t.Logf("celeris880 DRAINHELD before=%d after=%d kernelInflight=%d held=%d took=%v",
		before, after, cs.kernelInflight, len(cs.heldSends), took.Round(time.Millisecond))
	if cs.kernelInflight != 1 {
		t.Fatalf("the state did not form: kernelInflight=%d after the drain, want 1 (the owed op)", cs.kernelInflight)
	}
	// before is the count with the held first completion left out once
	// (kernelInflight 2: the SEND_ZC and the owed op). after is plain fdOps once
	// the drain is done; it is not the signal, since only the drain knows it
	// read the notification (fdOps).
	if before != 1 || took < 200*time.Millisecond {
		t.Errorf("the drain stopped waiting with an op owed on the descriptor: fdOps before=%d (want 1), took %v (want the %v bound)",
			before, took, time.Duration(shutdownFDDrainNanos))
	}
	cs.kernelInflight = 0
}

// TestShutdownDrainWaitsAfterReadingBothSendZCCompletions: neither SEND_ZC
// completion was read before the shutdown, so the drain reads both, the first
// (F_MORE) and then the notification, which takes the op off kernelInflight.
func TestShutdownDrainWaitsAfterReadingBothSendZCCompletions(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	w, cs, peer := newZCCloseWorker(t)
	cs.writeBuf = make([]byte, zcPayload)
	if w.flushSend(cs) || !cs.sendIsZC || cs.kernelInflight != 1 {
		t.Fatalf("no SEND_ZC placed: sendIsZC=%v kernelInflight=%d", cs.sendIsZC, cs.kernelInflight)
	}
	if _, err := w.ring.Submit(); err != nil {
		t.Fatalf("submit: %v", err)
	}
	stop := drainPeer880(peer)
	waitCQs880(t, w, 2, 5*time.Second)
	stop()
	before, after, took := drainOwed880(w, cs)
	t.Logf("celeris880 DRAINBOTH before=%d after=%d kernelInflight=%d took=%v",
		before, after, cs.kernelInflight, took.Round(time.Millisecond))
	if cs.kernelInflight != 1 {
		t.Fatalf("the state did not form: kernelInflight=%d after the drain, want 1", cs.kernelInflight)
	}
	if took < 200*time.Millisecond {
		t.Errorf("the drain stopped waiting with an op owed on the descriptor after reading both SEND_ZC completions: took %v (want the %v bound)",
			took, time.Duration(shutdownFDDrainNanos))
	}
	cs.kernelInflight = 0
}

// TestShutdownDrainWaitsAfterReadingTheNotifOfAnAppliedFirstCompletion: the
// first completion was applied before the shutdown (zcNotifPending set), the
// notification is read by the drain, which takes the op off kernelInflight
// while zcNotifPending stays set. The shape of a keep-alive conn with a recv
// armed beside a SEND_ZC notification still owed.
func TestShutdownDrainWaitsAfterReadingTheNotifOfAnAppliedFirstCompletion(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	w, cs, peer := newZCCloseWorker(t)
	firstZCCQE880(t, w, cs)
	if !cs.zcNotifPending {
		t.Fatalf("the first completion was not applied (zcNotifPending=false)")
	}
	stop := drainPeer880(peer)
	waitCQs880(t, w, 1, 5*time.Second)
	stop()
	before, after, took := drainOwed880(w, cs)
	t.Logf("celeris880 DRAINAPPLIED before=%d after=%d kernelInflight=%d zcNotifPending=%v took=%v",
		before, after, cs.kernelInflight, cs.zcNotifPending, took.Round(time.Millisecond))
	if cs.kernelInflight != 1 {
		t.Fatalf("the state did not form: kernelInflight=%d after the drain, want 1", cs.kernelInflight)
	}
	if took < 200*time.Millisecond {
		t.Errorf("the drain stopped waiting with an op owed on the descriptor: took %v (want the %v bound)",
			took, time.Duration(shutdownFDDrainNanos))
	}
	cs.kernelInflight = 0
}
