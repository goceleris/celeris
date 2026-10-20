//go:build linux

package epoll

// The two ways an HTTP/1 request ends the conn's HTTP/1 life, shared by every
// place that holds the verdict of ProcessH1 or DispatchBufferedBody: the recv
// path (drainRead: its main block, the direct-body block's own verdict and
// its tail) and the transplant replay (endCarriedReplay, celeris#908). They
// used to be copies, and a copy that was fixed in one place went on being
// wrong in the others (celeris#885 items 16 and 17).

// finishH1Upgrade completes an h2c upgrade whose 101 has been queued: the
// verdict was conn.ErrUpgradeH2C. It switches the conn to HTTP/2 (writeFn is
// the write function the H2 state keeps; the preface and the answer to the
// upgrade request are queued by then), then sends what is queued. It reports
// false when the switch or the send failed and the conn was closed, true when
// the conn goes on as HTTP/2.
//
// A send that does not fit the kernel's buffer arms EPOLLOUT, as every partial
// send does since celeris#885 item 42 (the dirty list holds the loop at a 0 ms
// epoll_wait).
//
// Loop thread, and no dispatch goroutine exists for the conn: the callers are
// drainRead's inline ProcessH1 (a conn not yet promoted to its goroutine, or a
// sync one), its body-recv block (sync only) and a fresh adoption. The
// goroutine's own upgrade is switchToH2Local. So noteWriteProgress, which
// consumes goMoved (under detachMu otherwise), needs no lock here
// (celeris#885).
func (l *Loop) finishH1Upgrade(cs *connState, fd int, writeFn func([]byte)) bool {
	if err := l.switchToH2(cs, writeFn); err != nil {
		l.closeConn(fd)
		return false
	}
	if cs.writePos < len(cs.writeBuf) {
		queued := 0
		if cs.writeSince != 0 {
			queued = csPendingBytes(cs) // before this flush (celeris#885)
		}
		if fErr := l.flushWrites(cs, true); fErr != nil {
			l.closeConn(fd)
			return false
		}
		if cs.writePos >= len(cs.writeBuf) {
			cs.pendingBytes = 0
			cs.endWrite()
			if cs.dirty {
				l.removeDirty(cs)
			}
		} else {
			// The 101 and the preface did not all fit: the send buffer is
			// full.
			cs.pendingBytes = len(cs.writeBuf) - cs.writePos
			cs.noteWriteProgress(queued, cs.pendingBytes)
			l.armEpollOut(cs)
		}
	}
	return true
}

// endH1Verdict ends a conn whose request returned a verdict that is neither
// ErrHijacked (the handler took the conn and cs is released: callers return
// before this) nor ErrUpgradeH2C (finishH1Upgrade): a parse error,
// errConnectionClose, a refused write. It flushes what ProcessH1 queued (the
// 400, or the response to a Connection: close request) and tells a detached
// middleware (OnError), both under detachMu, then closes once what the one
// write could not send (a response larger than the socket buffers) has gone
// out (closeWhenFlushed, celeris#761). The lock is released before the close,
// which takes it again.
//
// internal/engine/iouring's endH1Verdict does the same.
func (l *Loop) endH1Verdict(cs *connState, err error) {
	if mu := cs.detachMu; mu != nil {
		mu.Lock()
	}
	_ = l.flushWrites(cs, true)
	cs.pendingBytes = 0
	if cs.h1State != nil && cs.h1State.OnError != nil {
		cs.h1State.OnError(err)
	}
	if mu := cs.detachMu; mu != nil {
		mu.Unlock()
	}
	l.closeWhenFlushed(cs)
}
