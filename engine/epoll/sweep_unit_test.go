//go:build linux

package epoll

// celeris#657 P7/P8 at the unit level: the sweep's cost controls and the ask
// queue's lifetime rule, driven directly on the loop thread with no kernel
// scheduling in the way.

import (
	"runtime"
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
// class).
//
// celeris#657 R2, MINOR-c: this test's first version did not do what its own
// doc said. It called wg.Wait() BEFORE the release, so the asking goroutine
// had always exited and the two never overlapped — the ask and the release
// ran strictly in sequence, once each, and -race had nothing to observe. The
// version below keeps the asking goroutines RUNNING across the loop's drain,
// drop and release of OTHER connections, which is the shape production has
// (one dispatch goroutine per connection, all parking independently while the
// loop tears others down), and it asserts that the overlap actually happened
// rather than assuming it.
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
	if l.asksName(cs) {
		t.Errorf("celeris657 ASKLIFE: a queue the loop can still read names a released connState — the loop " +
			"would read a cs the pool can hand to another conn (celeris#654)")
	}
	l.drainTransplantAsks() // must not touch the released cs
	_ = unix.Close(fd)

	// Then the race, genuinely concurrent: a pool of dispatch goroutines
	// asking for their OWN connections, in a loop, for the whole time the
	// loop thread drains its queue and releases connections. Every release
	// goes through dropAsk first, as the loop's own paths do. Under -race,
	// this is what proves xferAskMu actually covers the queue against the
	// park-boundary appends.
	const askers = 8
	const rounds = 64
	var wg sync.WaitGroup
	var asks, overlaps atomic.Int64
	var inLoopWork atomic.Bool
	stop := make(chan struct{})
	for i := 0; i < askers; i++ {
		_, acs := movableConn(t, l)
		wg.Add(1)
		go func() { // a dispatch goroutine, parking over and over
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				// The CAS is the dedup: it succeeds again only after the
				// loop has drained this conn's ask, which is what makes
				// the wait below terminate.
				if acs.xferAsked.CompareAndSwap(false, true) {
					if inLoopWork.Load() {
						overlaps.Add(1)
					}
					l.askTransplant(acs)
					asks.Add(1)
				}
				runtime.Gosched()
			}
		}()
	}
	// The overlap is WAITED FOR, not hoped for: every round opens the window
	// and does not close it until an ask has been queued inside it. Without
	// this the loop below can finish all its rounds before the scheduler ever
	// runs an asker, which is how the first version of this test managed to
	// observe no concurrency at all and still pass (MINOR-c) — and it is what
	// two of the round-2 CI arms caught when they ran it.
	for i := 0; i < rounds; i++ {
		rfd, rcs := movableConn(t, l)
		l.drainTransplantAsks() // clears xferAsked, so the askers can ask again
		inLoopWork.Store(true)
		target := overlaps.Load() + 1
		deadline := time.Now().Add(20 * time.Second)
		for overlaps.Load() < target {
			if time.Now().After(deadline) {
				inLoopWork.Store(false)
				close(stop)
				wg.Wait()
				t.Fatalf("celeris657 ASKLIFE PREMISE: round %d waited 20s and not one of the %d asking "+
					"goroutines queued an ask inside the loop's window (asks=%d overlaps=%d). The "+
					"injection did not fire, so nothing raced anything", i, askers, asks.Load(), overlaps.Load())
			}
			runtime.Gosched()
		}
		if l.conns[rfd] == rcs {
			l.detachFromEpoll(rfd, rcs)
			l.dropAsk(rcs)
			releaseConnState(rcs)
			_ = unix.Close(rfd)
		}
		inLoopWork.Store(false)
	}
	close(stop)
	wg.Wait()
	if got := overlaps.Load(); got < rounds {
		t.Errorf("celeris657 ASKLIFE PREMISE: %d asks landed inside the loop's window over %d rounds, want "+
			"at least one each", got, rounds)
	}
	l.drainTransplantAsks()
	t.Logf("celeris657 ASKLIFE asks=%d overlaps=%d adopted=%d", asks.Load(), overlaps.Load(), tgt.count())
}

// asksName reports whether any queue the loop can still read names cs. Loop
// thread, test only.
func (l *Loop) asksName(cs *connState) bool {
	l.xferAskMu.Lock()
	defer l.xferAskMu.Unlock()
	for _, q := range l.xferAskQ {
		if q == cs {
			return true
		}
	}
	for _, q := range l.xferAskDrain {
		if q == cs {
			return true
		}
	}
	return false
}

// TestSweepCadenceBacksOffAndResets pins the cost control the price harness
// depends on: a pass that moves nothing doubles the interval to the cap, and
// any move takes it back to the floor.
func TestSweepCadenceBacksOffAndResets(t *testing.T) {
	l, _ := sweepLoop(t)
	tgt := &countingTarget{}
	defer tgt.closeAll()
	// One conn that never moves and is TRANSIENT residue: a detected HTTP/1
	// keep-alive with an unflushed response, so flushedAtBoundary refuses it
	// and the refusal can clear with no event of the conn's own. (Before
	// celeris#657 R2 this fixture was an accepted-but-silent conn; that is
	// now the PERMANENT resUnstarted class and would go dormant — MINOR-e.)
	fd, cs := movableConn(t, l)
	cs.writeBuf = append(cs.writeBuf, 'x')
	cs.pendingBytes = 1
	_ = fd
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

// TestSweepDoesNotBlockOnADetachedConnsLock is the celeris#667/#672
// interaction on the epoll side. tryTransplant takes cs.asyncInMu to read the
// async gate, and a detached WebSocket or SSE conn's dispatch goroutine holds
// that same lock while it takes delivery of a frame. A sweep that examined
// such a conn would contend for it on every pass, for as long as the drain
// lasted, on a conn the hand-off refuses anyway. The sweep reads the
// permanent classes first and never reaches the lock.
func TestSweepDoesNotBlockOnADetachedConnsLock(t *testing.T) {
	l, _ := sweepLoop(t)
	l.async = true
	tgt := &countingTarget{}
	defer tgt.closeAll()
	_, cs := detachedConn(t, l)
	cs.asyncRun = true
	l.transplant.Store(&transplantState{target: tgt})

	// The dispatch goroutine, mid-delivery, holding its own input lock.
	cs.asyncInMu.Lock()
	done := make(chan struct{})
	go func() { l.sweep(); close(done) }()
	select {
	case <-done:
		cs.asyncInMu.Unlock()
	case <-time.After(2 * time.Second):
		cs.asyncInMu.Unlock()
		<-done
		t.Fatal("celeris657 WSLOCK: the sweep blocked on a detached conn's asyncInMu. It examined a conn the " +
			"hand-off refuses for as long as it lives, and contended with the WebSocket delivery path to do it")
	}
	if got := tgt.count(); got != 0 {
		t.Errorf("celeris657 WSLOCK: %d detached conns were handed over, want 0", got)
	}
}
