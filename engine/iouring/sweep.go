//go:build linux

package iouring

import (
	"sync/atomic"
	"time"

	"github.com/goceleris/celeris/engine"
)

// The post-switch sweep, io_uring half (celeris#657 P9). The epoll half's
// docstring (engine/epoll/sweep.go) states the problem and the shape; this
// file is the same mechanism on the worker thread, with three differences
// this engine forces.
//
//  1. The hand-off is not free here. A connection sitting idle has its next
//     recv armed, and handing it over then is the fd-lifetime bug of PR-2. So
//     the sweep does exactly what the event path does: it calls the unchanged
//     tryTransplant, which refuses under the R0 gate and starts a REAP, and
//     the connection leaves at that recv's own -ECANCELED. The sweep places no
//     SQE itself and bypasses no gate.
//
//  2. On a worker whose kernel has no IORING_ASYNC_CANCEL flags there is no
//     reap. A connection with its recv armed can then never be moved by an
//     examination, only by its own next request (whose response is held). The
//     sweep must not ask: calling tryTransplant would count a
//     TransplantReapUnsupported on every pass for as long as the drain lasted.
//     Such a connection is classed as PINNED residue instead, which is
//     permanent, so the sweep goes dormant rather than spinning.
//
//  3. A promoted async connection is owned by its dispatch goroutine, which
//     claims its own hand-off at its park (runAsyncHandler). The worker must
//     not seize it. The sweep gives each such connection one Broadcast per
//     drain, so a goroutine parked before the drain started re-evaluates and
//     makes its claim; after that it is left alone.
const (
	iouSweepMinIvl = int64(2 * time.Millisecond)
	iouSweepMaxIvl = int64(64 * time.Millisecond)
	iouSweepBudget = 256
)

// Residual classes, as in the epoll half.
const (
	resDetached = iota
	resH2
	resPinned
	resBusy
	numResidual
)

func residualPermanent(class int) bool { return class != resBusy }

// sweepCounters are the engine-wide destinations a worker publishes into. The
// residual entries are gauges, published as a delta per worker.
type sweepCounters struct {
	passes   atomic.Uint64
	residual [numResidual]atomic.Uint64
}

// sweepWait is the ring-wait cap this worker needs to run its sweep on time,
// and whether a cap is owed at all. Worker thread.
func (w *Worker) sweepWait() (time.Duration, bool) {
	h := w.transplant.Load()
	if h == nil {
		return 0, false
	}
	if h != w.sweepH {
		return 0, true // a new drain: sweep before waiting on anything
	}
	if w.sweepDormant || len(w.liveConns) == 0 {
		return 0, false
	}
	d := time.Duration(w.sweepNext - time.Now().UnixNano())
	if d < 0 {
		d = 0
	}
	return d, true
}

// wakeSweep lifts dormancy and makes the next pass due, when a connection
// joins this worker's live set. Worker thread.
func (w *Worker) wakeSweep() {
	if w.sweepDormant {
		w.sweepDormant = false
		w.sweepNext = 0
	}
}

// sweep runs one budgeted pass. Worker thread only, after drainDetachQueue —
// so a claim a Broadcast produced is already drained when the next pass looks
// at the connection, and the pass sees the settled table.
//
// liveConns here is removed by scan-and-swap (removeLiveConn), tail into the
// vacated slot, so a backward walk visits each connection at most once.
func (w *Worker) sweep() {
	h := w.transplant.Load()
	if h == nil {
		if w.sweepH != nil {
			w.sweepH = nil
			w.sweepDormant = false
			w.sweepCursor = -1
			w.resetCycle()
			w.publishResidual()
		}
		return
	}
	if h != w.sweepH {
		w.sweepH = h
		w.sweepIvl = iouSweepMinIvl
		w.sweepNext = 0
		w.sweepDormant = false
		w.sweepCursor = -1
		w.resetCycle()
	}
	if w.sweepDormant {
		return
	}
	if len(w.liveConns) == 0 {
		// Nothing left to hold: retract the residue this worker published.
		if w.sweepPub != ([numResidual]uint64{}) {
			w.resetCycle()
			w.publishResidual()
		}
		return
	}
	now := time.Now().UnixNano()
	if now < w.sweepNext {
		return
	}
	if w.sweepCnt != nil {
		w.sweepCnt.passes.Add(1)
	}

	// sweepCursor is -1 at the start of a cycle, and otherwise the index the
	// last budgeted pass stopped at. A stale value (connections left since)
	// is clamped back to the tail.
	i := w.sweepCursor
	if i < 0 || i >= len(w.liveConns) {
		i = len(w.liveConns) - 1
	}
	examined, moved, kicks := 0, 0, 0
	for ; i >= 0 && examined < iouSweepBudget; i-- {
		if w.transplant.Load() != h {
			break
		}
		if i >= len(w.liveConns) {
			continue
		}
		fd := w.liveConns[i]
		if fd < 0 || fd >= len(w.conns) {
			continue
		}
		cs := w.conns[fd]
		if cs == nil {
			continue
		}
		examined++
		// Work already owed on this connection: a reap on its way, a claim
		// its dispatch goroutine made, a response held for the hand-off at
		// its SEND completion. Each is a transient state something else
		// will resolve; re-examining adds nothing and could place a second
		// reap.
		if cs.closing || cs.transplantReap > 0 || cs.transplantPending.Load() || cs.transplantHold {
			w.cycleRes[resBusy]++
			w.cycleTransient++
			continue
		}
		// Permanent residue: a detached WebSocket or SSE conn, an H2 one, a
		// fixed-file one, one whose last hand-off failed at its dup. The
		// gates below all refuse it, so examining it is work with no
		// outcome — and for a detached conn the Broadcast further down
		// would be a spurious wake into the WebSocket chanReader's own
		// pause/resume machinery (celeris#667/#672), for a conn that can
		// never be offered. Counted under its class, which is what lets
		// the sweep go dormant with WS conns on the engine.
		if class := w.residualClass(cs); class != resBusy {
			w.cycleRes[class]++
			continue
		}
		if w.async && cs.asyncPromoted.Load() {
			// Owned by its dispatch goroutine. One Broadcast per drain
			// wakes a goroutine that parked before the drain was set, so
			// its own park-boundary check runs and it claims the hand-off.
			// On a worker that cannot reap, no promoted connection is ever
			// offered (asyncTransplantEligible), so a Broadcast could only
			// make it re-park: none is sent, and the connection is
			// permanent residue rather than a reason to keep sweeping.
			cs.asyncInMu.Lock()
			running := cs.asyncRun
			if w.asyncCancelFlags && running && len(cs.asyncInBuf) == 0 && cs.sweepKick != h {
				cs.sweepKick = h
				cs.asyncCond.Broadcast()
				kicks++
			}
			cs.asyncInMu.Unlock()
			if running {
				if w.asyncCancelFlags {
					w.cycleRes[resBusy]++
					w.cycleTransient++
				} else {
					w.cycleRes[resPinned]++
				}
				continue
			}
		}
		// No reap on this worker and a recv already armed: only the
		// connection's own next request can move it (see the file comment).
		// Do not ask, or every pass would count a reap it cannot place.
		if !w.asyncCancelFlags && cs.recvArmed {
			w.cycleRes[resPinned]++
			continue
		}
		w.tryTransplant(fd)
		if w.conns[fd] != cs {
			moved++
			continue
		}
		class := w.residualClass(cs)
		w.cycleRes[class]++
		if !residualPermanent(class) {
			w.cycleTransient++
		}
	}
	w.cycleMoved += moved + kicks

	if i < 0 {
		if w.cycleMoved == 0 && w.cycleTransient == 0 {
			w.sweepDormant = true
		}
		w.publishResidual()
		w.resetCycle()
		w.sweepCursor = -1
	} else {
		w.sweepCursor = i
	}

	if moved > 0 || kicks > 0 {
		w.sweepIvl = iouSweepMinIvl
	} else if w.sweepIvl < iouSweepMaxIvl {
		w.sweepIvl *= 2
		if w.sweepIvl > iouSweepMaxIvl {
			w.sweepIvl = iouSweepMaxIvl
		}
	}
	w.sweepNext = now + w.sweepIvl
}

func (w *Worker) resetCycle() {
	w.cycleMoved = 0
	w.cycleTransient = 0
	w.cycleRes = [numResidual]uint64{}
}

// publishResidual moves this worker's per-class residue into the engine-wide
// gauges as a delta against what it published last. Worker thread.
func (w *Worker) publishResidual() {
	if w.sweepCnt == nil {
		w.sweepPub = w.cycleRes
		return
	}
	for c := range w.cycleRes {
		if d := int64(w.cycleRes[c]) - int64(w.sweepPub[c]); d != 0 {
			w.sweepCnt.residual[c].Add(uint64(d))
		}
	}
	w.sweepPub = w.cycleRes
}

// residualClass names why the hand-off refused cs, reading the gates
// tryTransplant has just left it behind. Worker thread.
func (w *Worker) residualClass(cs *connState) int {
	if cs.fixedFile {
		return resPinned
	}
	if cs.h1State != nil && cs.h1State.Detached.Load() {
		return resDetached
	}
	if cs.h2State != nil || cs.asyncH2Promoted.Load() ||
		(cs.detected && engine.Protocol(cs.protocol.Load()) != engine.HTTP1) {
		return resH2
	}
	if cs.reapSuppressed && cs.recvArmed {
		// A hand-off of this connection failed at its dup and no reap is
		// placed for it until it next receives data.
		return resPinned
	}
	return resBusy
}
