//go:build linux

package iouring

// celeris#657 P9 at the unit level, on the fd_lifetime fixture: the kernel is
// out of the loop, so every ordering is the test's own.
//
// Two claims. The sweep must reach a connection that produces no completion
// at all — the idle keep-alive a revert finds — and it must reach it THROUGH
// the ordinary hand-off, so PR-2's rule still holds: the connection leaves at
// its own recv's -ECANCELED, never with that recv still armed. And on a
// kernel whose cancel flags the startup probe did not find, where no reap can
// be placed, the sweep must not ask at all: a reap counted on every pass for
// as long as the drain lasted is the spin the dormancy rule exists to avoid.

import (
	"testing"

	"golang.org/x/sys/unix"
)

// forceIOUPass makes the next w.sweep() run a pass regardless of cadence.
func forceIOUPass(w *Worker) { w.sweepNext = 0 }

func TestQuiesceMovesAnIdleConn(t *testing.T) {
	f := newFDLFixture(t, false)
	f.armFirstRecv()
	f.serveOne() // the state a revert finds: response flushed, next recv armed

	f.startDrain()
	// No completion will arrive for this conn. Only the sweep can reach it.
	f.w.sweep()

	sqes := takeSQEs(f.w.ring)
	if len(sqes) != 1 || !f.isReap(sqes[0]) {
		t.Fatalf("celeris657 QUIESCE: the sweep placed %v, want exactly one reported cancel of the conn's own "+
			"recv. Nothing else examines a conn that sends nothing", sqes)
	}
	if got := f.tgt.adopted.Load(); got != 0 {
		t.Fatalf("celeris657 QUIESCE R0: the sweep handed the conn over with its recv still armed (%d adopts). "+
			"The sweep must go through the same gate as the event path", got)
	}
	if !f.cs.recvArmed {
		t.Fatalf("celeris657 QUIESCE PREMISE: the conn's recv is not armed")
	}
	if n := metric(t, f.e, "TransplantReaps"); n != 1 {
		t.Errorf("celeris657 QUIESCE: TransplantReaps = %d after one sweep pass, want 1", n)
	}

	// The reap lands: the recv is gone, read nothing, and the hand-off runs.
	f.process(f.recvCQE(-int32(unix.ECANCELED)))
	if got := f.tgt.adopted.Load(); got != 1 {
		t.Errorf("celeris657 QUIESCE: %d adopts after the reaped recv completed, want 1", got)
	}
	if n := metric(t, f.e, "TransplantHandoffInFlight"); n != 0 {
		t.Errorf("celeris657 QUIESCE W2: %d hand-offs were made with an op in flight, want 0", n)
	}
	if n := metric(t, f.e, "TransplantReapUnsupported"); n != 0 {
		t.Errorf("celeris657 QUIESCE: TransplantReapUnsupported = %d on a worker whose probe found the flags", n)
	}
	if fdIsOpen(f.fd) {
		t.Errorf("celeris657 QUIESCE: the original descriptor is still open after the hand-off")
	}
}

// TestSweepDoesNotSpinWithoutCancelFlags is the no-spin rule. A worker whose
// startup probe did not find IORING_ASYNC_CANCEL flags accepted cannot reap,
// so an idle connection's armed recv can only be cleared by the connection's
// own next request. The sweep must class it as permanent residue and go
// dormant, not ask on every pass.
func TestSweepDoesNotSpinWithoutCancelFlags(t *testing.T) {
	f := newFDLFixture(t, false)
	if !trySetWorkerField(f.w, "asyncCancelFlags", false) {
		t.Skip("this tree has no asyncCancelFlags probe")
	}
	f.armFirstRecv()
	f.serveOne()
	f.startDrain()

	for i := 0; i < 5; i++ {
		forceIOUPass(f.w)
		f.w.sweep()
	}
	if sqes := takeSQEs(f.w.ring); len(sqes) != 0 {
		t.Errorf("celeris657 NOSPIN: five sweep passes on a worker that cannot reap placed %v, want nothing", sqes)
	}
	if n := metric(t, f.e, "TransplantReapUnsupported"); n != 0 {
		t.Errorf("celeris657 NOSPIN: TransplantReapUnsupported = %d after five passes. A reap the worker cannot "+
			"place must not be attempted once per pass for as long as the drain lasts", n)
	}
	if !f.w.sweepDormant {
		t.Errorf("celeris657 NOSPIN: the sweep is still awake. A conn whose recv only its own next request can " +
			"clear is permanent residue on this worker")
	}
	if _, owed := f.w.sweepWait(); owed {
		t.Errorf("celeris657 NOSPIN: the sweep still caps the ring wait while dormant")
	}
	if got := f.tgt.adopted.Load(); got != 0 {
		t.Errorf("celeris657 NOSPIN: %d conns were handed over with a recv nothing can cancel, want 0", got)
	}
}

// TestSweepSkipsAConnWithWorkAlreadyOwed pins the other half of the no-spin
// rule: a conn whose reap is already on its way, or whose response is held
// for the hand-off at its SEND completion, must not be examined again — a
// second reap on the same recv is exactly the double-arm class of #484/#596.
func TestSweepSkipsAConnWithWorkAlreadyOwed(t *testing.T) {
	f := newFDLFixture(t, false)
	f.armFirstRecv()
	f.serveOne()
	f.startDrain()

	f.w.sweep()
	if sqes := takeSQEs(f.w.ring); len(sqes) != 1 || !f.isReap(sqes[0]) {
		t.Fatalf("celeris657 OWED PREMISE: the first pass placed %v, want one reap", sqes)
	}
	if f.cs.transplantReap == 0 {
		t.Fatalf("celeris657 OWED PREMISE: the conn carries no outstanding reap")
	}
	for i := 0; i < 4; i++ {
		forceIOUPass(f.w)
		f.w.sweep()
	}
	if sqes := takeSQEs(f.w.ring); len(sqes) != 0 {
		t.Errorf("celeris657 OWED: four further passes placed %v while a reap was already outstanding, want "+
			"nothing", sqes)
	}
	if n := metric(t, f.e, "TransplantReaps"); n != 1 {
		t.Errorf("celeris657 OWED: TransplantReaps = %d, want the 1 from the first pass", n)
	}
}

// TestSweepLeavesADetachedConnAlone is the celeris#667/#672 interaction. A
// detached WebSocket or SSE connection is a promoted async connection whose
// hand-off is refused for good, so the sweep must neither examine it nor
// Broadcast its dispatch goroutine — a spurious wake into the chanReader's
// own pause/resume machinery — and must count it as the permanent residue it
// is, so a standby worker holding WS connections goes dormant instead of
// sweeping for as long as the drain lasts.
func TestSweepLeavesADetachedConnAlone(t *testing.T) {
	f := newFDLFixture(t, true)
	f.armFirstRecv()
	f.serveOne()
	f.cs.asyncPromoted.Store(true)
	f.cs.asyncRun = true
	if f.cs.h1State == nil {
		t.Fatal("celeris657 WSRESIDUE PREMISE: the fixture conn has no H1 state")
	}
	f.cs.h1State.Detached.Store(true)
	f.startDrain()

	for i := 0; i < 4; i++ {
		forceIOUPass(f.w)
		f.w.sweep()
	}
	if sqes := takeSQEs(f.w.ring); len(sqes) != 0 {
		t.Errorf("celeris657 WSRESIDUE: the sweep placed %v for a detached conn, want nothing", sqes)
	}
	if f.cs.sweepKick != nil {
		t.Errorf("celeris657 WSRESIDUE: the sweep Broadcast a detached conn's dispatch goroutine; its hand-off " +
			"can never be offered, and the wake lands in the WebSocket chanReader's pause/resume path")
	}
	if !f.w.sweepDormant {
		t.Errorf("celeris657 WSRESIDUE: the sweep is still awake with only a detached conn left; it would " +
			"re-examine it for as long as the drain lasted")
	}
	if got := f.tgt.adopted.Load(); got != 0 {
		t.Errorf("celeris657 WSRESIDUE: %d detached conns were handed over, want 0", got)
	}
}
