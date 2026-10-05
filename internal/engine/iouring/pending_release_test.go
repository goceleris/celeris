//go:build linux

package iouring

import (
	"testing"
	"time"
)

// TestPendingReleaseDrainsWhenNoKernelOps verifies the fast path of the
// cancel-then-release close discipline (v1.4.15/7beebb9 corruption fix): a closed connState with NO
// kernel-held ops (kernelInflight == 0) is released on the next drain
// pass without waiting for any wall-clock hold. The old fixed 100 ms hold
// is gone — release is gated on the kernel's terminal CQEs, not time.
func TestPendingReleaseDrainsWhenNoKernelOps(t *testing.T) {
	w := &Worker{}
	start := time.Now().UnixNano()

	w.queuePendingRelease(&connState{})         // pooled-recycle path
	w.queuePendingReleaseDetached(&connState{}) // detached path (drops the ref)
	if got := len(w.pendingRelease); got != 2 {
		t.Fatalf("queued = %d, want 2", got)
	}

	// Long before the backstop deadline, both entries drain because the
	// kernel owes them nothing.
	w.cachedNow = start
	w.drainPendingRelease()
	if got := len(w.pendingRelease); got != 0 {
		t.Fatalf("inflight==0 entries not drained promptly: %d left, want 0", got)
	}
}

// TestPendingReleaseHeldWhileKernelOpsInflight is the v1.4.15/7beebb9-corruption regression
// guard: a closed connState whose kernel ops have NOT all delivered their
// terminal CQE must NOT be released — by any number of event-loop
// iterations — until either the CQEs arrive or the wall-clock BACKSTOP
// elapses. The v1.4.15/7beebb9 heap corruption happened exactly because
// release was time-based (100 ms) and shorter than TCP's 200 ms RTO_MIN,
// so a retransmitted POST segment was DMA'd into freed memory.
func TestPendingReleaseHeldWhileKernelOpsInflight(t *testing.T) {
	w := &Worker{}
	start := time.Now().UnixNano()

	cs := &connState{fd: 9, generation: 3, kernelInflight: 1, recvArmed: true}
	w.noteClosedInflight(cs)
	w.queuePendingRelease(cs)

	// No amount of draining may release the entry while the kernel still
	// holds an op and the backstop hasn't elapsed.
	w.cachedNow = start
	for i := 0; i < 100_000; i++ {
		w.drainPendingRelease()
	}
	if got := len(w.pendingRelease); got != 1 {
		t.Fatalf("entry with kernelInflight=1 drained before its terminal CQE: %d left, want 1", got)
	}

	// The terminal CQE arrives (stale: the conn is closed, so it routes
	// through the closedOps map) → the entry drains on the next pass.
	c := &completionEntry{
		UserData: encodeUserDataGen(udRecv, cs.fd, cs.generation),
		Res:      -104, // -ECONNRESET; any terminal completion works
	}
	w.conns = make([]*connState, 16) // slot 9 nil — conn was closed
	if !w.staleConnCQE(c, cs.fd, c.UserData) {
		t.Fatalf("CQE for closed conn not reported stale")
	}
	if cs.kernelInflight != 0 {
		t.Fatalf("stale terminal CQE did not drain kernelInflight: %d", cs.kernelInflight)
	}
	w.drainPendingRelease()
	if got := len(w.pendingRelease); got != 0 {
		t.Fatalf("entry not drained after terminal CQE: %d left, want 0", got)
	}
	if got := len(w.closedOps); got != 0 {
		t.Fatalf("closedOps not cleaned up after terminal CQE: %d entries left", got)
	}
}

// TestPendingReleaseBackstopIsWallClock verifies the last-resort backstop:
// an entry whose kernel ops never produce a CQE (kernel anomaly) is held
// for the full wall-clock window — which must comfortably exceed TCP
// RTO_MIN (200 ms) — and only then released, with its closedOps identity
// scrubbed so a later CQE cannot touch the released connState.
func TestPendingReleaseBackstopIsWallClock(t *testing.T) {
	if pendingReleaseHoldNanos < int64(time.Second) {
		t.Fatalf("backstop hold %v must be far above TCP RTO_MIN (200ms); "+
			"the 100ms-class hold is exactly the v1.4.15/7beebb9 bug", time.Duration(pendingReleaseHoldNanos))
	}

	w := &Worker{}
	start := time.Now().UnixNano()

	cs := &connState{fd: 5, generation: 7, kernelInflight: 1, recvArmed: true}
	w.noteClosedInflight(cs)
	w.queuePendingRelease(cs)
	deadline := w.pendingRelease[0].releaseAtNanos
	if deadline < start+pendingReleaseHoldNanos {
		t.Fatalf("backstop deadline %d < start+hold %d: not wall-clock based",
			deadline, start+pendingReleaseHoldNanos)
	}

	// Just before the deadline: held.
	w.cachedNow = deadline - 1
	w.drainPendingRelease()
	if got := len(w.pendingRelease); got != 1 {
		t.Fatalf("backstop released early: %d left, want 1", got)
	}

	// Past the deadline: released (WARN-worthy anomaly) and scrubbed from
	// closedOps so a late CQE is a no-op instead of a write into pooled
	// memory.
	w.cachedNow = deadline + 1
	w.drainPendingRelease()
	if got := len(w.pendingRelease); got != 0 {
		t.Fatalf("backstop did not release: %d left, want 0", got)
	}
	if got := len(w.closedOps); got != 0 {
		t.Fatalf("backstop release left closedOps entry behind: %d", got)
	}
	// A straggler CQE after the backstop must not touch anything.
	late := &completionEntry{UserData: encodeUserDataGen(udRecv, 5, 7), Res: 1}
	w.conns = make([]*connState, 16)
	if !w.staleConnCQE(late, 5, late.UserData) {
		t.Fatalf("post-backstop CQE not reported stale")
	}
}

// TestPendingReleaseStragglerDoesNotBlockQueue verifies the queue is
// compacted, not FIFO-prefix drained: one straggling entry (kernel op
// unaccounted) must not pin the release of entries queued after it whose
// ops have drained.
func TestPendingReleaseStragglerDoesNotBlockQueue(t *testing.T) {
	w := &Worker{}

	straggler := &connState{fd: 3, generation: 1, kernelInflight: 1, recvArmed: true}
	w.noteClosedInflight(straggler)
	w.queuePendingRelease(straggler)
	w.queuePendingRelease(&connState{fd: 4, generation: 1}) // drained
	w.queuePendingRelease(&connState{fd: 6, generation: 2}) // drained

	w.cachedNow = time.Now().UnixNano() // before any backstop deadline
	w.drainPendingRelease()
	if got := len(w.pendingRelease); got != 1 {
		t.Fatalf("straggler blocked the queue: %d left, want 1", got)
	}
	if w.pendingRelease[0].cs != straggler {
		t.Fatalf("wrong entry kept: fd=%d, want fd=3", w.pendingRelease[0].cs.fd)
	}
}

// TestClosedOpsCollisionHoldsAllConns covers the (fd, generation)
// collision case: two closed conns whose in-flight ops share a user_data
// identity (possible under fd reuse — generations are per-connState
// object). Their terminal CQEs are indistinguishable, so BOTH conns must
// be held until the COMBINED count drains; releasing either early on an
// arbitrary attribution would reopen the UAF.
func TestClosedOpsCollisionHoldsAllConns(t *testing.T) {
	const fd, gen = 12, 9
	w := &Worker{conns: make([]*connState, 32)}

	a := &connState{fd: fd, generation: gen, kernelInflight: 1, recvArmed: true}
	b := &connState{fd: fd, generation: gen, kernelInflight: 2, recvArmed: true}
	w.noteClosedInflight(a)
	w.noteClosedInflight(b)
	w.queuePendingRelease(a)
	w.queuePendingRelease(b)

	key := encodeConnOpKey(fd, gen)
	e := w.closedOps[key]
	if e == nil || e.inflight != 3 || len(e.conns) != 2 {
		t.Fatalf("collision entry not combined: %+v", e)
	}

	ud := encodeUserDataGen(udRecv, fd, gen)
	w.cachedNow = time.Now().UnixNano()

	// Two of three terminal CQEs: nobody releases yet.
	for i := 0; i < 2; i++ {
		c := &completionEntry{UserData: ud, Res: -125} // -ECANCELED
		if !w.staleConnCQE(c, fd, ud) {
			t.Fatalf("CQE %d not stale", i)
		}
	}
	w.drainPendingRelease()
	if got := len(w.pendingRelease); got != 2 {
		t.Fatalf("released before combined count drained: %d left, want 2", got)
	}

	// Third terminal CQE: combined count hits zero → both release.
	c := &completionEntry{UserData: ud, Res: -125}
	if !w.staleConnCQE(c, fd, ud) {
		t.Fatalf("final CQE not stale")
	}
	w.drainPendingRelease()
	if got := len(w.pendingRelease); got != 0 {
		t.Fatalf("not released after combined count drained: %d left", got)
	}
	if got := len(w.closedOps); got != 0 {
		t.Fatalf("closedOps not cleaned: %d entries", got)
	}
}

// TestStaleTerminalOpIgnoresIntermediateCQEs verifies only TERMINAL CQEs
// decrement the closed conn's accounting: a multishot-recv CQE carrying
// CQE_F_MORE (kernel still holds the op) and the cancel op's own CQE
// (udProvide tag never reaches staleConnCQE) must not be counted.
func TestStaleTerminalOpIgnoresIntermediateCQEs(t *testing.T) {
	const fd, gen = 8, 4
	w := &Worker{conns: make([]*connState, 32)}

	cs := &connState{fd: fd, generation: gen, kernelInflight: 1, recvArmed: true}
	w.noteClosedInflight(cs)
	w.queuePendingRelease(cs)

	ud := encodeUserDataGen(udRecv, fd, gen)

	// Intermediate multishot CQE (F_MORE set): stale, but NOT terminal.
	mid := &completionEntry{UserData: ud, Res: 64, Flags: cqeFMore}
	if !w.staleConnCQE(mid, fd, ud) {
		t.Fatalf("intermediate CQE not stale")
	}
	if cs.kernelInflight != 1 {
		t.Fatalf("intermediate F_MORE CQE drained the count: %d", cs.kernelInflight)
	}
	w.cachedNow = time.Now().UnixNano()
	w.drainPendingRelease()
	if got := len(w.pendingRelease); got != 1 {
		t.Fatalf("released on intermediate CQE: %d left, want 1", got)
	}

	// Terminal CQE (no F_MORE): drains and releases.
	fin := &completionEntry{UserData: ud, Res: -125}
	if !w.staleConnCQE(fin, fd, ud) {
		t.Fatalf("terminal CQE not stale")
	}
	w.drainPendingRelease()
	if got := len(w.pendingRelease); got != 0 {
		t.Fatalf("not released after terminal CQE: %d left", got)
	}
}

// TestLiveTerminalCQEUpdatesInflight verifies the live-conn side of the
// accounting chokepoint: a matching-generation terminal recv CQE clears
// recvArmed and decrements kernelInflight.
//
// The zero clamp exercised at the end is DAMAGE LIMITATION, not full
// protection: it only stops a misrouted CQE (generation collision across
// fd reuse, 1/65536 per reuse with the 16-bit tag) from pushing an
// already-drained counter negative. A misroute that lands while the live
// conn still has ops in flight under-counts it — a 1→0 misdecrement
// unlocks early release at that conn's own close (see the known-residual
// comment in staleConnCQE). The residual's production signal is the
// orphaned closedOps entry firing the 5 s backstop WARN.
func TestLiveTerminalCQEUpdatesInflight(t *testing.T) {
	const fd, gen = 6, 2
	w := &Worker{conns: make([]*connState, 32)}
	cs := &connState{fd: fd, generation: gen, kernelInflight: 2, recvArmed: true}
	w.conns[fd] = cs

	udR := encodeUserDataGen(udRecv, fd, gen)
	if w.staleConnCQE(&completionEntry{UserData: udR, Res: 10}, fd, udR) {
		t.Fatalf("live matching-gen CQE reported stale")
	}
	if cs.recvArmed || cs.kernelInflight != 1 {
		t.Fatalf("recv terminal CQE bookkeeping: recvArmed=%v inflight=%d, want false/1",
			cs.recvArmed, cs.kernelInflight)
	}

	udS := encodeUserDataGen(udSend, fd, gen)
	if w.staleConnCQE(&completionEntry{UserData: udS, Res: 10}, fd, udS) {
		t.Fatalf("live matching-gen send CQE reported stale")
	}
	if cs.kernelInflight != 0 {
		t.Fatalf("send terminal CQE did not decrement: %d", cs.kernelInflight)
	}

	// SEND_ZC first CQE (F_MORE): not terminal, no decrement, no clamp issues.
	cs.kernelInflight = 1
	if w.staleConnCQE(&completionEntry{UserData: udS, Res: 10, Flags: cqeFMore}, fd, udS) {
		t.Fatalf("ZC first CQE reported stale")
	}
	if cs.kernelInflight != 1 {
		t.Fatalf("ZC F_MORE CQE decremented early: %d", cs.kernelInflight)
	}
	// NOTIF CQE (F_NOTIF, no F_MORE): terminal.
	if w.staleConnCQE(&completionEntry{UserData: udS, Res: 0, Flags: cqeFNotif}, fd, udS) {
		t.Fatalf("ZC NOTIF CQE reported stale")
	}
	if cs.kernelInflight != 0 {
		t.Fatalf("ZC NOTIF CQE did not decrement: %d", cs.kernelInflight)
	}

	// Clamp: an extra terminal CQE (gen-collision misroute) must not go
	// negative.
	if w.staleConnCQE(&completionEntry{UserData: udS, Res: 10}, fd, udS) {
		t.Fatalf("live matching-gen send CQE reported stale")
	}
	if cs.kernelInflight != 0 {
		t.Fatalf("counter went negative on misrouted CQE: %d", cs.kernelInflight)
	}
}
