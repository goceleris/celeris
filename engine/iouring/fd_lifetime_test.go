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
		got := 0
		for deadline := time.Now().Add(2 * time.Second); got < 2 && time.Now().Before(deadline); {
			_ = f.w.ring.WaitCQETimeout(100 * time.Millisecond)
			head, tail := f.w.ring.BeginCQ()
			for ; head != tail; head++ {
				c := *f.w.ring.cqeAt(head)
				if decodeOp(c.UserData) == udRecv && c.Res != -int32(unix.ECANCELED) {
					t.Fatalf("the recv completed with %d, want -ECANCELED", c.Res)
				}
				f.w.processCQE(context.Background(), &c, time.Now().UnixNano())
				got++
			}
			f.w.ring.EndCQ(head)
		}
		if got != 2 {
			t.Fatalf("kernel produced %d completions, want the recv's -ECANCELED and the reap's own", got)
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

	t.Run("tryTransplant_refuses_a_claimed_conn", func(t *testing.T) {
		f := claimedAsync(t)
		f.w.tryTransplant(f.fd)
		if n := f.tgt.adopted.Load(); n != 0 {
			t.Fatalf("tryTransplant moved a conn its dispatch goroutine had claimed (%d hand-offs); "+
				"finishAsyncTransplant would then move it again", n)
		}
		if n := metric(t, f.e, "TransplantDoubleClaim"); n != 1 {
			t.Errorf("TransplantDoubleClaim = %d, want 1", n)
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
		if n := metric(t, f.e, "TransplantDoubleClaim"); n != 1 {
			t.Errorf("TransplantDoubleClaim = %d, want 1 (the sync claim refused)", n)
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
