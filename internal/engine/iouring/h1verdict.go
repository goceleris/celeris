//go:build linux

package iouring

// The two ways an HTTP/1 request ends the conn's HTTP/1 life, shared by every
// place that holds the verdict of ProcessH1 or DispatchBufferedBody: the recv
// path (handleRecv: its main block, the direct-body block's own verdict and
// its tail) and the transplant replay (endCarriedReplay, celeris#908). They
// used to be copies, and a copy that was fixed in one place went on being
// wrong in the others (celeris#884 items 37 and 39).

// finishH1Upgrade completes an h2c upgrade whose 101 has been queued: the
// verdict was conn.ErrUpgradeH2C. It switches the conn to HTTP/2 (the
// preface and the answer to the upgrade request are queued by then), then
// sends what is queued under detachMu. It reports false when the switch
// failed and the conn was closed, true when the conn goes on as HTTP/2.
//
// link says whether the send may be chained to a recv of the conn's own
// (flushSendLink, single-shot recv only): the recv path's callers pass true,
// and then must arm a recv themselves unless cs.recvLinked says the chain
// carries one (nor may they arm a second beside a linked one: two recvs on
// one fd split H2 frames across CQEs and lose END_STREAM, observed as flaky
// TestH2CUpgradeSubsequentStreams/iouring). The transplant replay passes
// false: it arms its one recv itself, and a SEND linked to a recv of its own
// would put a second recv on the fd. A sent-nothing flush (the SQ ring was
// full) marks the conn dirty so the dirty loop retries it.
func (w *Worker) finishH1Upgrade(cs *connState, fd int, link bool) bool {
	if err := w.switchToH2(cs); err != nil {
		w.closeConn(fd)
		return false
	}
	mu := cs.detachMu
	if mu != nil {
		mu.Lock()
	}
	cs.recvLinked = false
	if link && w.bufRing == nil {
		if w.flushSendLink(cs) {
			w.markDirty(cs)
		}
	} else {
		if w.flushSend(cs) {
			w.markDirty(cs)
		}
	}
	if mu != nil {
		mu.Unlock()
	}
	return true
}

// endH1Verdict ends a conn whose request returned a verdict that is neither
// ErrHijacked (the handler took the conn: callers return before this) nor
// ErrUpgradeH2C (finishH1Upgrade): a parse error, errConnectionClose, a
// refused write. It flushes what ProcessH1 queued (the 400, or the response
// to a Connection: close request) and tells a detached middleware (OnError),
// both under detachMu, then closes, which defers behind the send until the
// response has gone out.
//
// The flush was once outside the lock. completeSend's docstring states the
// invariant it broke: for a detached connection cs.sendBuf is read by the
// makeWriteFn closure on the user's goroutine, so every mutation of it needs
// detachMu. flushSend swaps sendBuf with writeBuf and sets cs.sending, so
// calling it unlocked races a handler that is writing, the same access
// pattern the race detector caught on the call sites fixed when that
// docstring was written (celeris#528). Reachable within one request: a
// handler can Detach (SSE, or a WebSocket upgrade) and then have ProcessH1
// surface an error, with the guarded write closure already installed.
//
// The h1State read has its own reason to be under the lock: the async
// dispatch goroutine's switchToH2Local nils cs.h1State under the same mutex
// (the #256 TOCTOU class).
//
// internal/engine/epoll's endH1Verdict does the same: one lock spanning the
// flush and OnError. The lock is released before the close, which takes it
// again.
func (w *Worker) endH1Verdict(cs *connState, fd int, err error) {
	if mu := cs.detachMu; mu != nil {
		mu.Lock()
		_ = w.flushSend(cs)
		if cs.h1State != nil && cs.h1State.OnError != nil {
			cs.h1State.OnError(err)
		}
		mu.Unlock()
	} else {
		_ = w.flushSend(cs)
	}
	w.closeConn(fd)
}
