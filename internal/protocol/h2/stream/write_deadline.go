package stream

import (
	"sync/atomic"
	"time"

	"golang.org/x/net/http2"
)

// The per-stream write deadline (celeris#889, D2).
//
// A response whose bytes wait for the peer's flow-control window (the tail of an
// inline handler's body, or a pool handler's, buffered in the stream's
// OutboundBuffer, Manager.outboundHeld) is bounded by WriteTimeout as a minimum
// rate, the rule #1119 gives a response parked on the socket: in a window of
// WriteTimeout the h2 layer must release at least min(64 KiB, what the stream
// held when the window began) of the stream's DATA, or the stream is reset with
// RST_STREAM(INTERNAL_ERROR), as net/http resets a stream at its write deadline
// and as #906 does for a pool handler that waits too long. There is no ceiling:
// a stream that takes its floor in every window is never cut, however long the
// response. The measure is bytes released (FlushOutbound), never frames: a PING,
// or a WINDOW_UPDATE that lets one byte out, moves nothing.
//
// What counts. A stream that released its floor in the window is held. The
// surplus beyond the floor carries to the next windows, up to writeBankWindows
// windows' worth (the bank), and a stream's first window is covered by the bytes
// it had in flight when it stalled (the seed, capped the same way): a client that
// sends few WINDOW_UPDATEs (nghttp2 and curl update when half of the window is
// consumed) releases in big steps, with windows between its updates that release
// nothing, and a rule that counted only the window it fell in reset it while it
// read far above the floor (review r7 F1). The guarantee is on the average: a stream
// that releases at least the floor per window on average, lagging that rate by no
// more than writeBankWindows windows' worth, survives however long its response.
//
// The connection's own rate matters too. A stream whose OWN send window has room
// but whose bytes wait for the CONNECTION window is credited with what the whole
// connection released in the window (a sibling takes it). And when the connection as
// a whole releases at least the floor, a stream that is below its own floor is not
// judged alone (review r7 F6): N parallel downloads that share a slow socket each
// get link/N, and the client reads the socket in the order the server sent, so a
// stream can wait behind its siblings' bytes for a window or more. It is held while
// it takes its share, at least half of floor/N (N the streams stalled), and held for
// up to writeIdleWindows consecutive windows in which it takes less (paused,
// dribbled or queued behind its siblings), and reset after that: the memory a
// stream that is not read pins is bounded by writeIdleWindows+1 windows, not by
// the siblings' life. When the connection itself releases less than the floor
// nothing is held (but a tail, below): the first window that does not move is the
// verdict, as for a single stream. A stream whose own window is shut is judged by its own releases
// and these allowances, even when the connection window is shut too, or a client
// could shut the conn window by dribbling to a sibling and shelter every stalled
// stream for ever.
//
// The end of a response is given a grace (tail grace, r8 F-A and r9 F-A2): the grant
// for the last bytes waits behind the siblings' bytes in the client's socket and can
// land after the window, and the backlog behind them can take more than a window to
// drain. A stream that holds under one floor and has had half of it released, and any
// stream while the whole connection holds under one floor, is held for up to
// writeIdleWindows windows without proof, reset in the next. The bank does not hide
// an unread stream: a window held on the bank alone pays a whole floor, however
// small the rest, so a response a little longer than the window is let go in the
// same number of windows as a long one (r9 F-H).
//
// State: a record per stalled stream, in a small map on the Manager, made on the
// first stall of the connection. Only a stream that holds buffered bytes has one,
// which is the rare case (a client that let its window run out), so no field
// costs every stream of every connection, and the common send touches nothing
// (noteBuffered is called where a buffer fills, not where DATA is sent). The map
// is guarded by stallMu, a leaf lock below a stream's mu (the buffering and
// flushing paths hold it) and below the manager's; the judgement takes it alone.
// The sweep reads one hint, DeadlineHints.WrDL, per connection.

// writeMinRelease is the floor of the rate: the bytes a stalled stream must
// release per WriteTimeout window to be held another (celeris#885's
// parkedMinAcked, at the h2 layer): 64 KiB per window, about 64 KiB/s at a 1 s
// WriteTimeout and about 1 KiB/s at the 60 s default.
const writeMinRelease = 64 << 10

// writeBankWindows is how many windows' worth of surplus release a stream may carry
// to later windows (and how many its in-flight bytes may cover at the start): the
// bound on how far behind the floor's rate a client's WINDOW_UPDATEs may lag.
const writeBankWindows = 4

// writeIdleWindows is how many consecutive windows a stream that takes less than its
// share may be held, on a connection that moves above the floor, before it is reset.
const writeIdleWindows = 3

// writeIdleCapWindows is the most idle windows a stream below its share may be
// held, on a connection that moves, when the connection has a long queue of bytes
// in flight (celeris#1170): the allowance is ceil(in flight / what the connection
// released in the window) windows, at least writeIdleWindows and at most this. A
// stream that waits behind its siblings' bytes in the client's socket is not
// idle for as long as the client takes to read what was sent ahead of it, and
// no longer. The cost is the bound of a stream that is truly paused or abandoned
// while the connection moves: 4 (the seed) + this + 1 windows. The one place the
// bound of the minimum rate is stretched; see the PR for #1170.
const writeIdleCapWindows = 16

// maxTimeout bounds a deadline length so that now + timeout cannot overflow an
// int64 of nanoseconds (a timeout of math.MaxInt64, the common way to write "never",
// made every deadline due at once: review r7 F3): a hundred years.
const maxTimeout = 100 * 365 * 24 * time.Hour

// clampTimeout is d, or maxTimeout if it is longer.
func clampTimeout(d time.Duration) time.Duration {
	if d > maxTimeout {
		return maxTimeout
	}
	return d
}

// stall is the record of a stream that holds buffered bytes.
type stall struct {
	armedAt  int64 // when the current window began (wall clock ns)
	held     int   // bytes the stream held when the window began
	pending  int   // bytes it holds now
	released int   // bytes of its own released in the window
	bank     int   // surplus released in earlier windows (and the seed), carried over
	idle     int   // consecutive windows held below its share on a connection above the floor
	connAt   int64 // Manager.connReleased when the window began
	connHeld int64 // what the connection held (buffered and waiting) when the window began
	idleMax  int   // idle windows allowed in the current idle run, fixed when it begins
	waiter   bool  // a pool handler blocked for window, not a buffer (write_wait.go): judged by its handler
}

// SetWriteTimeout sets the write deadline (the server's WriteTimeout): the length
// of the window a stalled stream is judged over. 0 or less turns it off. Set once,
// before the first frame.
func (m *Manager) SetWriteTimeout(d time.Duration) { m.wrTimeout = clampTimeout(d) }

// WriteTimeout is the window SetWriteTimeout set.
func (m *Manager) WriteTimeout() time.Duration { return m.wrTimeout }

// NextWriteDeadline is the earliest end of a stalled stream's window, or 0 if
// none is stalled (or the hint is stale after a stall ended: ExpiredWrites
// settles it). One atomic load; the engines' sweep reads it for every h2 conn.
func (m *Manager) NextWriteDeadline() int64 { return m.hints.WrDL.Load() }

// noteBuffered records that n bytes were just added to the buffer of s, already
// charged to outboundHeld: the stream is stalled from here, and its window begins
// if it was not already, seeded with the bytes it had in flight (sent, not yet
// granted back: the peer's initial window less what is left of it), at most
// writeBankWindows floors, which cover the time the client needs to consume them
// before its first update. Called with the stream's lock held, on the event loop or
// on a pool goroutine.
func (m *Manager) noteBuffered(s *Stream, n int) {
	if m.wrTimeout <= 0 || n <= 0 {
		return
	}
	id := s.ID
	m.stallMu.Lock()
	st := m.stalls[id]
	if st != nil && st.waiter {
		// A stream is a waiter or a buffer, never both (a pool handler that waits
		// buffers nothing). If it ever were, the sweep judges it: the waiter's
		// judge defers to a record that is not its own (judgeWait).
		st.waiter = false
		m.lowerWrDL(st.armedAt + int64(m.wrTimeout))
	}
	if st == nil {
		now := time.Now().UnixNano() // read only when a window begins, not for every append to a stalled stream
		if m.stalls == nil {
			m.stalls = make(map[uint32]*stall)
		}
		st = &stall{armedAt: now, bank: m.seedLocked(s), connAt: m.connReleased.Load(), connHeld: m.heldLocked(int64(n))}
		m.stalls[id] = st
		m.stallN.Store(int32(len(m.stalls)))
		m.lowerWrDL(now + int64(m.wrTimeout))
	}
	st.pending += n
	if st.held == 0 { // the window's first bytes
		st.held = st.pending
	}
	m.stallMu.Unlock()
}

// lowerWrDL lowers the hint to at, if at is earlier than what it holds.
func (m *Manager) lowerWrDL(at int64) {
	for {
		cur := m.hints.WrDL.Load()
		if cur != 0 && cur <= at {
			return
		}
		if m.hints.WrDL.CompareAndSwap(cur, at) {
			return
		}
	}
}

// noteReleased records that n bytes of the stream's buffer were just sent
// (FlushOutbound), with the stream's lock held: the stream's window is credited,
// and the record ends with the buffer.
func (m *Manager) noteReleased(s *Stream, n int) {
	if m.wrTimeout <= 0 || n <= 0 {
		return
	}
	m.stallMu.Lock()
	if st := m.stalls[s.ID]; st != nil && !st.waiter {
		st.released += n
		st.pending -= n
		if st.pending <= 0 {
			m.delStallLocked(s.ID)
			s.recordEnded() // s.mu is held: the next record's seed counts what is sent from here
		}
	}
	m.stallMu.Unlock()
}

// seedLocked is the bank a stream's first window starts with: the bytes it had in
// flight (sent, not yet granted back: the peer's initial window less what is left
// of its own), at most writeBankWindows floors, and no more than the stream has
// sent since its previous record ended (Stream.seedSince). The last bound is for a streaming
// response whose Writes each wait: without it every record was seeded again with
// the same bytes the client never granted back, and a client that granted each
// Write's bytes once in five windows held the stream at a fifth of the floor for
// ever (celeris#1170 review r2 F2). A stream with no previous record has the first
// two bounds only. The stream's lock (for seedSince) must be held.
func (m *Manager) seedLocked(s *Stream) int {
	seed := min(max(int(m.initWin.Load())-int(s.windowSize.Load()), 0), writeBankWindows*writeMinRelease)
	if s.seedSince > 0 {
		seed = min(seed, int(s.seedSince)-1)
	}
	return seed
}

// heldLocked is what the connection holds waiting for the peer: the bytes its
// streams buffer and the bytes its pool handlers still have to send, and at
// least own (the stream judged). stallMu must be held.
func (m *Manager) heldLocked(own int64) int64 {
	return max(m.outboundHeld.Load()+m.waitHeldLocked(), own)
}

// waitHeldLocked is the bytes the pool handlers that wait for window still have to
// send: the sum of their records' pending. A scan of the stalled streams (at most
// the connection's concurrent streams), made where a window begins, not on any path
// that sends. stallMu must be held.
func (m *Manager) waitHeldLocked() int64 {
	var n int64
	for _, st := range m.stalls {
		if st.waiter {
			n += int64(st.pending)
		}
	}
	return n
}

// delStallLocked ends the record of stream id. stallMu must be held.
func (m *Manager) delStallLocked(id uint32) {
	if _, ok := m.stalls[id]; ok {
		delete(m.stalls, id)
		m.stallN.Store(int32(len(m.stalls)))
	}
}

// dropStall ends the record of stream id, whose buffer was dropped or whose
// stream has left the connection. A load of stallN when no stream is stalled.
func (m *Manager) dropStall(id uint32) {
	if m.stallN.Load() == 0 {
		return
	}
	m.stallMu.Lock()
	m.delStallLocked(id)
	m.stallMu.Unlock()
}

// dropAllStalls ends every record (Close).
func (m *Manager) dropAllStalls() {
	m.stallMu.Lock()
	clear(m.stalls)
	m.stallN.Store(0)
	m.stallMu.Unlock()
}

// stalledStreams is how many streams hold a record. For tests and the leak check.
func (m *Manager) stalledStreams() int {
	m.stallMu.Lock()
	defer m.stallMu.Unlock()
	return len(m.stalls)
}

// creditConn counts n bytes of DATA the connection released (ReserveSendWindow),
// for the judgement of a stream starved by the connection window. Only while a
// stream is stalled: one atomic load otherwise.
func (m *Manager) creditConn(n int32) {
	if n > 0 && m.stallN.Load() != 0 {
		m.connReleased.Add(int64(n))
	}
}

// ExpiredWrites appends to out, up to its capacity, the IDs of the streams whose
// window has ended and who did not release their floor in it, and renews the
// window of those that did. It settles the hint to the earliest end of the windows
// that remain. Called by the loop thread under H2State.mu when NextWriteDeadline
// says one is due; it takes no lock another path holds while it takes stallMu: the
// records due are copied out under stallMu alone, the streams' windows are read
// under the manager's read lock alone, and the verdicts are applied under stallMu
// alone again. A record whose stream is gone from the connection ends here (the
// removal paths end theirs; this is the net). If out is full the hint is left due,
// so the next sweep takes the rest.
func (m *Manager) ExpiredWrites(now int64, out []uint32) []uint32 {
	wt := int64(m.wrTimeout)
	if wt <= 0 {
		m.hints.WrDL.Store(0)
		return out
	}
	var idBuf [32]uint32
	var stBuf [32]*stall
	ids, sts := idBuf[:0], stBuf[:0]
	m.stallMu.Lock()
	for id, st := range m.stalls {
		if !st.waiter && st.armedAt+wt <= now { // a waiter is judged by its handler
			ids, sts = append(ids, id), append(sts, st)
		}
	}
	m.stallMu.Unlock()

	// The streams' windows, under the manager's lock alone.
	var viewBuf [32]uint8
	views := viewBuf[:0]
	const (
		viewGone = 1 << iota // the stream is not in the map
		viewOwn              // its own send window has room
	)
	if len(ids) > 0 {
		m.mu.RLock()
		for _, id := range ids {
			var v uint8
			if s, ok := m.streams[id]; !ok {
				v = viewGone
			} else if s.windowSize.Load() > 0 {
				v = viewOwn
			}
			views = append(views, v)
		}
		m.mu.RUnlock()
	}
	connShut := atomic.LoadInt32(&m.connectionWindow) <= 0

	full := false
	m.stallMu.Lock()
	for i, id := range ids {
		st := m.stalls[id]
		// The record judged is the one found due: one that ended and began again
		// while the windows were read (a pool goroutine flushed the buffer whole and
		// buffered a new tail) is another window, not due (review r7 F2).
		if st == nil || st != sts[i] || st.armedAt+wt > now {
			continue
		}
		if st.waiter {
			continue // became a waiter's since it was found due
		}
		if views[i]&viewGone != 0 {
			m.delStallLocked(id)
			continue
		}
		if !m.holdsLocked(st, views[i]&viewOwn != 0 && connShut, now) {
			if len(out) == cap(out) {
				full = true
				continue
			}
			out = append(out, id)
		}
	}
	var next int64
	for _, st := range m.stalls {
		if st.waiter {
			continue // the hint is the sweep's: a waiter's window is its handler's timer
		}
		if at := st.armedAt + wt; next == 0 || at < next {
			next = at
		}
	}
	if full {
		next = now // due again at the next sweep
	}
	m.stallN.Store(int32(len(m.stalls)))
	m.hints.WrDL.Store(next) // under stallMu, as noteBuffered lowers it: an arm is not lost
	m.stallMu.Unlock()
	return out
}

// holdsLocked is the verdict on the stalled stream st whose window has ended at
// now: it reports whether the stream is held, and if so begins its next window.
// starved says the stream's own window has room and the connection's is shut.
// m.stallMu must be held. The rule, tail grace included, is in the file's comment.
func (m *Manager) holdsLocked(st *stall, starved bool, now int64) bool {
	connRel := m.connReleased.Load() - st.connAt
	connFloor := min(int64(writeMinRelease), st.connHeld)
	moving := connRel >= connFloor // the connection as a whole released its floor

	hold := false
	switch {
	case starved:
		// Waiting for the connection window, which is no fault of the stream's own:
		// its credit is the connection's.
		hold = moving
		st.idle = 0
	default:
		total := st.released + st.bank
		floor := min(writeMinRelease, st.held)
		switch {
		case total >= floor:
			hold = true
			charge := floor
			if st.released < floor {
				// Held on the bank alone: the window pays a whole floor, not the
				// (smaller) rest, or a stream whose rest is under a floor and that is
				// never read would spend a few bytes a window of its bank and live
				// for tens of thousands of windows (r9 F-H).
				charge = writeMinRelease
			}
			st.bank = min(max(total-charge, 0), writeBankWindows*writeMinRelease)
			st.idle = 0
		case moving:
			share := int(connFloor) / (2 * max(len(m.stalls), 1))
			if st.released > 0 && st.released >= share {
				hold = true
				st.idle = 0
			} else {
				if st.idle == 0 { // an idle run begins: its allowance is fixed now
					st.idleMax = m.idleAllowance(connRel)
				}
				if st.idle++; st.idle <= st.idleMax {
					hold = true
				}
			}
			st.bank = 0
		case (st.held < writeMinRelease && st.released*2 >= floor) || st.connHeld < writeMinRelease:
			// A tail. What is held (by the stream, or by the whole connection) is under
			// one floor, and "all of it in one window" is brittle: the grant for the last
			// bytes of a response waits behind its siblings' bytes in the socket and can
			// land just after the window (r8 F-A: reset 8 to 16 bytes before the end of
			// a download that took 21 s). For a stream whose own rest is small, half of
			// it released is a grace window; when the whole connection holds under one
			// floor, the end of the transfer is on the wire and the drain of its
			// backlog can take more than one window (r9 F-A2: 9 of 20 parallel downloads
			// reset 2 to 8 bytes from the end), so the connection's tail is held without
			// that proof. Both are inside the idle bound (writeIdleWindows).
			if st.idle++; st.idle <= writeIdleWindows {
				hold = true
			}
			st.bank = 0
		}
	}
	if hold {
		st.armedAt, st.held, st.released = now, st.pending, 0
		st.connAt, st.connHeld = m.connReleased.Load(), m.heldLocked(int64(st.pending))
	}
	return hold
}

// expireWrite resets the stalled stream id: the bytes it holds are dropped
// (refunded to the budget), reset is called with the stream so that the
// RST_STREAM is queued behind the frames the stream already queued, and the
// stream leaves the connection, a pool handler still running on it retiring it as
// for a reset from the peer (takeLocked). Everything happens under m.mu, while the
// stream is in the map, as resetStream does (celeris#951). It reports whether the
// stream was there and was reset. A stream that is already cancelled (a pool
// handler's own reset at #906's bound, or the peer's) is taken out without a
// second RST_STREAM.
func (m *Manager) expireWrite(id uint32, reset func(*Stream)) bool {
	m.mu.Lock()
	s, ok := m.streams[id]
	if !ok {
		m.mu.Unlock()
		m.dropStall(id)
		return false
	}
	cancelled := s.IsCancelled()
	s.mu.Lock()
	buf := s.OutboundBuffer
	if buf == nil || buf.Len() == 0 {
		// Nothing is held any more: the response went out whole between the verdict
		// and here (a pool goroutine flushed it). A reset now would follow its
		// END_STREAM (review r7 F2).
		s.mu.Unlock()
		m.mu.Unlock()
		m.dropStall(id)
		return false
	}
	m.refundOutbound(buf.Len())
	buf.Reset()
	s.OutboundEndStream = false
	s.outboundAbandoned = false
	s.mu.Unlock()
	if !cancelled && reset != nil {
		reset(s)
	}
	s.Cancel()
	release := m.takeLocked(id, s)
	m.mu.Unlock()
	m.afterTake(id, s, release)
	return !cancelled
}

// ExpireWrite resets stream id, whose write window passed without its floor, with
// RST_STREAM(INTERNAL_ERROR) behind the frames it has queued, and remembers the
// identifier as a reset one, so that the client's late frames for it (a
// WINDOW_UPDATE, a RST_STREAM, a PRIORITY) are ignored as for any stream the
// server reset. No response is written: the headers and part of the body are out.
// Not charged to the refusal budgets: it is the server's decision. Called by the
// loop thread under H2State.mu.
func (p *Processor) ExpireWrite(id uint32) {
	if p.manager.expireWrite(id, p.resetStalled) {
		p.resetIDs.mark(id)
	}
}

// resetStalled queues the RST_STREAM of an expired write behind the stream's
// frames through the connection's writer when it can (Resetter), and writes it
// straight to the connection otherwise (a test recorder).
func (p *Processor) resetStalled(s *Stream) {
	switch w := p.connWriter.(type) {
	case Resetter:
		w.ResetStream(s, http2.ErrCodeInternal) // cancels the stream as well
	case nil:
	default:
		_ = w.WriteRSTStreamPriority(s.ID, http2.ErrCodeInternal)
	}
}
