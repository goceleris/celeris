//go:build linux

package epoll

// The park-boundary ask (celeris#657, P8/A1).
//
// A promoted async connection is movable only while its dispatch goroutine is
// PARKED with an empty input buffer — for a back-to-back keep-alive client,
// the short gap between a response and the next request. A periodic sweep
// finds such a connection movable with a probability equal to that idle
// fraction, which is small exactly for the slow handlers async dispatch exists
// for: with a 5 ms handler and the sweep alone, the outgoing engine still held
// most of its connections a second after the switch in 7 of 8 runs, and none
// in 8 of 8 with this.
//
// So the goroutine ASKS at the park itself, and the loop decides. The
// goroutine appends its connState to a per-loop queue under a leaf lock,
// deduplicated by a CAS on cs.xferAsked, and signals the wake eventfd. The
// loop drains the queue on its own thread, immediately before
// drainDetachQueue, re-validates that the asking connState still owns its
// slot, and runs the unchanged tryTransplant — which re-checks parked && idle
// under asyncInMu, so the ask is a hint and never a decision.
//
// The release belt is what keeps it safe. connStates go back to a
// package-global sync.Pool, so an ask that outlived its connState would make
// the loop read a cs another connection now owns (the celeris#654 class).
// dropAsk runs on the loop thread at every release site, before the Put, and
// covers BOTH queues the loop can still read: the one goroutines append to
// and the batch the loop is walking (celeris#657 R2, MINOR-b).

// askAtPark is runAsyncHandler's park-boundary ask. Dispatch goroutine,
// holding cs.asyncInMu, at the top of the park loop.
//
// The permanent-class pre-check is the sweep's, on this side of the fence
// (celeris#657 R2, MINOR-a). Without it, while a drain is set, EVERY park of
// EVERY promoted async connection costs a queue entry and an eventfd write —
// including detached WebSocket and SSE connections, which park once per frame
// delivered, and H2-bound ones. tryTransplant refuses all of those on gates
// that cannot change while the connection lives (transplant.go), so the ask
// could only ever produce a refusal; the sweep has exactly this pre-check
// (sweep.go) and for exactly this reason.
//
// It reads only what THIS goroutine owns: cs.h1State and cs.h2State are
// written by switchToH2Local on this goroutine, and Detached and
// asyncH2Promoted are atomics. cs.protocol, cs.detected and cs.hijacked are
// the loop thread's and are deliberately NOT read here — the sweep, which
// runs there, screens those.
func (l *Loop) askAtPark(cs *connState) {
	if l.transplant.Load() == nil {
		return
	}
	if cs.h1State == nil || cs.h2State != nil || cs.asyncH2Promoted.Load() {
		return // H2-bound, or mid-upgrade: never offered
	}
	if cs.h1State.Detached.Load() {
		return // WebSocket or SSE: never offered while it lives
	}
	if cs.xferAsked.CompareAndSwap(false, true) {
		l.askTransplant(cs)
	}
}

// askTransplant queues an examination request for cs. Called by cs's dispatch
// goroutine at its park, holding cs.asyncInMu, after winning the cs.xferAsked
// CAS. xferAskMu is a leaf lock: nothing is taken under it, and the loop's own
// state is not touched here.
//
// INVARIANT (celeris#657 R2, MINOR-b), maintained under xferAskMu by every
// site that changes either side of it, and asserted by
// TestAskPendingCountsTheQueue:
//
//	l.xferAskPending == the number of NON-NIL entries in l.xferAskQ.
//
// It is read unlocked as a fast path in drainTransplantAsks. That is sound in
// the only direction that matters: a reader that races the Add below sees 0,
// skips the drain, and is woken again by the Signal that follows — the entry
// cannot be missed, only deferred by one iteration.
func (l *Loop) askTransplant(cs *connState) {
	l.xferAskMu.Lock()
	l.xferAskQ = append(l.xferAskQ, cs)
	l.xferAskPending.Add(1)
	l.xferAskMu.Unlock()
	l.wakeFD.Signal()
}

// drainTransplantAsks examines every connection that asked. Loop thread,
// immediately before drainDetachQueue, so a deferred async hand-off it starts
// is finished in the same iteration when the goroutine exits.
//
// The batch is parked on l.xferAskDrain rather than held in this frame,
// because examining one entry can re-enter the loop's release paths (the
// hand-off's AdoptConn runs here, teardown runs here) and dropAsk must be able
// to withdraw an entry the walk has not reached yet. Each entry is niled as it
// is CONSUMED, before anything is done with it, so dropAsk and the walk can
// never both act on the same one.
func (l *Loop) drainTransplantAsks() {
	if l.xferAskPending.Load() == 0 {
		return
	}
	l.xferAskMu.Lock()
	l.xferAskDrain = l.xferAskQ
	l.xferAskQ = l.xferAskSpare[:0]
	l.xferAskPending.Store(0)
	l.xferAskMu.Unlock()
	for i := range l.xferAskDrain {
		cs := l.xferAskDrain[i]
		if cs == nil {
			continue // withdrawn by dropAsk before its connState was released
		}
		l.xferAskDrain[i] = nil // consumed: dropAsk has nothing left to withdraw
		cs.xferAsked.Store(false)
		if l.transplant.Load() == nil {
			continue
		}
		fd := cs.fd
		// The slot check is what makes the ask safe to act on: only a
		// connState that still owns its fd on this loop is examined.
		if fd < 0 || fd >= len(l.conns) || l.conns[fd] != cs {
			continue
		}
		l.tryTransplant(fd)
	}
	q := l.xferAskDrain
	l.xferAskDrain = nil
	l.xferAskSpare = q[:0]
}

// dropAsk withdraws any queued ask naming cs. Loop thread, at every site that
// returns a connState to the pool, BEFORE the Put: no queue the loop can still
// read may name a connState the pool can hand to another connection.
//
// Both queues, not one. xferAskQ is where goroutines append; xferAskDrain is
// the batch a drain in progress is walking, and a release can happen from
// inside that walk — tryTransplant hands a connection over and the target's
// AdoptConn runs on this thread, teardown runs on this thread. Covering only
// xferAskQ left the rest to a reachability argument that was never written
// down and that the code did not enforce (celeris#657 R2, MINOR-b).
//
// Only cs's own dispatch goroutine sets xferAsked, and a connState is released
// only after that goroutine has exited, so no new ask can race this.
func (l *Loop) dropAsk(cs *connState) {
	if !cs.xferAsked.Load() {
		return
	}
	// xferAskDrain is loop-thread-only; dropAsk runs on the loop thread, as
	// does the walk, so no lock is owed for it.
	for i, q := range l.xferAskDrain {
		if q == cs {
			l.xferAskDrain[i] = nil
		}
	}
	l.xferAskMu.Lock()
	for i, q := range l.xferAskQ {
		if q == cs {
			l.xferAskQ[i] = nil
			l.xferAskPending.Add(^uint32(0)) // keep the count == non-nil entries
		}
	}
	l.xferAskMu.Unlock()
	cs.xferAsked.Store(false)
}
