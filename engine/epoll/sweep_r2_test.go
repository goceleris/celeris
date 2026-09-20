//go:build linux

package epoll

// celeris#657 PR-3 round 2: the defects the two round-1 reviews found in the
// sweep and the park-boundary ask, each pinned by a test that fails on
// 9869f7e (the round-1 head) for the reason the review gave.
//
//	MAJOR-1  a cycle could go dormant without examining a connection that
//	         joined it after the cursor had passed the tail.
//	MAJOR-2  the residual classifier read dispatch-goroutine-owned fields
//	         with no lock (the celeris#256/#548 TOCTOU class).
//	MINOR-a  the park-boundary ask had no permanent-class pre-check.
//	MINOR-b  dropAsk could not withdraw an ask from the batch already
//	         detached from the queue, and did not keep the pending count.
//	MINOR-d  the residual gauges could stand for connections the loop no
//	         longer held, including after shutdown.
//	MINOR-e  a connection that had sent no byte was classed transient, so
//	         the sweep could never go dormant while one was on the loop.

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/internal/conn"
)

// permanentConn registers a connection that both heads class as PERMANENT
// residue: a detached (WebSocket/SSE) HTTP/1 conn. It needs no socket — the
// sweep never reaches tryTransplant for permanent residue — so a cycle of a
// few hundred of them costs no descriptors.
func permanentConn(l *Loop, fd int) *connState {
	cs := &connState{fd: fd, liveIdx: -1}
	cs.protocol = engine.HTTP1
	cs.detected = true
	cs.h1State = conn.NewH1State()
	cs.h1State.Detached.Store(true)
	l.conns[fd] = cs
	l.addLiveConn(cs)
	return cs
}

// asyncConn registers a promoted async connection with a REAL detachMu, the
// way acquireConnState builds one — the shape whose h1State/h2State/protocol
// a dispatch goroutine owns.
func asyncConn(t *testing.T, l *Loop) (int, *connState) {
	t.Helper()
	fd := socketpairFD(t, l)
	cs := acquireConnState(context.Background(), fd, 64, true)
	cs.liveIdx = -1
	cs.protocol = engine.HTTP1
	cs.detected = true
	cs.h1State = conn.NewH1State()
	cs.asyncPromoted = true
	l.conns[fd] = cs
	l.addLiveConn(cs)
	l.connCount++
	l.activeConns.Add(1)
	if cs.detachMu == nil {
		t.Fatal("celeris657 PREMISE: acquireConnState(async) left detachMu nil; the lock this test is about " +
			"does not exist")
	}
	return fd, cs
}

// TestSweepCannotGoDormantWithAnUnexaminedArrival is MAJOR-1, in the review's
// own numbers: 261 connections and a budget of 256.
//
// Pass 1 examines indices 260..5 and leaves the cursor at 4. A connection is
// then ADOPTED — appended at index 261, past the cursor, where this cycle's
// backward walk can never reach it. Pass 2 walks 4..0 and completes the
// cycle. On 9869f7e the cycle's verdict is computed from cycleMoved and
// cycleTransient, to neither of which the new connection contributed, so the
// sweep goes dormant and the connection stays on the outgoing engine for the
// rest of the drain — the exact failure the sweep exists to fix.
func TestSweepCannotGoDormantWithAnUnexaminedArrival(t *testing.T) {
	l, _ := sweepLoop(t)
	tgt := &countingTarget{}
	defer tgt.closeAll()
	const residue = 261 // sweepBudget + 5, the review's trace
	if residue != sweepBudget+5 {
		t.Fatalf("celeris657 ARRIVAL PREMISE: the trace is written for budget %d, not %d", residue-5, sweepBudget)
	}
	for i := 0; i < residue; i++ {
		permanentConn(l, 600+i)
	}
	l.transplant.Store(&transplantState{target: tgt})

	l.sweep() // pass 1
	if l.sweepCursor != residue-1-sweepBudget {
		t.Fatalf("celeris657 ARRIVAL PREMISE: after pass 1 the cursor is %d, want %d (%d conns, budget %d)",
			l.sweepCursor, residue-1-sweepBudget, residue, sweepBudget)
	}
	if l.sweepDormant {
		t.Fatalf("celeris657 ARRIVAL PREMISE: the sweep went dormant after one budgeted pass")
	}

	// The arrival, mid-cycle, at the tail: an adopt from the other engine,
	// or an accept. It is movable, so the only reason it can be left behind
	// is that nothing ever looked at it.
	fd, cs := movableConn(t, l)
	if cs.liveIdx <= l.sweepCursor {
		t.Fatalf("celeris657 ARRIVAL PREMISE: the arrival landed at index %d, at or below the cursor %d; the "+
			"trace needs it past the cursor", cs.liveIdx, l.sweepCursor)
	}

	// Run the sweep until it settles. The cycle rule may cost extra cycles;
	// it may not cost an unexamined connection.
	const budgetOfPasses = 16
	passes := 0
	for ; passes < budgetOfPasses && !l.sweepDormant; passes++ {
		forcePass(l)
		l.sweep()
	}
	if l.conns[fd] == cs {
		t.Errorf("celeris657 ARRIVAL: the sweep settled (dormant=%v) after %d passes with the connection that "+
			"joined at index %d still on this loop. It arrived past the cursor at %d, so the cycle in progress "+
			"could not reach it, and the cycle's dormancy verdict was computed without it: it stays on the "+
			"outgoing engine for the rest of the drain",
			l.sweepDormant, passes, residue, residue-1-sweepBudget)
	}
	if got := tgt.count(); got != 1 {
		t.Errorf("celeris657 ARRIVAL: %d conns were handed over, want the 1 that arrived mid-cycle", got)
	}
	if !l.sweepDormant {
		t.Errorf("celeris657 ARRIVAL: the sweep was still awake after %d passes with only permanent residue "+
			"left. The cycle rule must cost a bounded number of extra cycles, not dormancy itself", passes)
	}
}

// TestSweepCostIsBoundedUnderContinuousArrivals is the other half of MAJOR-1:
// the rule that stops a cycle from going dormant must not become a spin. One
// pass examines at most sweepBudget connections and always returns, whatever
// arrives between passes, and the cadence is untouched.
func TestSweepCostIsBoundedUnderContinuousArrivals(t *testing.T) {
	l, cnt := sweepLoop(t)
	tgt := &countingTarget{}
	defer tgt.closeAll()
	for i := 0; i < 8; i++ {
		permanentConn(l, 1200+i)
	}
	l.transplant.Store(&transplantState{target: tgt})

	const rounds = 64
	next := 1300
	for i := 0; i < rounds; i++ {
		permanentConn(l, next) // an arrival before every pass, for ever
		next++
		forcePass(l)
		start := time.Now()
		l.sweep()
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("celeris657 BOUNDED: one sweep pass took %v; the walk did not terminate", d)
		}
	}
	// One pass per call, never more: the arrival flag defers dormancy, it
	// does not restart the walk inside a call.
	if n := cnt.passes.Load(); n != rounds {
		t.Errorf("celeris657 BOUNDED: %d passes ran for %d sweep() calls, want one each. An arrival must not "+
			"make a call walk the set more than once", n, rounds)
	}
	if l.sweepIvl != sweepMaxIvl {
		t.Errorf("celeris657 BOUNDED: the cadence settled at %v, want the %v cap. Continuous arrivals must "+
			"back the sweep off exactly as continuous transient residue does",
			time.Duration(l.sweepIvl), time.Duration(sweepMaxIvl))
	}
	if l.sweepDormant {
		t.Errorf("celeris657 BOUNDED PREMISE: the sweep went dormant while connections were still arriving")
	}
	// And it does settle the moment they stop.
	for i := 0; i < 8 && !l.sweepDormant; i++ {
		forcePass(l)
		l.sweep()
	}
	if !l.sweepDormant {
		t.Errorf("celeris657 BOUNDED: the sweep never went dormant after the arrivals stopped")
	}
	if got := tgt.count(); got != 0 {
		t.Errorf("celeris657 BOUNDED: %d permanent-residue conns were handed over, want 0", got)
	}
}

// TestSweepClassifiesUnderTheLockTheEngineRequires is MAJOR-2, under -race.
//
// switchToH2Local (loop.go) runs on the per-conn dispatch goroutine and, under
// cs.detachMu, does three writes: CloseH1 + h1State = nil, h2State = the new
// state, protocol = H2C. residualClass read all three from the loop thread
// with no lock at all, once per live connection per pass — the TOCTOU this
// engine's own docstrings forbid in three places (iouring/worker.go 2586,
// 3035, 5117). The goroutine below performs exactly those writes under exactly
// that lock while the loop sweeps; on 9869f7e the race detector reports the
// read against them.
func TestSweepClassifiesUnderTheLockTheEngineRequires(t *testing.T) {
	l, _ := sweepLoop(t)
	l.async = true
	tgt := &countingTarget{}
	defer tgt.closeAll()
	_, cs := asyncConn(t, l)
	// RUNNING, not parked: the state a connection is actually in while its
	// dispatch goroutine is inside ProcessH1, which is the only state
	// switchToH2Local runs in. tryTransplant bows out at its own async gate
	// for such a connection (transplant.go: !parked → return), so the
	// classifier is the only thing that reads the three fields, which is the
	// claim under test.
	cs.asyncRun = true
	cs.asyncParked = false
	l.transplant.Store(&transplantState{target: tgt})

	stop := make(chan struct{})
	var flips atomic.Int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // the dispatch goroutine, at an h2c upgrade
		defer wg.Done()
		h1 := cs.h1State
		for {
			select {
			case <-stop:
				cs.detachMu.Lock()
				cs.h1State = h1
				cs.h2State = nil
				cs.protocol = engine.HTTP1
				cs.detachMu.Unlock()
				return
			default:
			}
			// switchToH2Local's writes, in its order, under its lock.
			cs.detachMu.Lock()
			cs.h1State = nil
			cs.h2State = &conn.H2State{}
			cs.protocol = engine.H2C
			cs.detachMu.Unlock()
			cs.detachMu.Lock()
			cs.h1State = h1
			cs.h2State = nil
			cs.protocol = engine.HTTP1
			cs.detachMu.Unlock()
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
		forcePass(l)
		l.sweep()
		passes++
		runtime.Gosched() // never starve the upgrade goroutine this test races
	}
	close(stop)
	wg.Wait()
	if got := flips.Load(); got < wantFlips {
		t.Fatalf("celeris657 XFERRACE PREMISE: the upgrade goroutine ran %d times in %d passes, want %d; the "+
			"injection did not fire often enough for the read and the write to interleave", got, passes, wantFlips)
	}
	t.Logf("celeris657 XFERRACE passes=%d upgrade_flips=%d", passes, flips.Load())
}

// TestParkAskSkipsWhatTheHandoffCannotAccept is MINOR-a. While a drain is set,
// 9869f7e's park-boundary ask fires for EVERY promoted async connection at
// EVERY park — one queue entry and one eventfd write each — including detached
// WebSocket/SSE connections, which park once per delivered frame, and
// H2-bound ones. tryTransplant refuses all of them on gates that cannot change
// while the connection lives. The sweep has this pre-check; the ask must too.
func TestParkAskSkipsWhatTheHandoffCannotAccept(t *testing.T) {
	l, _ := sweepLoop(t)
	l.async = true
	tgt := &countingTarget{}
	defer tgt.closeAll()

	_, movable := asyncConn(t, l)
	_, detached := asyncConn(t, l)
	detached.h1State.Detached.Store(true)
	_, h2bound := asyncConn(t, l)
	h2bound.h2State = &conn.H2State{}
	_, promoted := asyncConn(t, l)
	promoted.asyncH2Promoted.Store(true)

	// No drain: nobody asks at all.
	for _, cs := range []*connState{movable, detached, h2bound, promoted} {
		l.askAtPark(cs)
	}
	if n := l.askQueueLen(); n != 0 {
		t.Fatalf("celeris657 ASKGATE PREMISE: %d asks were queued with no drain set, want 0", n)
	}

	l.transplant.Store(&transplantState{target: tgt})
	for _, cs := range []*connState{detached, h2bound, promoted} {
		for i := 0; i < 3; i++ { // three parks each, as a WS conn does per frame
			l.askAtPark(cs)
		}
	}
	if n := l.askQueueLen(); n != 0 {
		t.Errorf("celeris657 ASKGATE: %d asks were queued for connections the hand-off can never accept "+
			"(detached WS/SSE, h2State set, asyncH2Promoted). Each is an eventfd write and a queue entry the "+
			"loop must walk, once per park, for as long as the drain lasts", n)
	}
	for _, cs := range []*connState{detached, h2bound, promoted} {
		if cs.xferAsked.Load() {
			t.Errorf("celeris657 ASKGATE: a connection the hand-off refuses is marked as having asked")
		}
	}

	// The control: the connection the hand-off CAN accept still asks, once.
	l.askAtPark(movable)
	l.askAtPark(movable)
	if n := l.askQueueLen(); n != 1 {
		t.Errorf("celeris657 ASKGATE: %d asks queued for a movable conn parked twice, want exactly 1 (the CAS "+
			"deduplicates)", n)
	}
}

// askQueueLen is the number of live (non-withdrawn) entries the loop still has
// to look at, across both queues. Loop thread, test only.
func (l *Loop) askQueueLen() int {
	n := 0
	l.xferAskMu.Lock()
	for _, cs := range l.xferAskQ {
		if cs != nil {
			n++
		}
	}
	l.xferAskMu.Unlock()
	for _, cs := range l.xferAskDrain {
		if cs != nil {
			n++
		}
	}
	return n
}

// dropTarget is a hand-off target whose AdoptConn runs a callback on the loop
// thread — which is where AdoptConn really runs, and where every teardown path
// runs too.
type dropTarget struct {
	countingTarget
	onAdopt func()
}

func (d *dropTarget) AdoptConn(fd int, c engine.Carryover) error {
	err := d.countingTarget.AdoptConn(fd, c)
	if d.onAdopt != nil {
		f := d.onAdopt
		d.onAdopt = nil
		f()
	}
	return err
}

// TestDropAskWithdrawsFromTheBatchBeingDrained is MINOR-b, with the celeris#654
// hazard made concrete.
//
// drainTransplantAsks detaches the whole queue and then walks it. On 9869f7e
// that batch is a local slice, so dropAsk — which every release path calls —
// cannot reach it: an entry for a connState released DURING the walk survives,
// and when the walk reaches it the connState has been pooled and handed to
// another connection, which the loop then hands off without that connection
// ever having asked.
//
// The release here happens inside the target's AdoptConn, which is loop-thread
// code reached from inside the walk, and the pooled connState is re-installed
// on a different descriptor by hand, so the test does not depend on what
// sync.Pool chooses to return.
func TestDropAskWithdrawsFromTheBatchBeingDrained(t *testing.T) {
	l, _ := sweepLoop(t)
	tgt := &dropTarget{}
	defer tgt.closeAll()
	l.transplant.Store(&transplantState{target: tgt})

	fdA, csA := movableConn(t, l) // walked first: it is queued first
	fdB, csB := movableConn(t, l) // released while A is being handed over
	fdC := socketpairFD(t, l)     // the connection csB's memory is handed to
	t.Cleanup(func() { _ = unix.Close(fdC) })

	csA.xferAsked.Store(true)
	l.askTransplant(csA)
	csB.xferAsked.Store(true)
	l.askTransplant(csB)

	tgt.onAdopt = func() {
		// B goes away on the loop thread, the way every teardown path does:
		// dropAsk, then the pool.
		l.detachFromEpoll(fdB, csB)
		l.dropAsk(csB)
		releaseConnState(csB)
		// ...and the pool hands that connState to the next connection.
		// Modelled exactly: the SAME pointer, a different descriptor, live
		// on this loop, never having asked for anything.
		csB.fd = fdC
		csB.liveIdx = -1
		csB.protocol = engine.HTTP1
		csB.detected = true
		csB.h1State = conn.NewH1State()
		l.conns[fdC] = csB
		l.addLiveConn(csB)
	}

	l.drainTransplantAsks()

	if l.conns[fdC] != csB {
		t.Errorf("celeris657 ASKBATCH: the loop handed over the connection on fd %d. It never asked: the ask "+
			"that named it was queued by the PREVIOUS owner of that connState, dropAsk could not withdraw it "+
			"from the batch already detached from the queue, and the slot check passed because the pool had "+
			"handed the memory on (celeris#654)", fdC)
	}
	if got := tgt.count(); got != 1 {
		t.Errorf("celeris657 ASKBATCH: %d hand-offs, want exactly 1 (conn A, which asked)", got)
	}
	if n := l.askQueueLen(); n != 0 {
		t.Errorf("celeris657 ASKBATCH: %d asks still queued after the drain, want 0", n)
	}
	if l.conns[fdA] != nil {
		t.Errorf("celeris657 ASKBATCH PREMISE: conn A was not handed over, so the walk never re-entered a "+
			"release path and the test proved nothing (fd %d)", fdA)
	}
}

// TestAskPendingCountsTheQueue is the invariant askTransplant states, where
// the code can be checked against it: xferAskPending is the number of non-nil
// entries in xferAskQ. On 9869f7e it is a 0/1 flag that dropAsk never
// decrements, so a queue emptied entirely by withdrawals still reads 1.
func TestAskPendingCountsTheQueue(t *testing.T) {
	l, _ := sweepLoop(t)
	tgt := &countingTarget{}
	defer tgt.closeAll()
	l.transplant.Store(&transplantState{target: tgt})

	check := func(when string) {
		t.Helper()
		l.xferAskMu.Lock()
		live := 0
		for _, cs := range l.xferAskQ {
			if cs != nil {
				live++
			}
		}
		l.xferAskMu.Unlock()
		if got := int(l.xferAskPending.Load()); got != live {
			t.Errorf("celeris657 ASKCOUNT %s: xferAskPending = %d, but the queue holds %d non-nil entries. "+
				"The count and the queue are changed under the same lock; they cannot disagree", when, got, live)
		}
	}

	check("empty")
	conns := make([]*connState, 0, 3)
	for i := 0; i < 3; i++ {
		_, cs := movableConn(t, l)
		cs.xferAsked.Store(true)
		l.askTransplant(cs)
		conns = append(conns, cs)
		check("after ask")
	}
	if got := l.xferAskPending.Load(); got != 3 {
		t.Fatalf("celeris657 ASKCOUNT PREMISE: xferAskPending = %d after three asks, want 3", got)
	}
	for i, cs := range conns {
		l.dropAsk(cs)
		check("after withdrawal")
		if i == len(conns)-1 {
			if got := l.xferAskPending.Load(); got != 0 {
				t.Errorf("celeris657 ASKCOUNT: xferAskPending = %d with every ask withdrawn, want 0", got)
			}
		}
	}
	l.drainTransplantAsks()
	check("after drain")
}

// TestResidualGaugeRetractsWhatTheLoopNoLongerHolds is MINOR-d. The gauges are
// a statement about what the engine HOLDS. A dormant sweep publishes nothing,
// and on 9869f7e the dormancy check comes BEFORE the empty-set retraction, so
// a loop whose connections all closed while it slept left its last cycle's
// residue standing for the rest of the drain — and plan step 5 gates the
// nightly on exactly these numbers.
func TestResidualGaugeRetractsWhatTheLoopNoLongerHolds(t *testing.T) {
	l, cnt := sweepLoop(t)
	tgt := &countingTarget{}
	defer tgt.closeAll()
	const held = 3
	held3 := make([]*connState, 0, held)
	for i := 0; i < held; i++ {
		held3 = append(held3, permanentConn(l, 1500+i))
	}
	l.transplant.Store(&transplantState{target: tgt})

	l.sweep()
	if !l.sweepDormant {
		t.Fatalf("celeris657 RETRACT PREMISE: the sweep is awake with only detached conns left")
	}
	if got := cnt.residual[resDetached].Load(); got != held {
		t.Fatalf("celeris657 RETRACT PREMISE: the gauge reads %d, want %d", got, held)
	}

	// One of the three closes while the sweep sleeps.
	l.removeLiveConn(held3[0])
	l.conns[held3[0].fd] = nil
	for i := 0; i < 6 && cnt.residual[resDetached].Load() != held-1; i++ {
		forcePass(l)
		l.sweep()
	}
	if got := cnt.residual[resDetached].Load(); got != held-1 {
		t.Errorf("celeris657 RETRACT: the gauge reads %d after one of %d connections left, want %d. It is a "+
			"gauge of what the engine holds, not of what its last cycle saw", got, held, held-1)
	}

	// Then the rest.
	for _, cs := range held3[1:] {
		l.removeLiveConn(cs)
		l.conns[cs.fd] = nil
	}
	for i := 0; i < 6 && cnt.residual[resDetached].Load() != 0; i++ {
		forcePass(l)
		l.sweep()
	}
	for c := 0; c < numResidual; c++ {
		if got := cnt.residual[c].Load(); got != 0 {
			t.Errorf("celeris657 RETRACT: residual class %d reads %d with nothing left on the loop, want 0", c, got)
		}
	}
}

// TestResidualGaugeRetractsAtShutdown is the other half of MINOR-d: nothing
// retracted a loop's residue when the loop went away, because the sweep does
// not run after shutdown.
func TestResidualGaugeRetractsAtShutdown(t *testing.T) {
	l, cnt := sweepLoop(t)
	l.listenFD = -1
	l.timerFD = -1
	tgt := &countingTarget{}
	defer tgt.closeAll()
	const held = 2
	for i := 0; i < held; i++ {
		fd := socketpairFD(t, l)
		cs := &connState{fd: fd, liveIdx: -1}
		cs.protocol = engine.HTTP1
		cs.detected = true
		cs.h1State = conn.NewH1State()
		cs.h1State.Detached.Store(true)
		l.conns[fd] = cs
		l.addLiveConn(cs)
	}
	l.transplant.Store(&transplantState{target: tgt})
	l.sweep()
	if got := cnt.residual[resDetached].Load(); got != held {
		t.Fatalf("celeris657 SHUTRETRACT PREMISE: the gauge reads %d, want %d", got, held)
	}

	l.shutdown()

	for c := 0; c < numResidual; c++ {
		if got := cnt.residual[c].Load(); got != 0 {
			t.Errorf("celeris657 SHUTRETRACT: residual class %d reads %d after the loop shut down, want 0. A "+
				"loop that is gone holds nothing, and nothing else ever retracts what it published", c, got)
		}
	}
}

// TestSweepGoesDormantOnAConnThatHasSentNothing is MINOR-e. A connection that
// has been accepted and has sent no byte has cs.detected == false, which is
// tryTransplant's very first gate. On 9869f7e the classifier called that
// TRANSIENT residue, so cycleTransient was bumped on every cycle, the sweep
// never went dormant, epoll_wait stayed capped at the 64 ms back-off for the
// whole drain, and TransplantResidualBusy — the gauge whose standing non-zero
// value IS the placement bug — read non-zero after the switch had settled.
func TestSweepGoesDormantOnAConnThatHasSentNothing(t *testing.T) {
	l, cnt := sweepLoop(t)
	tgt := &countingTarget{}
	defer tgt.closeAll()
	fd := socketpairFD(t, l)
	cs := regLive(l, fd)
	l.connCount++
	l.activeConns.Add(1)
	if cs.detected {
		t.Fatal("celeris657 UNSTARTED PREMISE: the fixture conn is already detected")
	}
	l.transplant.Store(&transplantState{target: tgt})

	for i := 0; i < 6 && !l.sweepDormant; i++ {
		forcePass(l)
		l.sweep()
	}
	if !l.sweepDormant {
		t.Errorf("celeris657 UNSTARTED: the sweep is still awake with one accepted-but-silent connection left. " +
			"It cannot be handed over until it speaks, and when it speaks the per-event call site examines it: " +
			"re-examining it in between is work with no outcome, and it pins epoll_wait at the back-off cap")
	}
	if got := cnt.residual[resBusy].Load(); got != 0 {
		t.Errorf("celeris657 UNSTARTED: TransplantResidualBusy = %d for a connection that has sent nothing. "+
			"Busy is the transient class and a standing non-zero Busy after a switch settles is the placement "+
			"bug itself; this connection is not it", got)
	}
	// R2BASE-DROP-BEGIN resUnstarted does not exist on 9869f7e
	if got := cnt.residual[resUnstarted].Load(); got != 1 {
		t.Errorf("celeris657 UNSTARTED: TransplantResidualUnstarted = %d, want 1", got)
	}
	// R2BASE-DROP-END
	if ms := l.sweepWaitMs(); ms != -1 {
		t.Errorf("celeris657 UNSTARTED: sweepWaitMs = %d, want -1: a dormant sweep must not cap epoll_wait", ms)
	}
	if got := tgt.count(); got != 0 {
		t.Errorf("celeris657 UNSTARTED: %d connections that had sent nothing were handed over, want 0", got)
	}

	// And it is still examined the moment it does speak: the per-event call
	// site is untouched by dormancy.
	cs.protocol = engine.HTTP1
	cs.detected = true
	cs.h1State = conn.NewH1State()
	l.tryTransplant(fd)
	if got := tgt.count(); got != 1 {
		t.Errorf("celeris657 UNSTARTED: the event path handed over %d conns once the connection spoke, want 1", got)
	}
}
