package stream

import (
	"sync/atomic"
)

const windowUpdateThreshold = 8192

// ConsumeSendWindow decrements connection and stream windows after sending DATA.
func (m *Manager) ConsumeSendWindow(streamID uint32, n int32) {
	if n <= 0 {
		return
	}
	atomic.AddInt32(&m.connectionWindow, -n)
	if s, ok := m.GetStream(streamID); ok {
		s.windowSize.Add(-n)
	}
}

// ConsumeSendWindowFast decrements connection and stream windows after sending DATA.
// Avoids Manager lock.
func (m *Manager) ConsumeSendWindowFast(s *Stream, n int32) {
	if n <= 0 {
		return
	}
	atomic.AddInt32(&m.connectionWindow, -n)
	s.windowSize.Add(-n)
}

// ReserveSendWindow atomically reserves up to `want` bytes of send credit for
// stream s, debiting BOTH the per-stream and the connection-level send windows
// by the granted amount in one operation, and returns how many bytes were
// granted — min(want, stream window, connection window), or 0 when either
// window is exhausted. The caller MUST send exactly the returned number of
// bytes and buffer the rest.
//
// Unlike a load-then-debit (which clamps against a stale window value and can
// be raced past the peer-authorized credit), the reservation is a pair of
// compare-and-swap loops, so the event loop (flushStreamOutbound) and a worker
// goroutine (async WriteResponse) can never over-subscribe a shared window:
// each reserves exactly what it will send BEFORE writing, so the connection
// window is never driven past the credit the peer granted (RFC 7540 §6.9).
func (m *Manager) ReserveSendWindow(s *Stream, want int32) int32 {
	if want <= 0 {
		return 0
	}
	// Reserve from the per-stream window first.
	var streamGrant int32
	for {
		sw := s.windowSize.Load()
		if sw <= 0 {
			return 0
		}
		streamGrant = want
		if streamGrant > sw {
			streamGrant = sw
		}
		if s.windowSize.CompareAndSwap(sw, sw-streamGrant) {
			break
		}
	}
	// Reserve the same amount from the connection window; if it grants less,
	// refund the per-stream overage so the two windows stay consistent.
	var connGrant int32
	for {
		cw := atomic.LoadInt32(&m.connectionWindow)
		if cw <= 0 {
			connGrant = 0
			break
		}
		connGrant = streamGrant
		if connGrant > cw {
			connGrant = cw
		}
		if atomic.CompareAndSwapInt32(&m.connectionWindow, cw, cw-connGrant) {
			break
		}
	}
	if connGrant < streamGrant {
		s.windowSize.Add(streamGrant - connGrant)
	}
	return connGrant
}

// AccumulateWindowUpdate accumulates window credits without sending immediately.
func (m *Manager) AccumulateWindowUpdate(streamID uint32, increment uint32) {
	m.windowUpdateMu.Lock()
	defer m.windowUpdateMu.Unlock()

	if streamID == 0 {
		m.pendingConnWindowUpdate += increment
	} else {
		if m.pendingStreamUpdates == nil {
			m.pendingStreamUpdates = make(map[uint32]uint32)
		}
		m.pendingStreamUpdates[streamID] += increment
	}
	m.hasPendingUpdates.Store(true)
}

// FlushWindowUpdates sends accumulated WINDOW_UPDATE frames if threshold is met.
// Returns true if updates were sent.
func (m *Manager) FlushWindowUpdates(writer FrameWriter, force bool) bool {
	if !m.hasPendingUpdates.Load() {
		return false
	}

	m.windowUpdateMu.Lock()
	defer m.windowUpdateMu.Unlock()

	flushed := false

	connUpdate := m.pendingConnWindowUpdate
	if connUpdate > 0 && (force || connUpdate >= windowUpdateThreshold) {
		m.pendingConnWindowUpdate = 0
		_ = writer.WriteWindowUpdate(0, connUpdate)
		flushed = true
	}

	for sid, pending := range m.pendingStreamUpdates {
		if pending > 0 && (force || pending >= windowUpdateThreshold) {
			delete(m.pendingStreamUpdates, sid)
			_ = writer.WriteWindowUpdate(sid, pending)
			flushed = true
		}
	}

	if m.pendingConnWindowUpdate == 0 && len(m.pendingStreamUpdates) == 0 {
		m.hasPendingUpdates.Store(false)
	}

	return flushed
}
