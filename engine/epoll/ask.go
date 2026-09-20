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
// dropAsk runs on the loop thread at every release site, before the Put.

// askTransplant queues an examination request for cs. Called by cs's dispatch
// goroutine at its park, holding cs.asyncInMu, after winning the cs.xferAsked
// CAS. xferAskMu is a leaf lock: nothing is taken under it, and the loop's own
// state is not touched here.
func (l *Loop) askTransplant(cs *connState) {
	l.xferAskMu.Lock()
	l.xferAskQ = append(l.xferAskQ, cs)
	l.xferAskPending.Store(1)
	l.xferAskMu.Unlock()
	l.wakeFD.Signal()
}

// drainTransplantAsks examines every connection that asked. Loop thread,
// immediately before drainDetachQueue, so a deferred async hand-off it starts
// is finished in the same iteration when the goroutine exits.
func (l *Loop) drainTransplantAsks() {
	if l.xferAskPending.Load() == 0 {
		return
	}
	l.xferAskMu.Lock()
	q := l.xferAskQ
	l.xferAskQ = l.xferAskSpare[:0]
	l.xferAskPending.Store(0)
	l.xferAskMu.Unlock()
	for _, cs := range q {
		if cs == nil {
			continue // withdrawn by dropAsk before its connState was released
		}
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
	clear(q)
	l.xferAskSpare = q[:0]
}

// dropAsk withdraws any queued ask naming cs. Loop thread, at every site that
// returns a connState to the pool, BEFORE the Put: the queue must never name a
// connState the pool can hand to another connection. Only cs's own dispatch
// goroutine sets xferAsked, and a connState is released only after that
// goroutine has exited, so no new ask can race this.
func (l *Loop) dropAsk(cs *connState) {
	if !cs.xferAsked.Load() {
		return
	}
	l.xferAskMu.Lock()
	for i, q := range l.xferAskQ {
		if q == cs {
			l.xferAskQ[i] = nil
		}
	}
	l.xferAskMu.Unlock()
	cs.xferAsked.Store(false)
}
