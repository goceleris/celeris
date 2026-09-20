//go:build linux

package epoll

// celeris#657 P7/P8 at the unit level: the sweep's cost controls and the ask
// queue's lifetime rule, driven directly on the loop thread with no kernel
// scheduling in the way.

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/internal/conn"
)

// sweepLoop is newLedgerLoop with the sweep's engine-wide counters wired, a
// real wake eventfd (StartTransplant signals it) and the sweep's own state at
// its zero value.
func sweepLoop(t *testing.T) (*Loop, *sweepCounters) {
	t.Helper()
	l := newLedgerLoop(t)
	cnt := &sweepCounters{}
	l.sweepCnt = cnt
	return l, cnt
}

// movableConn registers a plain HTTP/1 keep-alive at a clean, flushed
// boundary: exactly what tryTransplant accepts.
func movableConn(t *testing.T, l *Loop) (int, *connState) {
	t.Helper()
	fd := socketpairFD(t, l)
	cs := regLive(l, fd)
	cs.protocol = engine.HTTP1
	cs.detected = true
	cs.h1State = conn.NewH1State()
	l.connCount++
	l.activeConns.Add(1)
	return fd, cs
}

// detachedConn is a movable conn turned into a detached (WebSocket/SSE) one:
// permanent residue, the class dormancy is decided on.
func detachedConn(t *testing.T, l *Loop) (int, *connState) {
	t.Helper()
	fd, cs := movableConn(t, l)
	cs.h1State.Detached.Store(true)
	return fd, cs
}

// forcePass makes the next sweep() call run a pass regardless of cadence.
func forcePass(l *Loop) { l.sweepNext = 0 }

func TestSweepStopsWithTheDrain(t *testing.T) {
	l, cnt := sweepLoop(t)
	tgt := &countingTarget{}
	defer tgt.closeAll()
	for i := 0; i < 4; i++ {
		movableConn(t, l)
	}

	// No drain: the sweep must not run a single pass.
	l.sweep()
	if n := cnt.passes.Load(); n != 0 {
		t.Fatalf("celeris657 SWEEPSTOP: %d passes with no drain set, want 0", n)
	}
	if ms := l.sweepWaitMs(); ms != -1 {
		t.Fatalf("celeris657 SWEEPSTOP: sweepWaitMs = %d with no drain set, want -1 (no cap owed)", ms)
	}

	ts := &transplantState{target: tgt}
	l.transplant.Store(ts)
	if ms := l.sweepWaitMs(); ms != 0 {
		t.Errorf("celeris657 SWEEPSTOP: sweepWaitMs = %d for a drain whose first pass is owed, want 0", ms)
	}
	l.sweep()
	if n := cnt.passes.Load(); n != 1 {
		t.Fatalf("celeris657 SWEEPSTOP: %d passes after the drain started, want 1", n)
	}
	if got := tgt.count(); got != 4 {
		t.Fatalf("celeris657 SWEEPSTOP PREMISE: the pass handed over %d of 4 conns", got)
	}

	// The drain stops. Nothing more may run, whatever the cadence says, and
	// no epoll_wait cap may be owed.
	l.transplant.Store(nil)
	for i := 0; i < 3; i++ {
		forcePass(l)
		l.sweep()
	}
	if n := cnt.passes.Load(); n != 1 {
		t.Errorf("celeris657 SWEEPSTOP: %d passes after StopTransplant, want the 1 from before it", n)
	}
	if ms := l.sweepWaitMs(); ms != -1 {
		t.Errorf("celeris657 SWEEPSTOP: sweepWaitMs = %d after StopTransplant, want -1: a stopped drain must "+
			"not pin epoll_wait", ms)
	}
	if l.sweepTS != nil {
		t.Errorf("celeris657 SWEEPSTOP: the sweep still holds a drain epoch after StopTransplant")
	}
	for c, n := range [numResidual]uint64{} {
		_ = n
		if got := cnt.residual[c].Load(); got != 0 {
			t.Errorf("celeris657 SWEEPSTOP: residual gauge %d = %d after the drain stopped, want 0", c, got)
		}
	}
}

func TestSweepGoesDormantOnPermanentResidue(t *testing.T) {
	l, cnt := sweepLoop(t)
	tgt := &countingTarget{}
	defer tgt.closeAll()
	const detached = 3
	for i := 0; i < detached; i++ {
		detachedConn(t, l)
	}
	l.transplant.Store(&transplantState{target: tgt})

	// The first cycle examines all three, moves none, and every refusal is
	// permanent: the sweep must stop.
	l.sweep()
	if n := cnt.passes.Load(); n != 1 {
		t.Fatalf("celeris657 DORMANT PREMISE: %d passes, want 1", n)
	}
	if got := tgt.count(); got != 0 {
		t.Fatalf("celeris657 DORMANT PREMISE: %d detached conns were handed over; they must not be", got)
	}
	if !l.sweepDormant {
		t.Errorf("celeris657 DORMANT: the sweep is still awake after a full cycle in which every conn left was "+
			"detached (WS/SSE) — a residue that cannot change without an event of its own. It would re-examine "+
			"%d conns for as long as the drain lasted", detached)
	}
	if ms := l.sweepWaitMs(); ms != -1 {
		t.Errorf("celeris657 DORMANT: sweepWaitMs = %d while dormant, want -1: a dormant sweep must not cap "+
			"epoll_wait", ms)
	}
	if got := cnt.residual[resDetached].Load(); got != detached {
		t.Errorf("celeris657 DORMANT: TransplantResidualDetached gauge = %d, want %d", got, detached)
	}
	for _, c := range []int{resH2, resPinned, resBusy} {
		if got := cnt.residual[c].Load(); got != 0 {
			t.Errorf("celeris657 DORMANT: residual class %d = %d, want 0", c, got)
		}
	}

	// Dormant means dormant: further calls run no pass at all.
	before := cnt.passes.Load()
	for i := 0; i < 5; i++ {
		forcePass(l)
		l.sweep()
	}
	if n := cnt.passes.Load(); n != before {
		t.Errorf("celeris657 DORMANT: %d passes ran while dormant, want none", n-before)
	}

	// A connection joining the live set is a different set: the sweep wakes,
	// and the new one moves.
	movableConn(t, l)
	if l.sweepDormant {
		t.Fatalf("celeris657 DORMANT: adding a conn to liveConns did not wake the sweep")
	}
	forcePass(l)
	l.sweep()
	if got := tgt.count(); got != 1 {
		t.Errorf("celeris657 DORMANT: after waking, the sweep handed over %d conns, want the 1 movable one", got)
	}
	if got := cnt.residual[resDetached].Load(); got != detached {
		t.Errorf("celeris657 DORMANT: TransplantResidualDetached gauge = %d after the wake, want %d",
			got, detached)
	}
}

// TestSweepBudgetCoversEveryConnAcrossPasses pins the per-pass budget and its
// cursor: one pass examines at most sweepBudget connections, and the cycle
// that follows reaches the rest rather than starting over at the tail.
func TestSweepBudgetCoversEveryConnAcrossPasses(t *testing.T) {
	l, cnt := sweepLoop(t)
	tgt := &countingTarget{}
	defer tgt.closeAll()
	// Detached conns, so a pass moves nothing and the cursor is the only
	// thing that decides what the next pass sees.
	total := sweepBudget + 5
	for i := 0; i < total; i++ {
		fd := socketpairFD(t, l)
		cs := regLive(l, fd)
		cs.protocol = engine.HTTP1
		cs.detected = true
		cs.h1State = conn.NewH1State()
		cs.h1State.Detached.Store(true)
		l.connCount++
		l.activeConns.Add(1)
	}
	l.transplant.Store(&transplantState{target: tgt})

	l.sweep()
	if l.sweepCursor != total-1-sweepBudget {
		t.Errorf("celeris657 BUDGET: after one pass the cursor is %d, want %d (%d conns, budget %d)",
			l.sweepCursor, total-1-sweepBudget, total, sweepBudget)
	}
	if l.sweepDormant {
		t.Errorf("celeris657 BUDGET: the sweep went dormant after ONE budgeted pass, before the cycle had " +
			"examined every conn")
	}
	forcePass(l)
	l.sweep()
	if n := cnt.passes.Load(); n != 2 {
		t.Fatalf("celeris657 BUDGET: %d passes, want 2", n)
	}
	if !l.sweepDormant {
		t.Errorf("celeris657 BUDGET: the cycle completed over two passes and every conn left was permanent " +
			"residue, but the sweep is still awake")
	}
	if got := cnt.residual[resDetached].Load(); got != uint64(total) {
		t.Errorf("celeris657 BUDGET: the cycle's residual gauge is %d, want %d — a budgeted cycle must count "+
			"every conn it examined, across its passes", got, total)
	}
}

// TestAskNeverOutlivesConnState is the P8 lifetime rule. connStates go back to
// a package-global sync.Pool, so an ask still naming one after its release
// would make the loop read a cs another connection owns (the celeris#654
// class). Under -race, with a close running concurrently with the ask.
func TestAskNeverOutlivesConnState(t *testing.T) {
	l, _ := sweepLoop(t)
	tgt := &countingTarget{}
	defer tgt.closeAll()
	l.transplant.Store(&transplantState{target: tgt})

	// The plain ordering first: an ask queued, then the connState released.
	fd, cs := movableConn(t, l)
	cs.xferAsked.Store(true)
	l.askTransplant(cs)
	l.detachFromEpoll(fd, cs)
	l.dropAsk(cs)
	releaseConnState(cs)
	if cs.xferAsked.Load() {
		t.Errorf("celeris657 ASKLIFE: xferAsked survived the release")
	}
	l.xferAskMu.Lock()
	for i, q := range l.xferAskQ {
		if q == cs {
			t.Errorf("celeris657 ASKLIFE: the ask queue still names a released connState at index %d — the "+
				"loop would read a cs the pool can hand to another conn (celeris#654)", i)
		}
	}
	l.xferAskMu.Unlock()
	l.drainTransplantAsks() // must not touch the released cs
	_ = unix.Close(fd)

	// Then the race: dispatch goroutines asking while the loop drains and
	// releases. Every release goes through dropAsk first, as the loop's own
	// paths do.
	const rounds = 64
	var wg sync.WaitGroup
	var asks atomic.Int64
	for i := 0; i < rounds; i++ {
		rfd, rcs := movableConn(t, l)
		start := make(chan struct{})
		wg.Add(1)
		go func() { // the dispatch goroutine's side of the ask
			defer wg.Done()
			<-start
			if rcs.xferAsked.CompareAndSwap(false, true) {
				l.askTransplant(rcs)
				asks.Add(1)
			}
		}()
		close(start)
		wg.Wait() // the goroutine has exited: a connState is released only then
		l.drainTransplantAsks()
		if l.conns[rfd] == rcs {
			l.detachFromEpoll(rfd, rcs)
			l.dropAsk(rcs)
			releaseConnState(rcs)
			_ = unix.Close(rfd)
		}
	}
	if asks.Load() == 0 {
		t.Fatal("celeris657 ASKLIFE PREMISE: no ask was ever queued")
	}
	l.drainTransplantAsks()
	t.Logf("celeris657 ASKLIFE asks=%d adopted=%d", asks.Load(), tgt.count())
}

// TestSweepCadenceBacksOffAndResets pins the cost control the price harness
// depends on: a pass that moves nothing doubles the interval to the cap, and
// any move takes it back to the floor.
func TestSweepCadenceBacksOffAndResets(t *testing.T) {
	l, _ := sweepLoop(t)
	tgt := &countingTarget{}
	defer tgt.closeAll()
	// One conn that never moves and is TRANSIENT residue (not detected yet,
	// so the protocol gates refuse it but it could change on its own), so
	// the sweep keeps running and backs off instead of going dormant.
	fd := socketpairFD(t, l)
	cs := regLive(l, fd)
	l.connCount++
	l.activeConns.Add(1)
	_ = cs
	l.transplant.Store(&transplantState{target: tgt})

	l.sweep()
	if l.sweepIvl != sweepMinIvl*2 {
		t.Errorf("celeris657 CADENCE: after one pass that moved nothing the interval is %v, want %v",
			time.Duration(l.sweepIvl), time.Duration(sweepMinIvl*2))
	}
	if l.sweepDormant {
		t.Fatalf("celeris657 CADENCE PREMISE: the sweep went dormant on a transient residue")
	}
	for i := 0; i < 12; i++ {
		forcePass(l)
		l.sweep()
	}
	if l.sweepIvl != sweepMaxIvl {
		t.Errorf("celeris657 CADENCE: the back-off settled at %v, want the %v cap",
			time.Duration(l.sweepIvl), time.Duration(sweepMaxIvl))
	}
	if ms := l.sweepWaitMs(); ms < 0 || ms > int(sweepMaxIvl/int64(time.Millisecond)) {
		t.Errorf("celeris657 CADENCE: sweepWaitMs = %d, want a cap within the %v interval",
			ms, time.Duration(sweepMaxIvl))
	}

	// Something moves: back to the floor.
	movableConn(t, l)
	forcePass(l)
	l.sweep()
	if got := tgt.count(); got != 1 {
		t.Fatalf("celeris657 CADENCE PREMISE: the pass handed over %d conns, want 1", got)
	}
	if l.sweepIvl != sweepMinIvl {
		t.Errorf("celeris657 CADENCE: after a pass that moved a conn the interval is %v, want the %v floor",
			time.Duration(l.sweepIvl), time.Duration(sweepMinIvl))
	}
}
