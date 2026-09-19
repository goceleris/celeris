//go:build linux

package iouring

import (
	"context"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// celeris#657 face 2 (the fd-lifetime rule, R0): a connection leaves io_uring
// only when no read can still resolve its descriptor. The hand-off used to dup
// the fd, close the original and cancel the armed recv with a SKIP_SUCCESS
// cancel, while that recv could still complete with the client's next request
// or, through a recycled fd number, another connection's. Measured: client
// errors == joined stale-data CQEs, 1,707 == 1,707 in 48 of 48 runs.
//
// The fix has two halves. HOLD: while a drain is set, an eligible response is
// flushed with no recv behind it, so its SEND completion finds nothing in
// flight and hands the conn off. REAP: a conn whose recv is already armed is
// not handed off; the worker cancels that recv with a reported cancel of its
// own tag and hands off at the recv's -ECANCELED. A cancel that misses is
// retried; it is never followed by a hand-off.

// TestTransplantNeverHandsOffArmedRecv pins R0 at the sync hand-off site.
func TestTransplantNeverHandsOffArmedRecv(t *testing.T) {
	// The udSend dispatch site exactly as run()'s inlined switch runs it:
	// the send is handled, then, with a drain set, tryTransplant. The base
	// hands the conn off here with its linked RECV still armed.
	t.Run("dispatch_site_refuses_and_reaps", func(t *testing.T) {
		f := newFDLFixture(t, false)
		f.armFirstRecv()
		f.deliver(fdlGET)
		if got := takeSQEs(f.w.ring); len(got) != 2 || got[1].op != opRECV || got[0].flags&sqeIOLink == 0 {
			t.Fatalf("setup: request placed %v, want SEND|LINK then RECV", got)
		}
		f.startDrain()
		c := f.sendCQE()
		if !f.w.staleConnCQE(c, f.fd, c.UserData) {
			f.w.handleSend(c, f.fd, 1)
			if f.w.transplant.Load() != nil {
				f.w.tryTransplant(f.fd)
			}
		}
		if n := f.tgt.adopted.Load(); n != 0 {
			t.Fatalf("handed off %d time(s) with the linked RECV armed: that recv reads the "+
				"client's next request after the fd moved (TransplantHandoffInFlight=%d)",
				n, f.e.metrics.handoffLoss.handoffInFlight.Load())
		}
		if f.w.conns[f.fd] != f.cs || !f.cs.recvArmed {
			t.Fatal("the conn left the table, or its recv was forgotten, without a hand-off")
		}
		sqes := takeSQEs(f.w.ring)
		if len(sqes) != 1 || !f.isReap(sqes[0]) {
			t.Fatalf("placed %v, want exactly one REPORTED cancel of this recv's user_data "+
				"tagged 0x09 (the reap)", sqes)
		}
	})

	// A request served while a drain is set is HELD: its response goes out
	// with no recv behind it, so its SEND completion finds nothing in flight
	// and hands the conn off there. Through processCQE, which the
	// listener-close harvest uses.
	t.Run("held_response_hands_off_with_nothing_in_flight", func(t *testing.T) {
		f := newFDLFixture(t, false)
		f.armFirstRecv()
		f.serveOne()
		f.startDrain()
		f.deliver(fdlGET) // the linked recv takes request 2 while the drain is set
		sq := takeSQEs(f.w.ring)
		if len(sq) != 1 || sq[0].op != opSEND || sq[0].flags&sqeIOLink != 0 {
			t.Fatalf("a request served while a drain is set placed %v, want one UNLINKED SEND "+
				"and no recv behind it (HOLD)", sq)
		}
		if f.cs.recvArmed {
			t.Fatal("a held response left a recv armed")
		}
		f.process(f.sendCQE())
		if n := f.tgt.adopted.Load(); n != 1 {
			t.Fatalf("the held conn's SEND completion made %d hand-offs, want 1", n)
		}
		if got := takeSQEs(f.w.ring); len(got) != 0 {
			t.Fatalf("the hand-off placed %v, want nothing: no op was in flight to cancel", got)
		}
		if n := f.e.metrics.handoffLoss.handoffInFlight.Load(); n != 0 {
			t.Fatalf("TransplantHandoffInFlight = %d, want 0", n)
		}
		if n := metric(t, f.e, "TransplantHeld"); n != 1 {
			t.Errorf("TransplantHeld = %d, want 1", n)
		}
	})

	t.Run("cancelled_recv_hands_off", func(t *testing.T) {
		f := newFDLFixture(t, false)
		f.armFirstRecv()
		f.serveOne()
		// Request 2 is served before the drain starts: its response goes out
		// with a linked RECV, and the drain is set while that SEND is in flight.
		f.deliver(fdlGET)
		_ = takeSQEs(f.w.ring)
		f.startDrain()
		f.process(f.sendCQE())
		sqes := takeSQEs(f.w.ring)
		if n := f.tgt.adopted.Load(); n != 0 || len(sqes) != 1 || !f.isReap(sqes[0]) {
			t.Fatalf("SEND completion with the linked RECV armed: %d hand-off(s), placed %v; "+
				"want 0 and exactly one reap", n, sqes)
		}
		closes := f.w.closeCount.Load()
		f.process(f.recvCQE(-int32(unix.ECANCELED)))
		if n := f.tgt.adopted.Load(); n != 1 {
			t.Fatalf("the reaped recv's -ECANCELED made %d hand-offs, want 1 (closeCount %d -> %d: "+
				"a -ECANCELED routed to the generic error branch closes a healthy conn)",
				n, closes, f.w.closeCount.Load())
		}
		if f.w.closeCount.Load() != closes {
			t.Fatal("the reaped conn was closed")
		}
		if n := f.e.metrics.handoffLoss.handoffInFlight.Load(); n != 0 {
			t.Fatalf("TransplantHandoffInFlight = %d, want 0", n)
		}
		// The cancel's own completion (a hit) arrives for a conn that left.
		c := f.reapCQE(1)
		if !f.w.staleConnCQE(c, f.fd, c.UserData) {
			t.Fatal("the reap's completion was not stale after the hand-off")
		}
	})

	t.Run("data_first_is_served_and_held", func(t *testing.T) {
		f := newFDLFixture(t, false)
		f.armFirstRecv()
		f.serveOne()
		f.deliver(fdlGET)
		_ = takeSQEs(f.w.ring)
		f.startDrain()
		f.process(f.sendCQE())
		if sqes := takeSQEs(f.w.ring); len(sqes) != 1 || !f.isReap(sqes[0]) {
			t.Fatalf("placed %v, want one reap", sqes)
		}
		reqs := f.w.reqBatch
		// The client's next request beats the cancel: the recv completes
		// with data. It must be served here, not dropped, and its response
		// held.
		f.deliver(fdlGET)
		if f.w.reqBatch != reqs+1 {
			t.Fatalf("the request that beat the reap was not served (reqBatch %d -> %d)", reqs, f.w.reqBatch)
		}
		sqes := takeSQEs(f.w.ring)
		if len(sqes) != 1 || sqes[0].op != opSEND || sqes[0].flags&sqeIOLink != 0 {
			t.Fatalf("its response placed %v, want one UNLINKED SEND and no recv (HOLD)", sqes)
		}
		if f.tgt.adopted.Load() != 0 {
			t.Fatal("handed off with the response still in flight")
		}
		// The reap's cancel then reports its miss: no hand-off follows it.
		f.process(f.reapCQE(0))
		if f.tgt.adopted.Load() != 0 {
			t.Fatal("a missed reap was followed by a hand-off")
		}
		f.process(f.sendCQE())
		if n := f.tgt.adopted.Load(); n != 1 {
			t.Fatalf("the held response's SEND completion made %d hand-offs, want 1", n)
		}
		if n := f.e.metrics.handoffLoss.handoffInFlight.Load(); n != 0 {
			t.Fatalf("TransplantHandoffInFlight = %d, want 0", n)
		}
	})

	// The kernel, not the test, completes the ops: the reap's SQE really
	// cancels the armed recv (so its user_data target is right), and the
	// hand-off follows the -ECANCELED in whichever order the two CQEs come.
	// A kernel that rejects IORING_ASYNC_CANCEL flags (before 5.19) fails the
	// reap with -EINVAL and leaves the recv armed; the fixture places the
	// reap regardless of the startup probe, so on such a kernel this checks
	// that the failure is neither retried nor followed by a hand-off.
	t.Run("kernel_cancels_the_armed_recv", func(t *testing.T) {
		f := newFDLFixture(t, false)
		if !f.w.prepareRecv(f.cs, f.cs.buf) {
			t.Fatal("arm refused")
		}
		if _, err := f.w.ring.Submit(); err != nil {
			t.Fatalf("submit recv: %v", err)
		}
		f.startDrain()
		f.w.tryTransplant(f.fd)
		if n := f.tgt.adopted.Load(); n != 0 {
			t.Fatalf("tryTransplant handed off an idle conn with its recv armed (%d)", n)
		}
		if f.w.ring.Pending() != 1 {
			t.Fatalf("tryTransplant placed %d SQEs, want the reap", f.w.ring.Pending())
		}
		if _, err := f.w.ring.Submit(); err != nil {
			t.Fatalf("submit reap: %v", err)
		}
		var reapRes *int32
		recvCancelled := false
		for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
			if reapRes != nil && (recvCancelled || *reapRes == -int32(unix.EINVAL)) {
				break
			}
			_ = f.w.ring.WaitCQETimeout(100 * time.Millisecond)
			head, tail := f.w.ring.BeginCQ()
			for ; head != tail; head++ {
				c := *f.w.ring.cqeAt(head)
				switch decodeOp(c.UserData) {
				case udRecv:
					if c.Res != -int32(unix.ECANCELED) {
						t.Fatalf("the recv completed with %d, want -ECANCELED", c.Res)
					}
					recvCancelled = true
				case wantReapTag:
					r := c.Res
					reapRes = &r
				}
				f.w.processCQE(context.Background(), &c, time.Now().UnixNano())
			}
			f.w.ring.EndCQ(head)
		}
		if reapRes == nil {
			t.Fatal("the reap produced no completion of its own")
		}
		if *reapRes == -int32(unix.EINVAL) {
			if recvCancelled || f.tgt.adopted.Load() != 0 {
				t.Fatalf("the kernel rejected the reap (-EINVAL), yet recvCancelled=%v and %d hand-off(s)",
					recvCancelled, f.tgt.adopted.Load())
			}
			for i := 1; i <= 3; i++ {
				f.w.drainDetachQueue()
				if n := f.w.ring.Pending(); n != 0 {
					t.Fatalf("loop iteration %d placed %d SQE(s) after the kernel rejected the reap: the "+
						"failed cancel is retried, one failing cancel per iteration for as long as the drain lasts", i, n)
				}
			}
			t.Logf("celeris681 the kernel rejected the reap's cancel flags (-EINVAL): not retried, no hand-off")
			return
		}
		if !recvCancelled {
			t.Fatalf("the reap completed with %d but the recv's -ECANCELED never came", *reapRes)
		}
		if n := f.tgt.adopted.Load(); n != 1 {
			t.Fatalf("%d hand-offs after the kernel cancelled the recv, want 1", n)
		}
	})
}

// TestTransplantReapMissIsRetried: a reap that finds nothing to cancel (the
// recv completed first, or is still linked behind its SEND and not issued yet)
// is retried and never followed by a hand-off; and its count is a COUNT, so a
// stale miss cannot clear a newer reap (the celeris#484/#596 lesson).
func TestTransplantReapMissIsRetried(t *testing.T) {
	armedAndReaped := func(t *testing.T) *fdlFixture {
		t.Helper()
		f := newFDLFixture(t, false)
		f.armFirstRecv()
		f.serveOne()
		f.deliver(fdlGET)
		_ = takeSQEs(f.w.ring)
		f.startDrain()
		f.process(f.sendCQE())
		if sqes := takeSQEs(f.w.ring); len(sqes) != 1 || !f.isReap(sqes[0]) {
			t.Fatalf("SEND completion with the linked RECV armed placed %v, want one reap", sqes)
		}
		return f
	}

	t.Run("miss_is_retried_never_handed_off", func(t *testing.T) {
		f := armedAndReaped(t)
		f.process(f.reapCQE(-int32(unix.ENOENT)))
		if f.tgt.adopted.Load() != 0 || !f.cs.recvArmed {
			t.Fatal("a missed reap was followed by a hand-off with the recv still armed")
		}
		// The next loop iteration retries it.
		f.w.drainDetachQueue()
		sqes := takeSQEs(f.w.ring)
		if len(sqes) != 1 || !f.isReap(sqes[0]) {
			t.Fatalf("the next iteration placed %v, want the reap again", sqes)
		}
		f.process(f.recvCQE(-int32(unix.ECANCELED)))
		if n := f.tgt.adopted.Load(); n != 1 {
			t.Fatalf("the retried reap's -ECANCELED made %d hand-offs, want 1", n)
		}
		if n := metric(t, f.e, "TransplantReapMisses"); n != 1 {
			t.Errorf("TransplantReapMisses = %d, want 1", n)
		}
		if n := metric(t, f.e, "TransplantReaps"); n != 2 {
			t.Errorf("TransplantReaps = %d, want 2", n)
		}
	})

	t.Run("stale_miss_cannot_clear_a_newer_reap", func(t *testing.T) {
		f := armedAndReaped(t)
		// Reap 1 is in flight. The client's request beats it; the drain
		// stops before the response, so the response goes out with a new
		// linked RECV (recv 2).
		f.stopDrain()
		f.deliver(fdlGET)
		if sqes := takeSQEs(f.w.ring); len(sqes) != 2 || sqes[1].op != opRECV {
			t.Fatalf("with the drain stopped the response placed %v, want SEND then RECV", sqes)
		}
		// A new drain (the next flap): recv 2 gets a reap of its own while
		// reap 1's completion is still owed.
		f.startDrain()
		f.process(f.sendCQE())
		if sqes := takeSQEs(f.w.ring); len(sqes) != 1 || !f.isReap(sqes[0]) {
			t.Fatalf("recv 2 got %v, want its own reap while reap 1 is still owed", sqes)
		}
		// Reap 1 reports its miss (its recv had already completed). This must
		// not forget reap 2, which is about to hit.
		f.process(f.reapCQE(0))
		f.w.drainDetachQueue()
		if sqes := takeSQEs(f.w.ring); len(sqes) != 0 {
			t.Fatalf("a stale miss re-reaped a recv that already has a reap in flight: %v", sqes)
		}
		closes := f.w.closeCount.Load()
		f.process(f.recvCQE(-int32(unix.ECANCELED)))
		if f.w.closeCount.Load() != closes {
			t.Fatal("reap 2's -ECANCELED closed the conn: the stale miss cleared the reap's state " +
				"and the cancel fell through to the generic error branch")
		}
		if n := f.tgt.adopted.Load(); n != 1 {
			t.Fatalf("reap 2's -ECANCELED made %d hand-offs, want 1", n)
		}
	})
}

// TestHoldReleasedWhenDrainStops: a held conn (response flushed with no recv
// behind it) must get its recv armed at that SEND's completion when it is not
// handed off there — here because the drain stopped in between. Otherwise it
// waits for a request it can never read.
func TestHoldReleasedWhenDrainStops(t *testing.T) {
	held := func(t *testing.T) *fdlFixture {
		t.Helper()
		f := newFDLFixture(t, false)
		f.armFirstRecv()
		f.serveOne()
		f.startDrain()
		f.deliver(fdlGET)
		if sqes := takeSQEs(f.w.ring); len(sqes) != 1 || sqes[0].op != opSEND || sqes[0].flags&sqeIOLink != 0 {
			t.Fatalf("a response while a drain is set placed %v, want one UNLINKED SEND (HOLD)", sqes)
		}
		// serveOne left the linked RECV armed; it took request 2 above, so
		// the conn now has no recv at all.
		if f.cs.recvArmed {
			t.Fatal("held conn has a recv armed")
		}
		return f
	}

	t.Run("processCQE_send_rearms_after_drain_stops", func(t *testing.T) {
		f := held(t)
		f.stopDrain()
		f.process(f.sendCQE())
		sqes := takeSQEs(f.w.ring)
		if len(sqes) != 1 || sqes[0].op != opRECV || sqes[0].tag() != udRecv {
			t.Fatalf("the held conn's SEND completion placed %v, want its recv armed", sqes)
		}
		if !f.cs.recvArmed || f.tgt.adopted.Load() != 0 {
			t.Fatalf("recvArmed=%v adopted=%d, want true/0", f.cs.recvArmed, f.tgt.adopted.Load())
		}
		if n := metric(t, f.e, "TransplantHoldRescued"); n != 0 {
			t.Errorf("TransplantHoldRescued = %d, want 0", n)
		}
	})

	t.Run("processCQE_send_hands_off_while_drain_set", func(t *testing.T) {
		f := held(t)
		f.process(f.sendCQE())
		if n := f.tgt.adopted.Load(); n != 1 {
			t.Fatalf("processCQE handled the held conn's SEND completion with a drain set and "+
				"made %d hand-offs, want 1", n)
		}
	})

	// The worker's own loop: the inlined udSend dispatch site.
	t.Run("worker_loop_rearms_after_drain_stops", testHoldReleasedInTheWorkerLoop)

	t.Run("refused_hand_off_rearms", func(t *testing.T) {
		f := held(t)
		f.tgt.refuse = true
		f.process(f.sendCQE())
		// The target refused: reclaimTransplant re-attached the dup'd fd as
		// a new conn with its own recv; the old slot is empty.
		if f.w.transplantHandoffRefused.Load() != 1 {
			t.Fatalf("hand-off refusals = %d, want 1", f.w.transplantHandoffRefused.Load())
		}
		armed := 0
		for _, s := range takeSQEs(f.w.ring) {
			if s.op == opRECV {
				armed++
			}
		}
		if armed != 1 {
			t.Fatalf("after a refused hand-off %d recv(s) were armed, want exactly 1", armed)
		}
	})
}

// TestHoldRescuedByCheckTimeouts is the belt under the release: a held conn
// whose send is done and that was neither handed off nor re-armed (a path that
// skipped the release) gets its recv armed by the timeout sweep, and the
// rescue is counted. The counter must stay 0 in every real run.
func TestHoldRescuedByCheckTimeouts(t *testing.T) {
	f := newFDLFixture(t, false)
	f.armFirstRecv()
	f.serveOne()
	f.startDrain()
	f.deliver(fdlGET)
	if sqes := takeSQEs(f.w.ring); len(sqes) != 1 || sqes[0].op != opSEND {
		t.Fatalf("placed %v, want one held SEND", sqes)
	}
	// The SEND completes through handleSend alone: no dispatch site ran, so
	// nothing released the hold.
	f.stopDrain()
	c := f.sendCQE()
	if !f.w.staleConnCQE(c, f.fd, c.UserData) {
		f.w.handleSend(c, f.fd, 1)
	}
	if sqes := takeSQEs(f.w.ring); len(sqes) != 0 || f.cs.recvArmed {
		t.Fatalf("handleSend alone placed %v (recvArmed=%v); the release belongs to the dispatch site", sqes, f.cs.recvArmed)
	}
	f.w.checkTimeouts()
	sqes := takeSQEs(f.w.ring)
	if len(sqes) != 1 || sqes[0].op != opRECV {
		t.Fatalf("checkTimeouts placed %v, want the stranded conn's recv", sqes)
	}
	if n := metric(t, f.e, "TransplantHoldRescued"); n != 1 {
		t.Fatalf("TransplantHoldRescued = %d, want 1", n)
	}
	// A second sweep finds nothing more to do.
	f.w.checkTimeouts()
	if sqes := takeSQEs(f.w.ring); len(sqes) != 0 {
		t.Fatalf("a second sweep placed %v", sqes)
	}
}

// TestOneOwnerPerHandoff (A6): a promoted async conn whose dispatch goroutine
// has claimed its own hand-off (transplantPending) belongs to that claim, and
// a finishAsyncTransplant for a connState that no longer owns its slot must
// not touch the fd number. Measured on the base: one identity moved twice in
// 167 runs, 20 us apart, the only run with a negative gauge.
func TestOneOwnerPerHandoff(t *testing.T) {
	// claimedAsync is a promoted async conn parked at a boundary whose
	// goroutine has marked it and exited. Nothing is in flight, so the only
	// thing between it and a second hand-off is the ownership check.
	claimedAsync := func(t *testing.T) *fdlFixture {
		t.Helper()
		f := newFDLFixture(t, true)
		f.cs.asyncPromoted.Store(true)
		f.cs.transplantPending.Store(true)
		f.startDrain()
		return f
	}

	// Leaving a claimed conn to its claim is ordering, not a double claim:
	// it is counted as TransplantClaimDeferred, a rate, so that
	// TransplantDoubleClaim can be held at 0 (celeris#681 C3).
	t.Run("tryTransplant_refuses_a_claimed_conn", func(t *testing.T) {
		f := claimedAsync(t)
		f.w.tryTransplant(f.fd)
		if n := f.tgt.adopted.Load(); n != 0 {
			t.Fatalf("tryTransplant moved a conn its dispatch goroutine had claimed (%d hand-offs); "+
				"finishAsyncTransplant would then move it again", n)
		}
		if n := metric(t, f.e, "TransplantDoubleClaim"); n != 0 {
			t.Errorf("TransplantDoubleClaim = %d, want 0: deferring to a claim is not a double claim", n)
		}
		if n := metric(t, f.e, "TransplantClaimDeferred"); n != 1 {
			t.Errorf("TransplantClaimDeferred = %d, want 1", n)
		}
	})

	t.Run("finishAsyncTransplant_refuses_a_stale_owner", func(t *testing.T) {
		f := claimedAsync(t)
		old := f.cs
		// The slot now belongs to a different connState on the same fd
		// number (the old one left and the number was reused).
		next := acquireConnState(context.Background(), f.fd, 4096, true)
		f.w.conns[f.fd] = next
		f.w.finishAsyncTransplant(old)
		if n := f.tgt.adopted.Load(); n != 0 {
			t.Fatalf("finishAsyncTransplant handed off fd %d for a connState that no longer owns "+
				"it (%d hand-offs): that is the next owner's socket", f.fd, n)
		}
		if f.w.conns[f.fd] != next || !fdIsOpen(f.fd) {
			t.Fatal("the next owner of the fd number lost its slot or its descriptor")
		}
		if n := metric(t, f.e, "TransplantDoubleClaim"); n != 1 {
			t.Errorf("TransplantDoubleClaim = %d, want 1", n)
		}
		f.w.conns[f.fd] = old // let the fixture's cleanup close the fd
	})

	// A claimed conn that is closing (a close deferred behind a send) is
	// refused like any closing conn. It still owns its slot, so this is no
	// double claim, and counting it would make a gate of 0 fail on a close.
	t.Run("closing_conn_is_refused_not_counted", func(t *testing.T) {
		f := claimedAsync(t)
		f.cs.closing = true
		f.w.finishAsyncTransplant(f.cs)
		f.cs.closing = false
		if n := f.tgt.adopted.Load(); n != 0 {
			t.Fatalf("finishAsyncTransplant handed off a closing conn (%d hand-offs)", n)
		}
		if n := metric(t, f.e, "TransplantDoubleClaim"); n != 0 {
			t.Errorf("TransplantDoubleClaim = %d, want 0: a closing conn that owns its slot is no double claim", n)
		}
	})

	// The measured interleaving end to end: the goroutine claims the
	// hand-off and enqueues itself; before the worker drains the queue, a
	// SEND completion for the same conn reaches the udSend dispatch site.
	t.Run("send_completion_between_claim_and_drain", func(t *testing.T) {
		f := claimedAsync(t)
		f.w.enqueueDetach(f.cs)
		f.cs.writeBuf = append(f.cs.writeBuf, "HTTP/1.1 200 OK\r\ncontent-length: 2\r\n\r\nok"...)
		if f.w.flushSend(f.cs) {
			t.Fatal("setup: flushSend found the ring full")
		}
		_ = takeSQEs(f.w.ring)
		c := f.sendCQE()
		if !f.w.staleConnCQE(c, f.fd, c.UserData) {
			f.w.handleSend(c, f.fd, 1)
			if f.w.transplant.Load() != nil {
				f.w.tryTransplant(f.fd)
			}
		}
		if f.w.conns[f.fd] == nil {
			// The sync path took the conn and closed its fd. In the measured
			// run the number had already been reused by the time the queued
			// claim ran; reuse it here the same way (a decoy socket on the
			// same number), so a second hand-off is visible as a hand-off.
			a, b := socketPairFDs(t)
			t.Cleanup(func() { _ = unix.Close(b) })
			if a != f.fd { // the kernel may already have handed out the freed number
				if err := unix.Dup3(a, f.fd, unix.O_CLOEXEC); err != nil {
					t.Fatalf("dup3: %v", err)
				}
				_ = unix.Close(a)
			}
		}
		f.w.drainDetachQueue()
		if n := f.tgt.adopted.Load(); n != 1 {
			t.Fatalf("the conn was handed off %d times, want exactly once", n)
		}
		if n := metric(t, f.e, "TransplantClaimDeferred"); n != 1 {
			t.Errorf("TransplantClaimDeferred = %d, want 1 (the sync attempt left the conn to its claim)", n)
		}
		if n := metric(t, f.e, "TransplantDoubleClaim"); n != 0 {
			t.Errorf("TransplantDoubleClaim = %d, want 0: the conn was handed off once", n)
		}
	})
}

// TestNoDrainSQESequenceIsUnchanged is the witness that routing every
// response tail through one hold-aware helper costs nothing when no drain is
// set: per request, the SQEs placed (opcode, flags including IO_LINK, op tag,
// fd) are exactly the base's. It passes on the base by construction; a change
// to the steady-state sequence fails it.
func TestNoDrainSQESequenceIsUnchanged(t *testing.T) {
	type want struct {
		op    uint8
		flags uint8
		tag   uint64
	}
	check := func(t *testing.T, f *fdlFixture, got []sqeRec, w []want) {
		t.Helper()
		ok := len(got) == len(w)
		for i := 0; ok && i < len(w); i++ {
			ok = got[i].op == w[i].op && got[i].flags == w[i].flags && got[i].tag() == w[i].tag &&
				int(got[i].fd) == f.fd
		}
		if !ok {
			t.Fatalf("per-request SQEs %v, want %+v", got, w)
		}
	}

	t.Run("sync_tail", func(t *testing.T) {
		f := newFDLFixture(t, false)
		f.armFirstRecv()
		for i := 0; i < 3; i++ {
			f.deliver(fdlGET)
			check(t, f, takeSQEs(f.w.ring), []want{{opSEND, sqeIOLink, udSend}, {opRECV, 0, udRecv}})
			f.process(f.sendCQE())
			check(t, f, takeSQEs(f.w.ring), nil)
		}
	})

	t.Run("sync_tail_writev", func(t *testing.T) {
		f := newFDLFixture(t, false)
		f.armFirstRecv()
		f.deliver("GET /large HTTP/1.1\r\nHost: x\r\n\r\n")
		check(t, f, takeSQEs(f.w.ring), []want{{opWRITEV, 0, udSend}, {opRECV, 0, udRecv}})
		f.process(f.sendCQE())
		check(t, f, takeSQEs(f.w.ring), nil)
	})

	t.Run("direct_body_tail", func(t *testing.T) {
		f := newFDLFixture(t, false)
		f.armFirstRecv()
		// Half the body: no response yet, and the next recv goes straight
		// into the body buffer.
		f.deliver("POST / HTTP/1.1\r\nHost: x\r\nContent-Length: 20\r\n\r\n0123456789")
		check(t, f, takeSQEs(f.w.ring), []want{{opRECV, 0, udRecv}})
		if !f.cs.recvIntoBody || len(f.cs.bodyRecvPin) < 10 {
			t.Fatalf("setup: the body recv was not armed into the body buffer")
		}
		n := copy(f.cs.bodyRecvPin, "abcdefghij")
		f.process(f.recvCQE(int32(n)))
		check(t, f, takeSQEs(f.w.ring), []want{{opSEND, 0, udSend}, {opRECV, 0, udRecv}})
		f.process(f.sendCQE())
		check(t, f, takeSQEs(f.w.ring), nil)
	})
}

// reapedFixture is a keep-alive conn whose response SEND completed while a
// drain was set and its linked RECV was armed: the hand-off refused it and
// placed one reap of that recv, which the test now completes as it likes.
func reapedFixture(t *testing.T) *fdlFixture {
	t.Helper()
	f := newFDLFixture(t, false)
	f.armFirstRecv()
	f.serveOne()
	f.deliver(fdlGET)
	_ = takeSQEs(f.w.ring)
	f.startDrain()
	f.process(f.sendCQE())
	if sqes := takeSQEs(f.w.ring); len(sqes) != 1 || !f.isReap(sqes[0]) {
		t.Fatalf("SEND completion with the linked RECV armed placed %v, want one reap", sqes)
	}
	return f
}

// heldHandOff delivers the client's next request on f's armed recv and
// checks that its response is HELD (one unlinked SEND, no recv) and that the
// SEND's completion hands the conn off with nothing in flight: the way a conn
// the hand-off could not reap leaves.
func heldHandOff(t *testing.T, f *fdlFixture) {
	t.Helper()
	f.deliver(fdlGET)
	if sqes := takeSQEs(f.w.ring); len(sqes) != 1 || sqes[0].op != opSEND || sqes[0].flags&sqeIOLink != 0 {
		t.Fatalf("the next request's response placed %v, want one UNLINKED SEND and no recv (HOLD)", sqes)
	}
	f.process(f.sendCQE())
	if n := f.tgt.adopted.Load(); n != 1 {
		t.Fatalf("the held response's SEND completion made %d hand-offs, want 1", n)
	}
	if n := f.e.metrics.handoffLoss.handoffInFlight.Load(); n != 0 {
		t.Fatalf("TransplantHandoffInFlight = %d, want 0", n)
	}
}

// TestTransplantReapFailureIsNotRetried (celeris#681 C1): a reap whose own
// completion is neither a hit nor a miss is a cancel that failed, and the
// kernel will fail the next one the same way. -EINVAL is what a kernel that
// rejects IORING_ASYNC_CANCEL flags returns (before 5.19). Retrying it placed
// a failing cancel on every loop iteration for as long as the drain lasted.
// It must be counted, not retried and not followed by a hand-off; the conn
// then leaves the way any unreaped conn does, after its next response.
func TestTransplantReapFailureIsNotRetried(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  int32
	}{
		{"EINVAL", -int32(unix.EINVAL)},
		{"EBADF", -int32(unix.EBADF)},
		{"ECANCELED", -int32(unix.ECANCELED)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := reapedFixture(t)
			f.process(f.reapCQE(tc.res))
			if n := f.tgt.adopted.Load(); n != 0 {
				t.Fatalf("a reap that failed with %d was followed by %d hand-off(s) with the recv armed", tc.res, n)
			}
			for i := 1; i <= 3; i++ {
				f.w.drainDetachQueue()
				if sqes := takeSQEs(f.w.ring); len(sqes) != 0 {
					t.Fatalf("loop iteration %d placed %v after the reap failed with %d: a failed cancel "+
						"is retried, one per iteration for as long as the drain lasts", i, sqes, tc.res)
				}
			}
			if !f.cs.recvArmed || f.w.conns[f.fd] != f.cs {
				t.Fatalf("recvArmed=%v, in table=%v: the conn must keep its recv and stay",
					f.cs.recvArmed, f.w.conns[f.fd] == f.cs)
			}
			if n := metric(t, f.e, "TransplantReapFailed"); n != 1 {
				t.Errorf("TransplantReapFailed = %d, want 1", n)
			}
			if n := metric(t, f.e, "TransplantReapMisses"); n != 0 {
				t.Errorf("TransplantReapMisses = %d, want 0: a failed cancel is not a miss", n)
			}
			heldHandOff(t, f)
		})
	}
}

// TestNoReapWithoutAsyncCancelFlags (celeris#681 C1): on a kernel that
// rejects IORING_ASYNC_CANCEL flags, which the engine's startup probe finds,
// no reap is ever placed, at either hand-off site or on a later iteration: it
// could only fail with -EINVAL and leave the recv armed. The conn keeps its
// recv and stays until that recv completes; a sync conn then leaves after its
// next response, held, with nothing in flight.
func TestNoReapWithoutAsyncCancelFlags(t *testing.T) {
	noFlags := func(t *testing.T, f *fdlFixture) {
		t.Helper()
		if !trySetWorkerField(f.w, "asyncCancelFlags", false) {
			t.Error("Worker has no asyncCancelFlags field: nothing tells the hand-off the kernel rejects cancel flags")
		}
	}
	checkNothingPlaced := func(t *testing.T, f *fdlFixture, where string) {
		t.Helper()
		if sqes := takeSQEs(f.w.ring); len(sqes) != 0 {
			t.Fatalf("%s placed %v on a kernel without cancel flags, want nothing: a reap fails with "+
				"-EINVAL there and leaves the recv armed", where, sqes)
		}
		if n := f.tgt.adopted.Load(); n != 0 {
			t.Fatalf("%s handed the conn off %d time(s) with its recv armed", where, n)
		}
		for i := 1; i <= 3; i++ {
			f.w.drainDetachQueue()
			if sqes := takeSQEs(f.w.ring); len(sqes) != 0 {
				t.Fatalf("loop iteration %d after %s placed %v, want nothing", i, where, sqes)
			}
		}
		if !f.cs.recvArmed {
			t.Fatalf("after %s the conn has no recv armed", where)
		}
	}

	t.Run("sync_site", func(t *testing.T) {
		f := newFDLFixture(t, false)
		noFlags(t, f)
		f.armFirstRecv()
		f.serveOne()
		f.deliver(fdlGET)
		_ = takeSQEs(f.w.ring)
		f.startDrain()
		f.process(f.sendCQE()) // the SEND completes with its linked RECV armed
		checkNothingPlaced(t, f, "the SEND completion")
		if n := metric(t, f.e, "TransplantReaps"); n != 0 {
			t.Errorf("TransplantReaps = %d, want 0", n)
		}
		if n := metric(t, f.e, "TransplantReapUnsupported"); n != 1 {
			t.Errorf("TransplantReapUnsupported = %d, want 1", n)
		}
		heldHandOff(t, f)
	})

	// A promoted async conn whose goroutine claimed its own hand-off: the
	// recv the feed path armed for it is the one op in flight.
	t.Run("async_site", func(t *testing.T) {
		f := newFDLFixture(t, true)
		noFlags(t, f)
		f.cs.asyncPromoted.Store(true)
		f.armFirstRecv()
		f.startDrain()
		f.cs.transplantPending.Store(true)
		f.w.enqueueDetach(f.cs)
		f.w.drainDetachQueue()
		checkNothingPlaced(t, f, "the drain of the goroutine's claim")
		if n := metric(t, f.e, "TransplantReaps"); n != 0 {
			t.Errorf("TransplantReaps = %d, want 0", n)
		}
	})
}

// TestReapSuppressedAfterFailedHandOff (celeris#681 C2): a hand-off that
// fails at its dup (a process out of descriptors) leaves the conn in place,
// and the recv re-armed for it used to be reaped again at once, failing the
// same way: a RECV, a cancel and two completions per loop iteration for as
// long as the failure and the drain lasted. No reap may follow a failed
// hand-off until the conn receives data again, and once the dup works the
// conn leaves normally.
func TestReapSuppressedAfterFailedHandOff(t *testing.T) {
	f := reapedFixture(t)
	dupCalls := 0
	failDup := func(int) (int, error) {
		dupCalls++
		return -1, unix.EMFILE
	}
	if !trySetWorkerField(f.w, "dupFD", failDup) {
		t.Error("Worker has no dupFD seam: the dup failure cannot be injected, the real dup runs")
	}
	// The reap lands; the hand-off it re-runs fails at the dup.
	f.process(f.recvCQE(-int32(unix.ECANCELED)))
	if n := f.tgt.adopted.Load(); n != 0 {
		t.Fatalf("the conn was handed off (%d) although its dup failed", n)
	}
	sqes := takeSQEs(f.w.ring)
	if len(sqes) != 1 || sqes[0].op != opRECV || sqes[0].tag() != udRecv {
		t.Fatalf("after the failed hand-off the completion placed %v, want only the conn's recv re-armed; "+
			"a reap of it fails the hand-off again at once", sqes)
	}
	for i := 1; i <= 3; i++ {
		f.w.drainDetachQueue()
		if sqes := takeSQEs(f.w.ring); len(sqes) != 0 {
			t.Fatalf("loop iteration %d placed %v after the failed hand-off, want nothing", i, sqes)
		}
	}
	// The client's next request: served here and HELD (the drain is still
	// set); its SEND completion tries the hand-off, whose dup fails again,
	// so the recv is re-armed, and again not reaped.
	f.deliver(fdlGET)
	if sqes := takeSQEs(f.w.ring); len(sqes) != 1 || sqes[0].op != opSEND {
		t.Fatalf("the next request placed %v, want its held SEND", sqes)
	}
	f.process(f.sendCQE())
	if sqes := takeSQEs(f.w.ring); len(sqes) != 1 || sqes[0].op != opRECV {
		t.Fatalf("the held SEND's completion, with the dup failing, placed %v, want only the recv re-armed", sqes)
	}
	if dupCalls != 2 {
		t.Errorf("dup tried %d times, want 2: once at the reap, once at the held response", dupCalls)
	}
	// The descriptors are back, and the conn receives data again, which
	// lifts the suppression: served with the drain stopped, its response
	// goes out with a linked RECV, and once the drain is set again that recv
	// is reaped and the conn leaves at its -ECANCELED.
	trySetWorkerField(f.w, "dupFD", (func(int) (int, error))(nil))
	f.stopDrain()
	f.deliver(fdlGET)
	if sqes := takeSQEs(f.w.ring); len(sqes) != 2 || sqes[1].op != opRECV {
		t.Fatalf("a request with the drain stopped placed %v, want SEND then its linked RECV", sqes)
	}
	f.startDrain()
	f.process(f.sendCQE())
	if sqes := takeSQEs(f.w.ring); len(sqes) != 1 || !f.isReap(sqes[0]) {
		t.Fatalf("after the conn received data again its armed recv got %v, want a reap: the "+
			"suppression outlived the failure it was for", sqes)
	}
	f.process(f.recvCQE(-int32(unix.ECANCELED)))
	if n := f.tgt.adopted.Load(); n != 1 {
		t.Fatalf("the reap's -ECANCELED made %d hand-offs, want 1", n)
	}
}

// TestReapedRecvLeavesNoLinkOrBuffer (celeris#681 C4): reapOutcome consumes
// the reaped recv's -ECANCELED, the last completion that recv has, so it
// must leave what handleRecv leaves after any recv's last completion: no
// stale recvLinked (only the chained recv's own completion clears it), and a
// provided buffer the completion carries returned to the ring.
func TestReapedRecvLeavesNoLinkOrBuffer(t *testing.T) {
	t.Run("linked_recv_is_unlinked", func(t *testing.T) {
		f := newFDLFixture(t, false)
		f.armFirstRecv()
		f.serveOne()
		if !f.cs.recvLinked {
			t.Fatal("setup: serveOne left no linked RECV")
		}
		f.startDrain()
		f.w.tryTransplant(f.fd)
		if sqes := takeSQEs(f.w.ring); len(sqes) != 1 || !f.isReap(sqes[0]) {
			t.Fatalf("placed %v, want the reap of the linked RECV", sqes)
		}
		f.stopDrain() // the conn stays: its recv is re-armed standalone
		f.process(f.recvCQE(-int32(unix.ECANCELED)))
		if sqes := takeSQEs(f.w.ring); len(sqes) != 1 || sqes[0].op != opRECV || sqes[0].flags&sqeIOLink != 0 {
			t.Fatalf("placed %v, want one standalone RECV", sqes)
		}
		if f.cs.recvLinked {
			t.Fatal("recvLinked is still set after the linked recv's last completion: the recv armed now is standalone")
		}
	})

	t.Run("provided_buffer_goes_back", func(t *testing.T) {
		f := reapedFixture(t)
		br, err := NewBufferRing(f.w.ring, 7, 4, 64)
		if err != nil {
			skipOrFail656(t, "provided buffer ring unavailable: %v", err)
		}
		t.Cleanup(func() { br.Close(f.w.ring) })
		f.w.bufRing = br
		tail := br.tail
		f.stopDrain()
		c := f.recvCQE(-int32(unix.ECANCELED))
		c.Flags = cqeFBuffer | 2<<16
		f.process(c)
		if br.tail != tail+1 || !f.w.hasBufReturns {
			t.Fatalf("provided buffer 2 not returned (ring tail %d -> %d, hasBufReturns=%v)",
				tail, br.tail, f.w.hasBufReturns)
		}
	})
}
