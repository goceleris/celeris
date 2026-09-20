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
//     PERMANENT reason (detached WS/SSE, H2/h2c, pinned) stops the sweep until
//     a connection is added to the loop. Dormancy is safe because it removes
//     only the EXTRA examinations: the per-event call site is untouched, so a
//     connection that becomes movable through its own traffic is still
//     examined at that traffic's event.
//
// epoll_wait is capped only while a pass is owed and the sweep is not dormant,
// so a standby loop with nothing to move blocks exactly as it did before.

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
	resDetached = iota // detached WS/SSE: never movable while it lives
	resH2              // H2, h2c, or an H1 connection mid-upgrade
	resPinned          // cannot be handed over at all (io_uring: fixed file, no reap)
	resBusy            // transient: mid-request, unflushed, handler running
	numResidual
)

// residualPermanent reports whether a refusal class can change without the
// connection producing an event of its own. Only the transient class can.
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

// wakeSweep lifts dormancy and makes the next pass due. Called when a
// connection joins this loop's live set (an accept, or an adopt from the other
// engine): the set the dormant cycle judged is no longer the set the loop
// holds. Loop thread; the guard keeps it off the accept path's cost when the
// sweep is not dormant, which is every case but this one.
func (l *Loop) wakeSweep() {
	if l.sweepDormant {
		l.sweepDormant = false
		l.sweepNext = 0
	}
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
		l.tryTransplant(fd)
		if l.conns[fd] != cs {
			moved++
			continue
		}
		class := l.residualClass(cs)
		l.cycleRes[class]++
		if !residualPermanent(class) {
			l.cycleTransient++
		}
	}
	l.cycleMoved += moved

	if i < 0 {
		// The cycle is complete: every connection this loop holds was
		// examined. Judge dormancy on the whole cycle, not on one budgeted
		// pass, then publish the residue and start again at the tail.
		if l.cycleMoved == 0 && l.cycleTransient == 0 {
			l.sweepDormant = true
		}
		l.publishResidual()
		l.resetCycle()
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

// resetCycle clears the per-cycle accounting. Loop thread.
func (l *Loop) resetCycle() {
	l.cycleMoved = 0
	l.cycleTransient = 0
	l.cycleRes = [numResidual]uint64{}
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
// tryTransplant has already run and left the connection here, and this only
// reads which of its gates the connection sits behind. Everything the gates
// can change on their own — a request in flight, an unflushed response, a
// running dispatch goroutine — is transient; the rest is permanent for as long
// as the connection lives. Loop thread.
func (l *Loop) residualClass(cs *connState) int {
	if cs.h1State != nil && cs.h1State.Detached.Load() {
		return resDetached
	}
	if cs.h2State != nil || cs.asyncH2Promoted.Load() || (cs.detected && cs.protocol != engine.HTTP1) {
		return resH2
	}
	if cs.hijacked {
		return resPinned
	}
	return resBusy
}
