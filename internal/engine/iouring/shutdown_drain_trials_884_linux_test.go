//go:build linux

package iouring

import (
	"context"
	"testing"
	"time"
)

// celeris#884 item 16, trials (a) and (b). The shutdown drain
// (endOwedOpsAtShutdown) ends the ops the kernel still owes on a live
// connection's descriptor before shutdown closes it. TestShutdownDrainSurvivesAFullCQRing873
// and the 880 tests pin it for a standalone recv; PR #1099 recorded two forms
// they do not reach, and its open question, keep or remove the drain, is
// answered by what these show: the drain is the only thing that ends them.
//
//   - (a) a recv linked behind a SEND whose CQE the loop has already read. The
//     kernel consumed both SQEs at the submit; the recv is issued, and its
//     descriptor number resolved, only when the SEND completes, so it can be
//     in no cancel table at the moment the descriptor is closed.
//   - (b) a multishot recv the loop re-armed after the kernel ended it with
//     ENOBUFS (the provided-buffer ring ran dry): the new SQE is in the SQ ring,
//     not yet submitted, and counts as owed from the moment it is placed.
//
// Each holds the worker at that point (the test is the loop) and calls the
// drain. With the drain disabled (mutant M1: `if false && w.ring != nil`) both
// leave the recv owed, kernelInflight 1, which is what shutdown would close
// the descriptor under. Mutant logs: the lane's mutants/ directory.

// reapReal884 dispatches the CQEs in the ring the way the loop does, until stop
// returns true for one, and returns whether it did.
func reapReal884(t *testing.T, w *Worker, stop func(c *completionEntry) bool) bool {
	t.Helper()
	for end := time.Now().Add(2 * time.Second); time.Now().Before(end); time.Sleep(time.Millisecond) {
		head, tail := w.ring.BeginCQ()
		found := false
		for ; head != tail; head++ {
			c := w.ring.cqeAt(head)
			w.processCQE(context.Background(), c, time.Now().UnixNano())
			if stop(c) {
				found = true
			}
		}
		w.ring.EndCQ(head)
		if found {
			return true
		}
	}
	return false
}

func drainEnded884(t *testing.T, f *fdlFixture) {
	t.Helper()
	w, cs := f.w, f.cs
	start := time.Now()
	w.endOwedOpsAtShutdown()
	took := time.Since(start)
	t.Logf("celeris884 drain: took=%v kernelInflight_after=%d gaveUp=%d", took.Round(time.Microsecond), cs.kernelInflight, w.handoffLoss.shutdownDrainGaveUp.Load())
	if cs.kernelInflight != 0 {
		t.Fatalf("the drain ended with %d op(s) still owed on fd %d: shutdown would close the descriptor under them", cs.kernelInflight, f.fd)
	}
	if g := w.handoffLoss.shutdownDrainGaveUp.Load(); g != 0 {
		t.Fatalf("shutdownDrainGaveUp=%d after a drain that finished", g)
	}
	if took > shutdownFDDrainBound873 {
		t.Fatalf("the drain took %v (bound %v)", took, shutdownFDDrainBound873)
	}
}

// TestShutdownDrainEndsALinkedRecvAfterItsSendCQE884 is trial (a).
func TestShutdownDrainEndsALinkedRecvAfterItsSendCQE884(t *testing.T) {
	f := newFDLFixture(t, false)
	w, cs := f.w, f.cs
	cs.writeBuf = append(cs.writeBuf, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"...)
	if retry := w.flushSendLink(cs); retry || !cs.recvLinked || !cs.recvArmed || !cs.sending || cs.kernelInflight != 2 {
		t.Fatalf("no linked SEND+RECV placed: retry=%v recvLinked=%v recvArmed=%v sending=%v kernelInflight=%d",
			retry, cs.recvLinked, cs.recvArmed, cs.sending, cs.kernelInflight)
	}
	if _, err := w.ring.Submit(); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if !reapReal884(t, w, func(c *completionEntry) bool { return c.UserData&udMask == udSend }) {
		t.Fatal("the SEND's completion never reached the ring")
	}
	// The worker is held here: the SEND's CQE read, its linked recv owed, no
	// completion for it (the peer sends nothing).
	if cs.sending || cs.kernelInflight != 1 || !cs.recvArmed {
		t.Fatalf("after the SEND's CQE: sending=%v kernelInflight=%d recvArmed=%v, want false 1 true", cs.sending, cs.kernelInflight, cs.recvArmed)
	}
	time.Sleep(50 * time.Millisecond)
	if head, tail := w.ring.BeginCQ(); head != tail {
		t.Fatalf("%d completion(s) in the ring with the peer silent: the recv did not stay owed", tail-head)
	}
	if !fdOwed(cs) {
		t.Fatal("setup: the conn owes the kernel nothing, so there is nothing for the drain to end")
	}
	drainEnded884(t, f)
}

// TestShutdownDrainEndsAReArmedMultishotRecv884 is trial (b).
func TestShutdownDrainEndsAReArmedMultishotRecv884(t *testing.T) {
	f := newMultishotFixture869(t)
	w, cs := f.w, f.cs
	f.armFirstRecv() // the first recv's SQE, taken off the ring: its completion is the test's
	if !cs.recvArmed || cs.kernelInflight != 1 {
		t.Fatalf("setup: recvArmed=%v kernelInflight=%d after the first arm", cs.recvArmed, cs.kernelInflight)
	}
	// The kernel ends the multishot with ENOBUFS (-105); handleRecv publishes
	// the buffers and re-arms.
	f.process(f.recvCQE(-105))
	// The worker is held here: the re-armed recv's SQE is in the SQ ring and
	// owed, and no enter has submitted it.
	if !cs.recvArmed || cs.kernelInflight != 1 || w.ring.Pending() == 0 {
		t.Fatalf("after the ENOBUFS: recvArmed=%v kernelInflight=%d pending SQEs=%d, want a re-armed recv, owed and unsubmitted",
			cs.recvArmed, cs.kernelInflight, w.ring.Pending())
	}
	if !fdOwed(cs) {
		t.Fatal("setup: the conn owes the kernel nothing, so there is nothing for the drain to end")
	}
	drainEnded884(t, f)
}
