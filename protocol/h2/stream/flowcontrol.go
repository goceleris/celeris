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
