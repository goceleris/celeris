//go:build linux

package iouring

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// recvArmFixture is a real ring plus one connState on a socketpair, wired so
// drainDetachQueue's pause/resume branches and staleConnCQE's accounting run
// unmodified. The peer end is returned so a test can complete an armed recv
// by writing to it.
type recvArmFixture struct {
	ring *Ring
	w    *Worker
	cs   *connState
	peer int
}

func newRecvArmFixture(t *testing.T) *recvArmFixture {
	t.Helper()
	ring := newTestRing(t)
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Skipf("socketpair unavailable: %v", err)
	}
	local, peer := pair[0], pair[1]
	// The ring is closed by newTestRing's cleanup, which cancels any recv
	// still armed on local; close the sockets after that.
	t.Cleanup(func() { _ = unix.Close(peer); _ = unix.Close(local) })

	w := &Worker{
		ring:      ring,
		conns:     make([]*connState, local+1),
		errCount:  &atomic.Uint64{},
		recvArm:   &recvArmStats{},
		h2EventFD: -1, // enqueueDetach then skips the wakeup write
	}
	cs := &connState{
		fd:         local,
		liveIdx:    -1,
		generation: 3,
		buf:        make([]byte, 4096),
		detachMu:   &sync.Mutex{},
	}
	w.conns[local] = cs
	return &recvArmFixture{ring: ring, w: w, cs: cs, peer: peer}
}

// submit pushes every pending SQE to the kernel and fails the test on a
// partial submit, so a t.Skipf-style silent pass is impossible.
func (f *recvArmFixture) submit(t *testing.T) {
	t.Helper()
	want := f.ring.Pending()
	n, err := f.ring.Submit()
	if err != nil || uint32(n) != want {
		t.Fatalf("submit: %d of %d SQEs accepted, err=%v", n, want, err)
	}
}

// reap waits up to timeout for at least one CQE and returns every CQE the
// ring holds, copied out before the CQ head advances.
func (f *recvArmFixture) reap(t *testing.T, timeout time.Duration) []completionEntry {
	t.Helper()
	if err := f.ring.WaitCQETimeout(timeout); err != nil {
		t.Fatalf("no CQE within %v: %v", timeout, err)
	}
	head, tail := f.ring.BeginCQ()
	var out []completionEntry
	for ; head != tail; head++ {
		out = append(out, *f.ring.cqeAt(head))
	}
	f.ring.EndCQ(head)
	return out
}

// pause / resume drive the real middleware hand-off: the desired state is
// published on cs and the conn is queued for the worker, exactly as the
// PauseRecv / ResumeRecv callbacks installed at detach time do.
func (f *recvArmFixture) pause() {
	f.cs.recvPauseDesired.Store(true)
	f.w.enqueueDetach(f.cs)
	f.w.drainDetachQueue()
}

func (f *recvArmFixture) resume() {
	f.cs.recvPauseDesired.Store(false)
	f.w.enqueueDetach(f.cs)
	f.w.drainDetachQueue()
}

// counts snapshots the five witnesses in a fixed order for comparisons:
// [resumeWhileCancelPending resumeWhileRecvInFlight armDeclined doubleArmed cqeUnaccounted].
func (f *recvArmFixture) counts() [5]uint64 {
	s := f.w.recvArm
	return [5]uint64{
		s.resumeWhileCancelPending.Load(),
		s.resumeWhileRecvInFlight.Load(),
		s.armDeclined.Load(),
		s.doubleArmed.Load(),
		s.cqeUnaccounted.Load(),
	}
}

// TestResumeBeforeCancelLandsPlacesNoSecondRecv drives the celeris#484 window
// through the real call sites rather than prepareRecv's own early return: a
// recv is armed and submitted, the middleware pauses (drainDetachQueue places
// the ASYNC_CANCEL and marks recvCancelPending), and BEFORE any CQE is reaped
// the middleware resumes. The resume branch must witness the window
// (RecvResumeWhileCancelPending), decline the arm (RecvArmDeclined), and leave
// exactly one recv outstanding with nothing added to the SQ ring.
//
// It then submits and reaps the cancel's -ECANCELED, which handleRecv's
// recvCancelPending branch answers by re-arming exactly once, and completes
// that recv with peer data so the terminal-CQE accounting is checked end to
// end: RecvDoubleArmed and RecvCQEUnaccounted stay 0 on an honest tree.
func TestResumeBeforeCancelLandsPlacesNoSecondRecv(t *testing.T) {
	f := newRecvArmFixture(t)
	w, cs, ring := f.w, f.cs, f.ring

	if !w.prepareRecv(cs, cs.buf) {
		t.Fatal("first arm refused on an empty ring")
	}
	if ring.Pending() != 1 || cs.recvOutstanding != 1 {
		t.Fatalf("after first arm: pending=%d outstanding=%d, want 1/1", ring.Pending(), cs.recvOutstanding)
	}
	f.submit(t)

	f.pause()
	if !cs.recvPaused || cs.recvCancelPending != 1 {
		t.Fatalf("pause did not place the cancel: recvPaused=%v recvCancelPending=%d", cs.recvPaused, cs.recvCancelPending)
	}
	if ring.Pending() != 1 {
		t.Fatalf("pause placed %d SQEs, want exactly the cancel", ring.Pending())
	}

	// The window: cancel in the SQ ring, not yet submitted, and the
	// middleware already withdrew the pause.
	f.resume()
	if got := f.counts(); got != [5]uint64{1, 1, 1, 0, 0} {
		t.Fatalf("after resume-before-cancel: [resumeWhileCancelPending resumeWhileRecvInFlight armDeclined doubleArmed cqeUnaccounted] = %v, want [1 1 1 0 0]", got)
	}
	if got := ring.Pending(); got != 1 {
		t.Fatalf("resume placed a second recv SQE: pending=%d (cancel only would be 1); both recvs target cs.buf (celeris#484)", got)
	}
	if cs.recvOutstanding != 1 || !cs.recvArmed {
		t.Fatalf("after resume: outstanding=%d recvArmed=%v, want 1/true", cs.recvOutstanding, cs.recvArmed)
	}
	if cs.recvPaused {
		t.Fatal("resume did not clear recvPaused")
	}

	// Let the cancel land. Two completions now: the cancel's own, reporting
	// how many ops it cancelled (celeris#596 made it reported rather than
	// skip-success), and the recv's -ECANCELED.
	//
	// The cancel HIT here, and that is the half of celeris#596 that must not
	// regress: a cancel that cancelled something leaves recvCancelPending set,
	// because handleRecv's -ECANCELED branch reads it to tell a withdrawn
	// pause (re-arm, the celeris#484 fix) from an I/O error (close).
	f.submit(t)
	cqes := f.reap(t, 2*time.Second)
	if len(cqes) != 2 {
		t.Fatalf("expected the cancel's own CQE and the recv's -ECANCELED, got %+v", cqes)
	}
	var recvCQE *completionEntry
	for i := range cqes {
		if decodeOp(cqes[i].UserData) == udRecv {
			if cqes[i].Res != -int32(unix.ECANCELED) {
				t.Fatalf("recv CQE res=%d, want -ECANCELED", cqes[i].Res)
			}
			recvCQE = &cqes[i]
			continue
		}
		if cqes[i].Res <= 0 {
			t.Fatalf("the cancel reported res=%d, want > 0: it was supposed to cancel the armed recv", cqes[i].Res)
		}
		w.processCQE(t.Context(), &cqes[i], time.Now().UnixNano())
		if cs.recvCancelPending != 1 {
			t.Fatal("a cancel that HIT retired recvCancelPending; the -ECANCELED that follows " +
				"would then fall through to the generic error path and close a healthy conn (celeris#484)")
		}
	}
	if recvCQE == nil {
		t.Fatalf("no recv CQE among %+v", cqes)
	}
	pendingBefore := ring.Pending()
	w.processCQE(t.Context(), recvCQE, time.Now().UnixNano())
	if got := ring.Pending(); got != pendingBefore+1 {
		t.Fatalf("the withdrawn pause's -ECANCELED re-armed %d recvs, want exactly 1", got-pendingBefore)
	}
	if cs.recvOutstanding != 1 || cs.recvCancelPending != 0 {
		t.Fatalf("after -ECANCELED re-arm: outstanding=%d recvCancelPending=%d, want 1/0", cs.recvOutstanding, cs.recvCancelPending)
	}
	f.submit(t)

	// Complete the re-armed recv with real bytes; its terminal CQE must be
	// the one the bookkeeping expects.
	if _, err := unix.Write(f.peer, []byte("ping")); err != nil {
		t.Fatalf("peer write: %v", err)
	}
	cqes = f.reap(t, 2*time.Second)
	for i := range cqes {
		if decodeOp(cqes[i].UserData) == udRecv {
			w.staleConnCQE(&cqes[i], cs.fd, cqes[i].UserData)
		}
	}
	if got := f.counts(); got != [5]uint64{1, 1, 1, 0, 0} {
		t.Fatalf("after the data CQE: witnesses = %v, want [1 1 1 0 0]", got)
	}
	if cs.recvOutstanding != 0 {
		t.Fatalf("outstanding=%d after the terminal CQE, want 0", cs.recvOutstanding)
	}
}

// TestStaleRecvArmedIsWitnessedByTerminalCQE is the negative control for the
// kernel-side witness. The guard in prepareRecv is only as good as
// cs.recvArmed; if the bookkeeping is cleared while the kernel still holds
// the recv (the gen-collision residual documented at staleConnCQE, or any
// future clear site), the guard passes a second arm with recvOutstanding
// going 0→1, never 2, so RecvDoubleArmed cannot see it. The kernel then posts
// two terminal recv CQEs for one counted placement, and the second one
// arrives with nothing outstanding: that is RecvCQEUnaccounted.
//
// The test forces exactly that: arm, pause (cancel placed), clear the
// bookkeeping as a stale site would, resume (second recv placed), then reap
// both terminal CQEs through staleConnCQE.
func TestStaleRecvArmedIsWitnessedByTerminalCQE(t *testing.T) {
	f := newRecvArmFixture(t)
	w, cs, ring := f.w, f.cs, f.ring

	if !w.prepareRecv(cs, cs.buf) {
		t.Fatal("first arm refused on an empty ring")
	}
	f.submit(t)
	f.pause()

	// Stale bookkeeping: recvArmed lies while recv #1 is kernel-held.
	cs.recvArmed = false
	cs.recvOutstanding = 0

	f.resume()
	if got := ring.Pending(); got != 2 {
		t.Fatalf("resume on stale bookkeeping placed %d SQEs beyond the cancel, want 1 (the second recv)", int(got)-1)
	}
	// resumeWhileRecvInFlight is 0 here BY CONSTRUCTION: the stale
	// bookkeeping says nothing is armed, so the window witness is blind
	// to it too — only the terminal CQE below can see it.
	if got := f.counts(); got != [5]uint64{1, 0, 0, 0, 0} {
		t.Fatalf("after resume: witnesses = %v, want [1 0 0 0 0] — the userspace guard cannot see this double recv", got)
	}
	if cs.recvOutstanding != 1 {
		t.Fatalf("outstanding=%d, want 1 (the second placement is the only one the bookkeeping knows)", cs.recvOutstanding)
	}
	f.submit(t)

	// One recv is cancelled, the other completes with data: two terminal
	// recv CQEs for one counted placement.
	if _, err := unix.Write(f.peer, []byte("ping")); err != nil {
		t.Fatalf("peer write: %v", err)
	}
	terminal := 0
	deadline := time.Now().Add(2 * time.Second)
	for terminal < 2 && time.Now().Before(deadline) {
		for _, c := range f.reap(t, 2*time.Second) {
			if decodeOp(c.UserData) != udRecv || cqeHasMore(c.Flags) {
				continue
			}
			terminal++
			w.staleConnCQE(&c, cs.fd, c.UserData)
		}
	}
	if terminal != 2 {
		t.Fatalf("reaped %d terminal recv CQEs, want 2 (one per kernel-held recv)", terminal)
	}
	if got := f.counts(); got != [5]uint64{1, 0, 0, 0, 1} {
		t.Fatalf("witnesses = %v, want [1 0 0 0 1]: exactly one terminal recv CQE with nothing outstanding", got)
	}
	if cs.recvOutstanding != 0 {
		t.Fatalf("outstanding=%d after both terminal CQEs, want 0", cs.recvOutstanding)
	}
}
