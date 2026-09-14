//go:build linux

package iouring

import (
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestMissedPauseCancelClearsCancelPending is the celeris#596 rig, flipped to
// assert the fixed behaviour. It drives the MISS: the armed recv completes
// with data BEFORE the backpressure pause's ASYNC_CANCEL runs, so the cancel
// matches nothing and no -ECANCELED will ever arrive to retire
// cs.recvCancelPending. The cancel's own completion is then the only event
// that can, and while that completion was suppressed (CQE_SKIP_SUCCESS over a
// miss the kernel reports as res == 0) and tagged udProvide, the flag stayed
// true for the rest of the connection's life and every subsequent resume was
// miscounted as a resume inside the celeris#484 window — 10 993 against 1 real
// entry at MaxBackpressureBuffer=8.
//
// Deliberately written without naming udRecvCancel or asserting the cancel
// CQE's result, so the same test body runs against origin/main, where it fails
// on the recvCancelPending assertion rather than on a tag or a missing CQE.
//
// The other half of the contract is enforced by
// TestResumeBeforeCancelLandsPlacesNoSecondRecv (recv_arming_window_test.go):
// when the cancel HITS, recvCancelPending must survive the resume so
// handleRecv's -ECANCELED branch still re-arms (celeris#484 / #560).
func TestMissedPauseCancelClearsCancelPending(t *testing.T) {
	f := newRecvArmFixture(t)
	w, cs := f.w, f.cs

	if !w.prepareRecv(cs, cs.buf) {
		t.Fatal("first arm refused on an empty ring")
	}
	f.submit(t)

	// The recv completes with real bytes. Drained through staleConnCQE, the
	// accounting chokepoint every conn-bound CQE passes through, so recvArmed
	// and recvOutstanding land exactly as they do on the worker; the protocol
	// machinery above it is not what this test is about.
	if _, err := unix.Write(f.peer, []byte("ping")); err != nil {
		t.Fatalf("peer write: %v", err)
	}
	cqes := f.reap(t, 2*time.Second)
	if len(cqes) != 1 || decodeOp(cqes[0].UserData) != udRecv || cqes[0].Res != 4 {
		t.Fatalf("expected one 4-byte recv CQE, got %+v", cqes)
	}
	if w.staleConnCQE(&cqes[0], cs.fd, cqes[0].UserData) {
		t.Fatal("the live conn's own recv CQE was judged stale")
	}
	if cs.recvArmed || cs.recvOutstanding != 0 {
		t.Fatalf("after the data CQE: recvArmed=%v outstanding=%d, want false/0", cs.recvArmed, cs.recvOutstanding)
	}

	// Now the pause places its cancel — for a recv that is already gone.
	f.pause()
	if !cs.recvPaused || cs.recvCancelPending != 1 {
		t.Fatalf("pause did not place the cancel: recvPaused=%v recvCancelPending=%d", cs.recvPaused, cs.recvCancelPending)
	}
	f.submit(t)

	// On origin/main this reap times out and returns nothing: the miss
	// completes as res == 0, which CQE_SKIP_SUCCESS suppresses. Tolerate that
	// here so the failure below is the state assertion, not the plumbing.
	_ = f.ring.WaitCQETimeout(2 * time.Second)
	head, tail := f.ring.BeginCQ()
	n := 0
	for ; head != tail; head++ {
		c := *f.ring.cqeAt(head)
		if decodeOp(c.UserData) == udRecv {
			t.Fatalf("unexpected recv CQE after the cancel: %+v", c)
		}
		w.processCQE(t.Context(), &c, time.Now().UnixNano())
		n++
	}
	f.ring.EndCQ(head)
	if n != 1 {
		t.Fatalf("the missed cancel produced %d CQEs, want exactly 1 — its completion is "+
			"the only event that can retire recvCancelPending (celeris#596)", n)
	}
	if cs.recvCancelPending != 0 {
		t.Fatal("the missed cancel's completion did not retire recvCancelPending: " +
			"the flag no longer describes the cancel window (celeris#596)")
	}

	// The flipped witness. A resume after a cancel that missed is not inside
	// the celeris#484 window — nothing is in flight to arm on top of — and
	// must not be counted as one.
	f.resume()
	if got := f.counts(); got != [5]uint64{0, 0, 0, 0, 0} {
		t.Fatalf("after a resume that follows a MISSED cancel: [resumeWhileCancelPending "+
			"resumeWhileRecvInFlight armDeclined doubleArmed cqeUnaccounted] = %v, want all zero", got)
	}
	if cs.recvOutstanding != 1 || !cs.recvArmed {
		t.Fatalf("resume did not re-arm: outstanding=%d recvArmed=%v, want 1/true", cs.recvOutstanding, cs.recvArmed)
	}
}

// TestMissedCancelDoesNotRetireASecondOutstandingCancel is the rig for the
// second defect celeris#596 exposed: retiring the pause's cancel state is only
// safe if each cancel retires ITS OWN.
//
// A connection that pauses, resumes and pauses again before the ring is
// drained has two ASYNC_CANCELs in flight, and they resolve in either order.
// Here the first MISSES (its recv had already completed with data) and the
// second HITS. With recvCancelPending as a bool, the first one's completion
// cleared the state the second still needed: the hit's -ECANCELED then found
// no pending cancel, fell through handleRecv's generic negative-result path
// and closed a healthy connection mid-stream — the celeris#484 failure,
// reintroduced. As a count, the miss retires one and the -ECANCELED retires
// the other, so the connection is re-armed instead.
//
// Observed for real: one frame-corruption failure (io.ErrUnexpectedEOF) in 12
// runs of the celeris#484 WS oracle at MaxBackpressureBuffer=8, on the
// bool-valued intermediate version of this fix.
func TestMissedCancelDoesNotRetireASecondOutstandingCancel(t *testing.T) {
	f := newRecvArmFixture(t)
	w, cs, ring := f.w, f.cs, f.ring

	// recv #1 is armed and completes with data, so the first pause's cancel
	// has nothing left to match.
	if !w.prepareRecv(cs, cs.buf) {
		t.Fatal("first arm refused on an empty ring")
	}
	f.submit(t)
	if _, err := unix.Write(f.peer, []byte("ping")); err != nil {
		t.Fatalf("peer write: %v", err)
	}
	cqes := f.reap(t, 2*time.Second)
	if len(cqes) != 1 || decodeOp(cqes[0].UserData) != udRecv || cqes[0].Res != 4 {
		t.Fatalf("expected one 4-byte recv CQE, got %+v", cqes)
	}
	w.staleConnCQE(&cqes[0], cs.fd, cqes[0].UserData)

	// Pause #1: cancel #1 is submitted and misses. Its completion is left in
	// the ring on purpose — this test is about WHEN it is processed.
	f.pause()
	f.submit(t)

	// Resume #1 arms recv #2; pause #2 submits cancel #2, which hits it.
	f.resume()
	f.pause()
	if cs.recvCancelPending != 2 {
		t.Fatalf("recvCancelPending=%d after two pauses, want 2: each pause submits its own "+
			"ASYNC_CANCEL and both are outstanding", cs.recvCancelPending)
	}
	f.submit(t)

	var missCQE, hitCQE, ecanceled *completionEntry
	deadline := time.Now().Add(3 * time.Second)
	for (missCQE == nil || hitCQE == nil || ecanceled == nil) && time.Now().Before(deadline) {
		for _, c := range f.reap(t, 2*time.Second) {
			c := c
			switch {
			case decodeOp(c.UserData) == udRecv:
				if c.Res != -int32(unix.ECANCELED) {
					t.Fatalf("recv CQE res=%d, want -ECANCELED", c.Res)
				}
				ecanceled = &c
			case c.Res > 0:
				hitCQE = &c
			default:
				missCQE = &c
			}
		}
	}
	if missCQE == nil || hitCQE == nil || ecanceled == nil {
		t.Fatalf("did not see all three completions: miss=%v hit=%v ecanceled=%v", missCQE, hitCQE, ecanceled)
	}

	// The dangerous order: the MISS is processed first.
	w.processCQE(t.Context(), missCQE, time.Now().UnixNano())
	w.processCQE(t.Context(), hitCQE, time.Now().UnixNano())
	if cs.recvCancelPending != 1 {
		t.Fatalf("recvCancelPending=%d after the missed cancel's completion, want 1: the cancel "+
			"that HIT is still outstanding and its -ECANCELED has not arrived (celeris#596)", cs.recvCancelPending)
	}

	// The middleware withdraws pause #2 before the -ECANCELED lands.
	f.resume()
	if cs.recvPaused {
		t.Fatal("resume did not clear recvPaused")
	}

	pendingBefore := ring.Pending()
	w.processCQE(t.Context(), ecanceled, time.Now().UnixNano())
	if cs.closing {
		t.Fatal("the withdrawn pause's -ECANCELED closed a healthy connection: the missed cancel " +
			"had retired the state the cancel that HIT still needed (celeris#484 / #596)")
	}
	if got := ring.Pending(); got != pendingBefore+1 {
		t.Fatalf("the withdrawn pause's -ECANCELED re-armed %d recvs, want exactly 1", got-pendingBefore)
	}
	if cs.recvCancelPending != 0 || !cs.recvArmed {
		t.Fatalf("after the -ECANCELED re-arm: recvCancelPending=%d recvArmed=%v, want 0/true",
			cs.recvCancelPending, cs.recvArmed)
	}
	if got := f.counts(); got[3] != 0 || got[4] != 0 {
		t.Fatalf("witnesses = %v: doubleArmed and cqeUnaccounted must stay 0", got)
	}
}
