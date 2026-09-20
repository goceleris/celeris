//go:build linux

package epoll

import (
	"sync/atomic"
	"time"

	"github.com/goceleris/celeris/engine"
)

// The post-switch sweep (celeris#657, face 1).
//
// Before this, a loop examined a connection for the #383 hand-off at exactly
// one place: the end of that connection's own epoll event (loop.go, after the
// event batch's drainRead). For a keep-alive client that event is the arrival
// of its next request, so the connection was always mid-request when it was
// looked at, and the async gate (parked && idle) refused it: the last refusal
// accounted for 100% of the stuck pool, at p(move) = 1.5e-4 to 4.7e-4 per
// examination. A connection that was IDLE when the drain started produced no
// event at all and was never examined: measured, all 64 stayed for 3 s at a
// promotion, and 64 of 64 at every sample for 1.5 s at a revert.
//
// The sweep re-examines the connections this loop still holds while a drain is
// set, on the loop thread, until it holds none of them. It adds no new way to
// move a connection: every candidate goes through the unchanged tryTransplant,
// so the eligibility gates, the ledger and (on the io_uring side) the
// fd-lifetime rule are exactly the ones the event path uses.
//
// Three things keep its cost bounded:
//
//   - a cadence: the first pass runs at once, then every sweepMinIvl, doubling
//     to sweepMaxIvl after a pass that moves nothing, and reset by any move;
//   - a per-pass budget with a cursor, so one pass over a large table is
//     several bounded passes rather than one long one, and the loop keeps
//     serving between them;
//   - DORMANCY: a full cycle in which every connection left was refused for a
//     PERMANENT reason (detached WS/SSE, H2/h2c, pinned, not yet started)
//     stops the sweep until the live set changes. Dormancy is safe because it
//     removes only the EXTRA examinations: the per-event call site is
//     untouched, so a connection that becomes movable through its own traffic
//     is still examined at that traffic's event.
//
// epoll_wait is capped only while a pass is owed and the sweep is not dormant,
// so a standby loop with nothing to move blocks exactly as it did before.
//
// # THE CYCLE RULE (celeris#657 R2, MAJOR-1)
//
// Dormancy is a statement about a whole cycle, so a cycle must not be able to
// end without having examined everything it is a statement about:
//
//	A cycle may conclude that every connection left is permanent residue
//	only if NOTHING JOINED the live set between its first pass and its
//	last. sweepArrived records that it did; a cycle with it set cannot go
//	dormant and starts another cycle from the tail instead.
//
// This is not belt-and-braces, it is the hole the cursor leaves. addLiveConn
// appends at the TAIL and a pass walks BACKWARDS from sweepCursor, so a
// connection that joins during a multi-pass cycle lands PAST the cursor: this
// cycle can never reach it, it contributes to neither cycleMoved nor
// cycleTransient, and without the rule the cycle would go dormant with it
// unexamined — on the outgoing engine, indefinitely, which is the exact
// failure the sweep exists to fix. (Worked: 261 connections, budget 256. Pass
// 1 examines 260..5 and leaves the cursor at 4; a connection is appended at
// index 261; pass 2 walks 4..0, completes the cycle and, without the rule,
// sleeps with 261 never looked at.)
//
// BOUNDEDNESS. The rule cannot spin. One sweep() call walks at most
// sweepBudget connections and always returns: the index strictly decreases and
// no arrival restarts a walk in progress. An arrival costs at most one further
// cycle, and the cadence is untouched — so the steady-state cost under a
// continuous stream of arrivals is at most sweepBudget examinations per
// sweepIvl per loop, which backs off to sweepMaxIvl for as long as nothing
// moves. Not going dormant under continuous arrivals is the POINT: while
// connections keep joining there is always one that has not been examined.

const (
	// sweepMinIvl is the interval after a pass that moved something, and the
	// floor of the back-off. 2 ms is the cadence B's prototype was measured at.
	sweepMinIvl = int64(2 * time.Millisecond)
	// sweepMaxIvl caps the back-off of a sweep that keeps finding nothing
	// movable and cannot go dormant (a transient residue: a slow handler, a
	// connection mid-request).
	sweepMaxIvl = int64(64 * time.Millisecond)
	// sweepBudget is the most connections one pass examines. A cycle over a
	// larger live set takes several passes, resuming at the cursor.
	sweepBudget = 256
)

// Residual classes. A connection the sweep examined and did not move is
// counted under the reason it was refused; the permanent ones are what
// dormancy is decided on.
const (
	resDetached  = iota // detached WS/SSE: never movable while it lives
	resH2               // H2, h2c, or an H1 connection mid-upgrade
	resPinned           // cannot be handed over at all (io_uring: fixed file, no reap)
	resUnstarted        // accepted, has sent nothing: no protocol detected yet
	resBusy             // transient: mid-request, unflushed, handler running
	numResidual
)

// residualPermanent reports whether a refusal class can change without the
// connection producing an event of its own. Only the transient class can.
//
// resUnstarted is PERMANENT by that definition and not by a weaker one: the
// gate that refuses it is !cs.detected, and cs.detected is set on the loop
// thread when the connection's first bytes are read — i.e. at an event of its
// own, where the per-event call site examines it anyway (celeris#657 R2,
// MINOR-e). Classing it transient made a silent accepted connection count as
// cycleTransient on every cycle, so the sweep NEVER went dormant, epoll_wait
// stayed capped at the 64 ms back-off for the whole drain, and the gauge read
// non-zero after the switch had settled.
func residualPermanent(class int) bool { return class != resBusy }

// sweepCounters are the engine-wide destinations a loop publishes to. The
// residual entries are GAUGES: a loop publishes the delta against what it
// published last, so the sum over loops is the live residue.
type sweepCounters struct {
	passes   atomic.Uint64
	residual [numResidual]atomic.Uint64
}

// sweepWaitMs is the epoll_wait cap this loop needs to run its sweep on time:
// 0 for a drain whose first pass is owed, the milliseconds to the next pass
// while connections remain, and -1 when no cap is owed — no drain, a dormant
// sweep, or an empty live set. Loop thread.
func (l *Loop) sweepWaitMs() int {
	ts := l.transplant.Load()
	if ts == nil {
		return -1
	}
	if ts != l.sweepTS {
		return 0 // a new drain: sweep before waiting on anything
	}
	if l.sweepDormant || len(l.liveConns) == 0 {
		return -1
	}
	d := (l.sweepNext - time.Now().UnixNano() + int64(time.Millisecond) - 1) / int64(time.Millisecond)
	if d < 0 {
		d = 0
	}
	return int(d)
}

// wakeSweep records that a connection JOINED this loop's live set (an accept,
// or an adopt from the other engine) and makes the sweep act on it. Loop
// thread, from addLiveConn.
//
// Two things happen here and they are not the same thing. Lifting dormancy is
// what gets a pass to run at all. Setting sweepArrived is what stops the cycle
// in progress from concluding, on evidence that predates this connection, that
// everything left is permanent — see THE CYCLE RULE above. The flag is set
// UNCONDITIONALLY: a sweep that is merely awake is mid-cycle, walking
// backwards from a cursor this connection has just been appended past.
func (l *Loop) wakeSweep() {
	l.sweepArrived = true
	if l.sweepDormant {
		l.sweepDormant = false
		l.sweepNext = 0
	}
}

// sweepNoteDeparture records that a connection LEFT this loop's live set. Loop
// thread, from removeLiveConn.
//
// A departure adds no work, so it does not set sweepArrived. It does make the
// published residue wrong: the gauges are what the engine HOLDS, and a dormant
// sweep publishes nothing, so without this a connection that closed while the
// sweep slept would be counted as residue for the rest of the drain
// (celeris#657 R2, MINOR-d). Lifting dormancy costs one more cycle, which
// re-counts what is left and sleeps again.
func (l *Loop) sweepNoteDeparture() {
	if l.sweepDormant {
		l.sweepDormant = false
		l.sweepNext = 0
	}
}

// sweepRetract drops everything this loop has published into the engine-wide
// gauges. Loop thread, at shutdown: a loop that is gone holds nothing, and
// nothing else would ever retract its last cycle's residue (celeris#657 R2,
// MINOR-d).
func (l *Loop) sweepRetract() {
	if l.sweepPub == ([numResidual]uint64{}) {
		return
	}
	l.resetCycle()
	l.publishResidual()
}

// sweep runs one budgeted pass of the post-switch sweep. Loop thread only,
// after the event batch (so no drainRead of this batch is pending) and before
// drainDetachQueue (so a deferred async hand-off it starts is finished in the
// same iteration once the goroutine exits).
//
// liveConns is swap-removed, so walking it BACKWARDS visits every connection
// at most once even as the walk removes them: the entry swapped into a vacated
// slot comes from the tail, which the walk has already passed.
func (l *Loop) sweep() {
	ts := l.transplant.Load()
	if ts == nil {
		// The drain stopped. Forget the epoch, clear dormancy and retract
		// the residual gauge: nothing is owed until the next drain.
		if l.sweepTS != nil {
			l.sweepTS = nil
			l.sweepDormant = false
			l.sweepCursor = -1
			l.resetCycle()
			l.publishResidual()
		}
		return
	}
	if ts != l.sweepTS {
		// A new drain: restart the cadence and the cycle.
		l.sweepTS = ts
		l.sweepIvl = sweepMinIvl
		l.sweepNext = 0
		l.sweepDormant = false
		l.sweepCursor = -1
		l.resetCycle()
	}
	if l.sweepDormant {
		// Reachable with a stale residue published only if a connection
		// could leave without sweepNoteDeparture: it cannot. removeLiveConn
		// is the one way out of liveConns and it lifts dormancy, so the
		// retraction below is always reached; shutdown, which truncates
		// liveConns wholesale, calls sweepRetract itself (celeris#657 R2,
		// MINOR-d). Keeping the dormant return AHEAD of the retraction is
		// therefore safe, and it is what keeps a dormant sweep free.
		return
	}
	if len(l.liveConns) == 0 {
		// Nothing left to hold: retract the residue this loop published,
		// so the gauge reads what the engine HOLDS and not what its last
		// non-empty cycle saw.
		if l.sweepPub != ([numResidual]uint64{}) {
			l.resetCycle()
			l.publishResidual()
		}
		return
	}
	now := time.Now().UnixNano()
	if now < l.sweepNext {
		return
	}
	if l.sweepCnt != nil {
		l.sweepCnt.passes.Add(1)
	}

	// sweepCursor is -1 at the start of a cycle, and otherwise the index the
	// last budgeted pass stopped at. A stale value (connections left since)
	// is clamped back to the tail.
	i := l.sweepCursor
	if i < 0 || i >= len(l.liveConns) {
		i = len(l.liveConns) - 1
	}
	examined, moved := 0, 0
	for ; i >= 0 && examined < sweepBudget; i-- {
		if l.transplant.Load() != ts {
			break // the drain stopped mid-pass; the next call resets everything
		}
		if i >= len(l.liveConns) {
			continue
		}
		fd := l.liveConns[i]
		if fd < 0 || fd >= len(l.conns) {
			continue
		}
		cs := l.conns[fd]
		if cs == nil {
			continue
		}
		examined++
		// Permanent residue: a detached WebSocket or SSE conn, an H2 one, a
		// hijacked one, one that has never spoken. tryTransplant refuses
		// each of them on a gate that cannot change while the conn lives,
		// or cannot change without an event of the conn's own, so examining
		// it is work with no outcome.
		//
		// read=false skips tryTransplant too, and loses nothing: every conn
		// with a detachMu that is held is one tryTransplant would refuse.
		// An async one has its dispatch goroutine inside ProcessH1, so the
		// parked && idle gate refuses it; one held by a guarded writeFn is
		// detached, so the Detached gate does. Skipping is the cheaper way
		// to reach the same answer, and it keeps the loop off a lock the
		// WebSocket write path holds (celeris#667/#672).
		if class, read := l.residualClass(cs); class != resBusy || !read {
			l.cycleRes[class]++
			if !residualPermanent(class) {
				l.cycleTransient++
			}
			continue
		}
		l.tryTransplant(fd)
		if l.conns[fd] != cs {
			moved++
			continue
		}
		class, _ := l.residualClass(cs)
		l.cycleRes[class]++
		if !residualPermanent(class) {
			l.cycleTransient++
		}
	}
	l.cycleMoved += moved

	if i < 0 {
		// The cycle is complete: every connection this loop held for the
		// whole of it was examined. Judge dormancy on the whole cycle, not
		// on one budgeted pass, and only if the set did not change under it
		// (THE CYCLE RULE). Then publish the residue and start again at the
		// tail.
		if l.cycleMoved == 0 && l.cycleTransient == 0 && !l.sweepArrived {
			l.sweepDormant = true
		}
		l.publishResidual()
		l.resetCycle() // clears sweepArrived: the next cycle is its own witness
		l.sweepCursor = -1
	} else {
		l.sweepCursor = i
	}

	if moved > 0 {
		l.sweepIvl = sweepMinIvl
	} else if l.sweepIvl < sweepMaxIvl {
		l.sweepIvl *= 2
		if l.sweepIvl > sweepMaxIvl {
			l.sweepIvl = sweepMaxIvl
		}
	}
	l.sweepNext = now + l.sweepIvl
}

// resetCycle clears the per-cycle accounting, sweepArrived included: it is a
// per-cycle witness, and clearing it here is what starts the next cycle's.
// Loop thread.
func (l *Loop) resetCycle() {
	l.cycleMoved = 0
	l.cycleTransient = 0
	l.cycleRes = [numResidual]uint64{}
	l.sweepArrived = false
}

// publishResidual moves this loop's per-class residue into the engine-wide
// gauges as a delta against what it published last, so the engine's value is
// the sum over loops of what each loop currently holds. Loop thread.
func (l *Loop) publishResidual() {
	if l.sweepCnt == nil {
		l.sweepPub = l.cycleRes
		return
	}
	for c := range l.cycleRes {
		if d := int64(l.cycleRes[c]) - int64(l.sweepPub[c]); d != 0 {
			l.sweepCnt.residual[c].Add(uint64(d))
		}
	}
	l.sweepPub = l.cycleRes
}

// residualClass names why the hand-off refused cs. It duplicates no decision:
// tryTransplant has already run and left the connection here, or is about to
// be spared the call, and this only reads which of its gates the connection
// sits behind. Everything the gates can change on their own — a request in
// flight, an unflushed response, a running dispatch goroutine — is transient;
// the rest is permanent for as long as the connection lives, or until the
// connection produces an event of its own. Loop thread.
//
// read=false means NOTHING was read and resBusy is a default, not a finding:
// see the locking rule below. The caller must treat it as transient residue
// and must not skip tryTransplant on the strength of it.
//
// # LOCKING (celeris#256 / #548 / #593, R2 MAJOR-2)
//
// cs.h1State, cs.h2State and cs.protocol are written by the dispatch
// goroutine's switchToH2Local (loop.go), which holds cs.detachMu across all
// three. Reading them from the loop thread without that lock is the TOCTOU
// this engine forbids in three docstrings, and the race detector flags it.
// So they are read here under cs.detachMu — and with TryLock, not Lock:
// runAsyncHandler holds cs.detachMu across the whole of ProcessH1, so a
// blocking Lock on the loop thread would park the loop, and therefore every
// connection it owns, behind one slow async handler. That is celeris#593, and
// snapshotH1Deadlines (iouring/worker.go) takes exactly this shape for exactly
// this reason.
//
// A connection whose detachMu is held is, by that fact, in a handler: the
// transient class, reported without reading anything. A connection with no
// detachMu at all has no dispatch goroutine — sync mode, or async before the
// first promotion — so the loop thread owns the three fields and the reads are
// inherently safe.
func (l *Loop) residualClass(cs *connState) (class int, read bool) {
	if mu := cs.detachMu; mu != nil {
		if !mu.TryLock() {
			return resBusy, false
		}
		class = l.classifyLocked(cs)
		mu.Unlock()
		return class, true
	}
	return l.classifyLocked(cs), true
}

// classifyLocked is residualClass's body, with its precondition met: either cs
// has no dispatch goroutine, or the caller holds cs.detachMu. Loop thread.
func (l *Loop) classifyLocked(cs *connState) int {
	if cs.h1State != nil && cs.h1State.Detached.Load() {
		return resDetached
	}
	if cs.h2State != nil || cs.asyncH2Promoted.Load() || (cs.detected && cs.protocol != engine.HTTP1) {
		return resH2
	}
	if cs.hijacked {
		return resPinned
	}
	if !cs.detected || cs.h1State == nil {
		// tryTransplant's FIRST gate (transplant.go). An accepted
		// connection that has sent no byte has no detected protocol and no
		// H1 state, and neither appears until it speaks.
		return resUnstarted
	}
	return resBusy
}
