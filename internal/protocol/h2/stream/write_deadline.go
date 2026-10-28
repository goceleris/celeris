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
// A stream whose own send window has room but whose bytes wait for the
// CONNECTION window is not waiting for anything of its own: its credit is what
// the whole connection released in the window (reserveCredit). A stream whose own
// window is shut is judged by its own releases, even when the connection window
// is shut too, or a client could shut the conn window by dribbling to a sibling
// and shelter every stalled stream.
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

// stall is the record of a stream that holds buffered bytes.
type stall struct {
	armedAt  int64 // when the current window began (wall clock ns)
	held     int   // bytes the stream held when the window began
	pending  int   // bytes it holds now
	released int   // bytes of its own released in the window
	connAt   int64 // Manager.connReleased when the window began
	connHeld int64 // Manager.outboundHeld when the window began
}

// SetWriteTimeout sets the write deadline (the server's WriteTimeout): the length
// of the window a stalled stream is judged over. 0 or less turns it off. Set once,
// before the first frame.
func (m *Manager) SetWriteTimeout(d time.Duration) { m.wrTimeout = d }

// WriteTimeout is the window SetWriteTimeout set.
func (m *Manager) WriteTimeout() time.Duration { return m.wrTimeout }

// NextWriteDeadline is the earliest end of a stalled stream's window, or 0 if
// none is stalled (or the hint is stale after a stall ended: ExpiredWrites
// settles it). One atomic load; the engines' sweep reads it for every h2 conn.
func (m *Manager) NextWriteDeadline() int64 { return m.hints.WrDL.Load() }

// noteBuffered records that n bytes were just added to the buffer of stream id,
// already charged to outboundHeld: the stream is stalled from here, and its
// window begins if it was not already. Called with the stream's lock held, on the
// event loop or on a pool goroutine.
func (m *Manager) noteBuffered(id uint32, n int) {
	if m.wrTimeout <= 0 || n <= 0 {
		return
	}
	m.stallMu.Lock()
	st := m.stalls[id]
	if st == nil {
		now := time.Now().UnixNano() // read only when a window begins, not for every append to a stalled stream
		if m.stalls == nil {
			m.stalls = make(map[uint32]*stall)
		}
		st = &stall{armedAt: now, connAt: m.connReleased.Load(), connHeld: max(m.outboundHeld.Load(), int64(n))}
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
func (m *Manager) noteReleased(id uint32, n int) {
	if m.wrTimeout <= 0 || n <= 0 {
		return
	}
	m.stallMu.Lock()
	if st := m.stalls[id]; st != nil {
		st.released += n
		st.pending -= n
		if st.pending <= 0 {
			delete(m.stalls, id)
			m.stallN.Store(int32(len(m.stalls)))
		}
	}
	m.stallMu.Unlock()
}

// dropStall ends the record of stream id, whose buffer was dropped or whose
// stream has left the connection. A load of stallN when no stream is stalled.
func (m *Manager) dropStall(id uint32) {
	if m.stallN.Load() == 0 {
		return
	}
	m.stallMu.Lock()
	if _, ok := m.stalls[id]; ok {
		delete(m.stalls, id)
		m.stallN.Store(int32(len(m.stalls)))
	}
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
	ids := idBuf[:0]
	m.stallMu.Lock()
	for id, st := range m.stalls {
		if st.armedAt+wt <= now {
			ids = append(ids, id)
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
		if st == nil {
			continue
		}
		if views[i]&viewGone != 0 {
			delete(m.stalls, id)
			continue
		}
		credit, floor := st.released, min(writeMinRelease, st.held)
		if views[i]&viewOwn != 0 && connShut { // starved by the connection window
			credit, floor = int(m.connReleased.Load()-st.connAt), int(min(writeMinRelease, st.connHeld))
		}
		if credit >= floor {
			st.armedAt, st.held, st.released = now, st.pending, 0
			st.connAt, st.connHeld = m.connReleased.Load(), max(m.outboundHeld.Load(), int64(st.pending))
			continue
		}
		if len(out) == cap(out) {
			full = true
			continue
		}
		out = append(out, id)
	}
	var next int64
	for _, st := range m.stalls {
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
	if buf := s.OutboundBuffer; buf != nil && buf.Len() > 0 {
		m.refundOutbound(buf.Len())
		buf.Reset()
	}
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
