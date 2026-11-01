//go:build linux

package iouring

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/mono"
)

// celeris#1173, review r1 of #1196. What the sweep gives back to a conn whose
// bytes the engine itself held, and for which conns. The time is mono.Manual and
// the loop's pass clock (w.cachedNow, read once per pass) is set by the test where
// the loop would set it, so no test sleeps.

// sweep1173 is one pass's sweep: the loop reads the clock when the pass starts
// (cachedNow) and sweeps at its end.
func sweep1173(w *Worker) {
	w.cachedNow = mono.Now()
	w.checkTimeouts()
}

// A partial send's remainder found in the SQ ring is excused only the time the
// engine held it. On a conn that is not closing a partial completion does not
// restart the write clock (it runs from the first SEND of the response, the
// documented io_uring rule), so a restamp to "now" would hand a large response
// to a slow reader a whole new window whenever a sweep and a partial completion
// fall in the same pass: WriteTimeout would then depend on that coincidence.
func TestRemainderHeldInTheSQRingGetsBackOnlyTheStall1173(t *testing.T) {
	const wt = 100 * time.Millisecond
	clk := clock1173(t)
	rig := newStallRig704(t)
	w := rig.w
	w.cfg.WriteTimeout = wt
	w.cfg.IdleTimeout = time.Hour
	w.cfg.ReadTimeout = time.Hour

	// The first SEND of the response was stamped at T0 (the rig's cachedNow);
	// 0.8 WT later a pass starts and the peer's partial completion stages the rest.
	clk.Advance(wt * 8 / 10)
	w.cachedNow = mono.Now()
	inFlight750(rig, "0123456789abcdef", false)
	w.handleSend(sendCQE750(rig, 8, 0), rig.local, w.cachedNow)
	if !rig.cs.sending || string(rig.cs.sendBuf) != "89abcdef" || unconsumed1173(w) == 0 {
		t.Fatalf("apparatus: sending=%v sendBuf=%q unconsumed=%d, want the remainder staged", rig.cs.sending, rig.cs.sendBuf, unconsumed1173(w))
	}
	// The worker stalls for half of WriteTimeout before the sweep of this pass:
	// 1.3 WT after the first SEND. The remainder is excused that half, and only it.
	clk.Advance(wt / 2)
	w.checkTimeouts()
	if cut1173(rig) {
		t.Fatalf("the remainder was cut at 1.3 WT after the first SEND, 0.5 WT of it the engine's own stall")
	}
	if _, err := w.ring.Submit(); err != nil {
		t.Fatalf("apparatus: submit: %v", err)
	}
	// 1.45 WT after the first SEND, minus the 0.5 WT excused: 0.95 WT, not cut.
	clk.Advance(wt * 15 / 100)
	sweep1173(w)
	if cut1173(rig) {
		t.Fatalf("the remainder was cut 0.95 WT after the clock that the stall was excused from")
	}
	// 1.55 WT after the first SEND, minus 0.5 WT: 1.05 WT. Cut. (A restamp to the
	// sweep's own time would have given it a whole window again: 0.25 WT here.)
	clk.Advance(wt / 10)
	sweep1173(w)
	if !cut1173(rig) {
		t.Fatalf("a partial send's remainder, found in the SQ ring across a stall of 0.5 WT, was given more than the stall back: " +
			"WriteTimeout of a large response then depends on whether a sweep lands in the pass of a partial completion")
	}
}

// A conn on the dirty list with bytes (the SQ ring was full when they were
// staged) is as much in the engine's hands as a SEND in the ring: the kernel has
// none of them.
func TestDirtyConnWithBytesTheKernelNeverHadIsNotCutByAStall1173(t *testing.T) {
	const wt = 100 * time.Millisecond
	clk := clock1173(t)
	rig := newStallRig704(t)
	w := rig.w
	w.cfg.WriteTimeout = wt
	w.cfg.IdleTimeout = time.Hour
	w.cfg.ReadTimeout = time.Hour

	for w.ring.GetSQE() != nil { // the SQ ring is full
	}
	rig.cs.writeBuf = append(rig.cs.writeBuf[:0], "a response"...)
	if !w.flushSend(rig.cs) {
		t.Fatal("apparatus: flushSend found an SQE in a full ring")
	}
	w.markDirty(rig.cs)
	if !rig.cs.dirty || rig.cs.sending || len(rig.cs.writeBuf) == 0 {
		t.Fatalf("apparatus: dirty=%v sending=%v writeBuf=%d", rig.cs.dirty, rig.cs.sending, len(rig.cs.writeBuf))
	}
	clk.Advance(2 * wt)
	w.checkTimeouts()
	if cut1173(rig) {
		t.Fatalf("a conn whose bytes sat on the dirty list for %v (SQ ring full, WriteTimeout %v) was cut: the peer was handed nothing", 2*wt, wt)
	}
	// A conn that stays dirty is still cut: only the time of a pass is excused.
	clk.Advance(wt / 2)
	sweep1173(w)
	if cut1173(rig) {
		t.Fatalf("the dirty conn was cut %v after the stall's sweep (WriteTimeout %v)", wt/2, wt)
	}
	clk.Advance(wt)
	sweep1173(w)
	if !cut1173(rig) {
		t.Fatalf("a conn that stayed on the dirty list for %v after the stall was not cut (WriteTimeout %v)", wt+wt/2, wt)
	}
}

// The restamp must not reach a conn that is not the one that placed the SEND: the
// SQE of an earlier owner of the descriptor number (a generation behind) names
// the number only.
func TestRestampIgnoresASendOfAnEarlierOwner1173(t *testing.T) {
	const wt = 100 * time.Millisecond
	clk := clock1173(t)
	rig := newStallRig704(t)
	w := rig.w
	w.cfg.WriteTimeout = wt
	w.cfg.IdleTimeout = time.Hour
	w.cfg.ReadTimeout = time.Hour

	rig.cs.writeBuf = append(rig.cs.writeBuf[:0], "a response"...)
	if w.flushSend(rig.cs) {
		t.Fatal("apparatus: flushSend found no SQE")
	}
	rig.cs.generation++ // the descriptor number now belongs to another conn, still sending
	clk.Advance(2 * wt)
	w.checkTimeouts()
	if !cut1173(rig) {
		t.Fatal("a SEND SQE of an earlier owner of the descriptor number restarted the write clock of the conn that holds it now")
	}
}

// The scan follows the ring's counters across their wrap, as the kernel's head
// and our tail do: 32-bit sequence numbers, indexed by the ring's mask.
func TestRestampFindsASendAcrossAWrappedRing1173(t *testing.T) {
	const wt = 100 * time.Millisecond
	clk := clock1173(t)
	rig := newStallRig704(t)
	w := rig.w
	w.cfg.WriteTimeout = wt
	w.cfg.IdleTimeout = time.Hour
	w.cfg.ReadTimeout = time.Hour

	// head == tail == 0xFFFFFFFE: the SEND takes that sequence number, two more
	// SQEs follow it, and the tail wraps to 1 (the slots wrap with the mask too).
	const start = uint32(0xFFFFFFFE)
	atomic.StoreUint32((*uint32)(w.ring.sqHead), start)
	atomic.StoreUint32((*uint32)(w.ring.sqTail), start)
	rig.cs.writeBuf = append(rig.cs.writeBuf[:0], "a response"...)
	if w.flushSend(rig.cs) {
		t.Fatal("apparatus: flushSend found no SQE")
	}
	for range 2 { // two more SQEs after the SEND
		if w.ring.GetSQE() == nil {
			t.Fatal("apparatus: no SQE")
		}
	}
	if tail := w.ring.sqPlaced(); tail != 1 {
		t.Fatalf("apparatus: tail %#x, want 1 (wrapped)", tail)
	}
	clk.Advance(2 * wt)
	w.checkTimeouts()
	if cut1173(rig) {
		t.Fatal("a SEND staged in the ring across the wrap of its 32-bit counters was not found by the sweep")
	}
	// The kernel consumes everything (head catches up with the tail): cut at WT.
	atomic.StoreUint32((*uint32)(w.ring.sqHead), w.ring.sqPlaced())
	clk.Advance(wt / 2)
	sweep1173(w)
	if cut1173(rig) {
		t.Fatal("cut at WT/2 after the restamp")
	}
	clk.Advance(wt)
	sweep1173(w)
	if !cut1173(rig) {
		t.Fatal("a SEND the kernel holds was not cut after WriteTimeout")
	}
}

// A detached conn (WebSocket, SSE) has no lastActivity write clock: its peer's
// progress is stallSince, which a SEND completion restamps and the sweep tests
// against WriteTimeout. Its next chunk sits in the SQ ring the same way, and a
// worker stall in the pass must not count against the peer here either. The
// descriptor is a unix socket, so the sweep has no tcp_info evidence of the peer
// reading (as for a fixed-file conn): the clock is all there is.
func TestDetachedConnWithASendInTheSQRingIsNotCutByAStall1173(t *testing.T) {
	const wt = 100 * time.Millisecond
	clk := clock1173(t)
	rig := newStallRig704(t)
	w := rig.w
	cs := rig.cs
	w.cfg.WriteTimeout = wt
	w.cfg.IdleTimeout = time.Hour
	w.cfg.ReadTimeout = time.Hour
	cs.h1State.Detached.Store(true)
	cs.h1State.WSReady.Store(true)

	// The peer took bytes at T0 (completeSend stamps the clock) and the next
	// chunk is staged in the same pass.
	cs.stallSince = w.cachedNow
	cs.closeSampled = w.cachedNow
	cs.writeBuf = append(cs.writeBuf[:0], "a chunk"...)
	if w.flushSend(cs) {
		t.Fatal("apparatus: flushSend found no SQE")
	}
	sweepDetached := func() {
		w.lastStallLook = 0
		sweep1173(w)
	}
	clk.Advance(2 * wt)
	w.lastStallLook = 0
	w.checkTimeouts()
	if cut1173(rig) {
		t.Fatalf("a detached conn was cut for a stall of %v of the worker, its chunk still in our SQ ring (WriteTimeout %v)", 2*wt, wt)
	}
	if _, err := w.ring.Submit(); err != nil {
		t.Fatalf("apparatus: submit: %v", err)
	}
	clk.Advance(wt / 2)
	sweepDetached()
	if cut1173(rig) {
		t.Fatal("cut at WT/2 after the stall")
	}
	clk.Advance(wt)
	sweepDetached()
	if !cut1173(rig) {
		t.Fatal("a detached peer that took nothing for 1.5 WT after the SEND was submitted was not cut")
	}
}
