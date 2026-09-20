//go:build linux

package iouring

// celeris#657 PR-3 round 2, io_uring half. The two MAJOR defects the reviews
// found are shared with the epoll sweep (engine/epoll/sweep_r2_test.go states
// them); these are the same claims on the worker thread, plus the two
// worker-side classifier defects.
//
//	MAJOR-1  a cycle could go dormant without examining a connection that
//	         joined it after the cursor had passed the tail.
//	MAJOR-2  residualClass read dispatch-goroutine-owned fields with no lock.
//	MINOR-d  the residual gauges could stand for connections the worker no
//	         longer held.
//	MINOR-e  a connection that had sent no byte was classed transient.

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/conn"
)

// synthConns registers n connections that both heads class as PERMANENT
// residue — fixed-file conns, which residualClass refuses before it reads
// anything — at conn-table indices the fixture's own descriptor cannot
// collide with. They hold no real descriptor, so the fixture's cleanup must
// not try to close them.
func synthConns(t *testing.T, w *Worker, base, n int) []*connState {
	t.Helper()
	if base+n > len(w.conns) {
		t.Fatalf("celeris657 PREMISE: conn table holds %d entries, need %d", len(w.conns), base+n)
	}
	out := make([]*connState, 0, n)
	for i := 0; i < n; i++ {
		fd := base + i
		cs := &connState{fd: fd, liveIdx: -1, fixedFile: true}
		w.conns[fd] = cs
		w.addLiveConn(cs)
		out = append(out, cs)
	}
	t.Cleanup(func() { // before the fixture's cleanup (LIFO): these own no fd
		for i := 0; i < n; i++ {
			w.conns[base+i] = nil
		}
	})
	return out
}

// TestSweepCannotGoDormantWithAnUnexaminedArrival is MAJOR-1 on the worker
// thread, in the review's numbers: 261 connections and a budget of 256.
//
// Pass 1 examines indices 260..5 and leaves the cursor at 4. A connection is
// then adopted — appended at index 261, past the cursor, unreachable by this
// cycle's backward walk. Pass 2 walks 4..0, completes the cycle, and on
// 9869f7e judges dormancy on counters the new connection never contributed
// to: it sleeps with that connection unexamined, and nothing else on a
// standby worker will ever look at it.
func TestSweepCannotGoDormantWithAnUnexaminedArrival(t *testing.T) {
	f := newFDLFixture(t, false)
	f.w.sweepCnt = &f.e.metrics.sweep
	f.armFirstRecv()
	f.serveOne() // the state a revert finds: response flushed, recv armed

	// The fixture's conn is the ARRIVAL: out of the live set for now.
	f.w.removeLiveConn(f.cs)
	const residue = 261
	if residue != iouSweepBudget+5 {
		t.Fatalf("celeris657 ARRIVAL PREMISE: the trace is written for budget %d, not %d",
			residue-5, iouSweepBudget)
	}
	synthConns(t, f.w, 500, residue)
	if n := len(f.w.liveConns); n != residue {
		t.Fatalf("celeris657 ARRIVAL PREMISE: %d live conns, want %d", n, residue)
	}
	f.startDrain()

	f.w.sweep() // pass 1
	if f.w.sweepCursor != residue-1-iouSweepBudget {
		t.Fatalf("celeris657 ARRIVAL PREMISE: after pass 1 the cursor is %d, want %d",
			f.w.sweepCursor, residue-1-iouSweepBudget)
	}
	if f.w.sweepDormant {
		t.Fatalf("celeris657 ARRIVAL PREMISE: the sweep went dormant after one budgeted pass")
	}
	if sqes := takeSQEs(f.w.ring); len(sqes) != 0 {
		t.Fatalf("celeris657 ARRIVAL PREMISE: pass 1 placed %v over permanent residue, want nothing", sqes)
	}

	f.w.addLiveConn(f.cs) // the arrival, at the tail, past the cursor
	if f.cs.liveIdx <= f.w.sweepCursor {
		t.Fatalf("celeris657 ARRIVAL PREMISE: the arrival landed at index %d, at or below the cursor %d",
			f.cs.liveIdx, f.w.sweepCursor)
	}

	// Sweep until the arrival is examined. Being examined here means a REAP
	// placed for its armed recv — the hand-off's own gate, unchanged by any
	// of this — not a hand-off: PR-2's rule is that the connection leaves at
	// that recv's -ECANCELED.
	const budgetOfPasses = 16
	passes, reaped := 0, false
	for ; passes < budgetOfPasses && !reaped; passes++ {
		if f.w.sweepDormant && !reaped {
			break // it has settled; whatever it did is what it will ever do
		}
		forceIOUPass(f.w)
		f.w.sweep()
		if sqes := takeSQEs(f.w.ring); len(sqes) == 1 && f.isReap(sqes[0]) {
			reaped = true
		} else if len(sqes) != 0 {
			t.Fatalf("celeris657 ARRIVAL: a pass placed %v, want nothing or one reap", sqes)
		}
	}
	if !reaped {
		t.Fatalf("celeris657 ARRIVAL: the sweep settled (dormant=%v) after %d passes without ever examining the "+
			"connection that joined at index %d. It arrived past the cursor at %d, so the cycle in progress "+
			"could not reach it and its dormancy verdict was computed without it: on a standby worker with no "+
			"completions, nothing else ever examines it",
			f.w.sweepDormant, passes, residue, residue-1-iouSweepBudget)
	}
	// The reap lands and the hand-off runs; then the sweep must settle again.
	f.process(f.recvCQE(-int32(unix.ECANCELED)))
	if got := f.tgt.adopted.Load(); got != 1 {
		t.Errorf("celeris657 ARRIVAL: %d adopts after the reaped recv completed, want 1", got)
	}
	settle := 0
	for ; settle < budgetOfPasses && !f.w.sweepDormant; settle++ {
		forceIOUPass(f.w)
		f.w.sweep()
	}
	if !f.w.sweepDormant {
		t.Errorf("celeris657 ARRIVAL: the sweep was still awake %d passes after the hand-off completed. The "+
			"cycle rule must cost a bounded number of extra cycles, not dormancy itself", settle)
	}
}

// TestSweepCostIsBoundedUnderContinuousArrivals: the cycle rule must not spin.
// One sweep() call runs one pass whatever arrives between calls, and the
// cadence still backs off to its cap.
func TestSweepCostIsBoundedUnderContinuousArrivals(t *testing.T) {
	f := newFDLFixture(t, false)
	f.w.sweepCnt = &f.e.metrics.sweep
	f.w.removeLiveConn(f.cs)
	synthConns(t, f.w, 500, 8)
	f.startDrain()

	const rounds = 64
	for i := 0; i < rounds; i++ {
		synthConns(t, f.w, 600+i, 1) // an arrival before every pass
		forceIOUPass(f.w)
		start := time.Now()
		f.w.sweep()
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("celeris657 BOUNDED: one sweep pass took %v; the walk did not terminate", d)
		}
	}
	if n := f.e.metrics.sweep.passes.Load(); n != rounds {
		t.Errorf("celeris657 BOUNDED: %d passes ran for %d sweep() calls, want one each", n, rounds)
	}
	if f.w.sweepIvl != iouSweepMaxIvl {
		t.Errorf("celeris657 BOUNDED: the cadence settled at %v, want the %v cap",
			time.Duration(f.w.sweepIvl), time.Duration(iouSweepMaxIvl))
	}
	if f.w.sweepDormant {
		t.Errorf("celeris657 BOUNDED PREMISE: the sweep went dormant while connections were still arriving")
	}
	for i := 0; i < 8 && !f.w.sweepDormant; i++ {
		forceIOUPass(f.w)
		f.w.sweep()
	}
	if !f.w.sweepDormant {
		t.Errorf("celeris657 BOUNDED: the sweep never went dormant after the arrivals stopped")
	}
	if sqes := takeSQEs(f.w.ring); len(sqes) != 0 {
		t.Errorf("celeris657 BOUNDED: the sweep placed %v over fixed-file residue, want nothing", sqes)
	}
}

// TestSweepClassifiesUnderTheLockTheEngineRequires is MAJOR-2, under -race.
//
// switchToH2Local (worker.go 2487, called from runAsyncHandler at 4212) runs
// on the per-conn dispatch goroutine and nils cs.h1State and sets cs.h2State
// under cs.detachMu. residualClass read both from the worker thread with no
// lock, once per live connection per pass — the celeris#256/#548 TOCTOU class
// this file's own docstrings forbid three times (worker.go 2586, 3035, 5117).
// The goroutine below performs exactly those writes under exactly that lock
// while the worker sweeps.
func TestSweepClassifiesUnderTheLockTheEngineRequires(t *testing.T) {
	f := newFDLFixture(t, true)
	f.w.sweepCnt = &f.e.metrics.sweep
	f.armFirstRecv()
	f.serveOne()
	if f.cs.detachMu == nil {
		t.Fatal("celeris657 XFERRACE PREMISE: the async fixture conn has no detachMu; the lock this test is " +
			"about does not exist")
	}
	// Owned by its dispatch goroutine, so tryTransplant itself bows out at
	// the async gate and the classifier is the only reader of the fields.
	f.cs.asyncPromoted.Store(true)
	f.cs.asyncInMu.Lock()
	f.cs.asyncRun = true
	f.cs.asyncInMu.Unlock()
	f.startDrain()

	stop := make(chan struct{})
	var flips atomic.Int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // the dispatch goroutine, at an h2c upgrade
		defer wg.Done()
		h1 := f.cs.h1State
		for {
			select {
			case <-stop:
				f.cs.detachMu.Lock()
				f.cs.h1State = h1
				f.cs.h2State = nil
				f.cs.detachMu.Unlock()
				return
			default:
			}
			f.cs.detachMu.Lock()
			f.cs.h1State = nil
			f.cs.h2State = &conn.H2State{}
			f.cs.detachMu.Unlock()
			f.cs.detachMu.Lock()
			f.cs.h1State = h1
			f.cs.h2State = nil
			f.cs.detachMu.Unlock()
			flips.Add(1)
		}
	}()

	// Drive until the upgrade goroutine has actually run a useful number of
	// times, not for a fixed number of passes: the race detector needs the
	// two to interleave, and a fixed count can starve the goroutine.
	const wantFlips = 200
	deadline := time.Now().Add(20 * time.Second)
	passes := 0
	for flips.Load() < wantFlips && time.Now().Before(deadline) {
		forceIOUPass(f.w)
		f.w.sweep()
		takeSQEs(f.w.ring)
		passes++
	}
	close(stop)
	wg.Wait()
	takeSQEs(f.w.ring)
	if got := flips.Load(); got < wantFlips {
		t.Fatalf("celeris657 XFERRACE PREMISE: the upgrade goroutine ran %d times in %d passes, want %d; the "+
			"injection did not fire often enough for the read and the write to interleave", got, passes, wantFlips)
	}
	t.Logf("celeris657 XFERRACE passes=%d upgrade_flips=%d", passes, flips.Load())
}

// TestSweepGoesDormantOnAConnThatHasSentNothing is MINOR-e on the worker
// thread. cs.detected == false is tryTransplant's first gate
// (transplant_source.go); classing it transient bumped cycleTransient on
// every cycle, so the sweep never went dormant, the ring wait stayed capped
// at the 64 ms back-off for the whole drain, and TransplantResidualBusy read
// non-zero after the switch had settled.
func TestSweepGoesDormantOnAConnThatHasSentNothing(t *testing.T) {
	f := newFDLFixture(t, false)
	f.w.sweepCnt = &f.e.metrics.sweep
	f.w.removeLiveConn(f.cs)
	// An accepted connection that has sent no byte: no protocol detected,
	// no H1 state, no recv completion yet.
	cs := &connState{fd: 500, liveIdx: -1}
	f.w.conns[500] = cs
	f.w.addLiveConn(cs)
	t.Cleanup(func() { f.w.conns[500] = nil })
	f.startDrain()

	for i := 0; i < 6 && !f.w.sweepDormant; i++ {
		forceIOUPass(f.w)
		f.w.sweep()
	}
	if !f.w.sweepDormant {
		t.Errorf("celeris657 UNSTARTED: the sweep is still awake with one accepted-but-silent connection left. " +
			"It cannot be handed over until it speaks, and when it speaks its own recv completion examines it")
	}
	if got := f.e.metrics.sweep.residual[resBusy].Load(); got != 0 {
		t.Errorf("celeris657 UNSTARTED: TransplantResidualBusy = %d for a connection that has sent nothing. "+
			"A standing non-zero Busy after a switch settles is the placement bug itself", got)
	}
	// R2BASE-DROP-BEGIN resUnstarted does not exist on 9869f7e
	if got := f.e.metrics.sweep.residual[resUnstarted].Load(); got != 1 {
		t.Errorf("celeris657 UNSTARTED: TransplantResidualUnstarted = %d, want 1", got)
	}
	// R2BASE-DROP-END
	if _, owed := f.w.sweepWait(); owed {
		t.Errorf("celeris657 UNSTARTED: the sweep still caps the ring wait while dormant")
	}
	if sqes := takeSQEs(f.w.ring); len(sqes) != 0 {
		t.Errorf("celeris657 UNSTARTED: the sweep placed %v for a connection that has sent nothing, want "+
			"nothing", sqes)
	}
}

// TestResidualGaugeRetractsWhatTheWorkerNoLongerHolds is MINOR-d on the
// worker thread: a dormant sweep publishes nothing, and on 9869f7e the
// dormancy check came before the empty-set retraction, so connections that
// left while the sweep slept stayed in the gauge for the rest of the drain.
func TestResidualGaugeRetractsWhatTheWorkerNoLongerHolds(t *testing.T) {
	f := newFDLFixture(t, false)
	f.w.sweepCnt = &f.e.metrics.sweep
	f.w.removeLiveConn(f.cs)
	const held = 3
	cs := synthConns(t, f.w, 500, held)
	f.startDrain()

	f.w.sweep()
	if !f.w.sweepDormant {
		t.Fatalf("celeris657 RETRACT PREMISE: the sweep is awake with only fixed-file conns left")
	}
	if got := f.e.metrics.sweep.residual[resPinned].Load(); got != held {
		t.Fatalf("celeris657 RETRACT PREMISE: the gauge reads %d, want %d", got, held)
	}

	f.w.removeLiveConn(cs[0])
	f.w.conns[cs[0].fd] = nil
	for i := 0; i < 6 && f.e.metrics.sweep.residual[resPinned].Load() != held-1; i++ {
		forceIOUPass(f.w)
		f.w.sweep()
	}
	if got := f.e.metrics.sweep.residual[resPinned].Load(); got != held-1 {
		t.Errorf("celeris657 RETRACT: the gauge reads %d after one of %d connections left, want %d. It is a "+
			"gauge of what the engine holds", got, held, held-1)
	}
	for _, c := range cs[1:] {
		f.w.removeLiveConn(c)
		f.w.conns[c.fd] = nil
	}
	for i := 0; i < 6 && f.e.metrics.sweep.residual[resPinned].Load() != 0; i++ {
		forceIOUPass(f.w)
		f.w.sweep()
	}
	for c := 0; c < numResidual; c++ {
		if got := f.e.metrics.sweep.residual[c].Load(); got != 0 {
			t.Errorf("celeris657 RETRACT: residual class %d reads %d with nothing left on the worker, want 0",
				c, got)
		}
	}
}
