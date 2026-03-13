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
		s.mu.Lock()
		s.WindowSize -= n
		s.mu.Unlock()
	}
}

// ConsumeSendWindowFast decrements connection and stream windows after sending DATA.
// Avoids Manager lock.
func (m *Manager) ConsumeSendWindowFast(s *Stream, n int32) {
	if n <= 0 {
		return
	}
	atomic.AddInt32(&m.connectionWindow, -n)
	s.mu.Lock()
	s.WindowSize -= n
	s.mu.Unlock()
}

// AccumulateWindowUpdate accumulates window credits without sending immediately.
func (m *Manager) AccumulateWindowUpdate(streamID uint32, increment uint32) {
	m.windowUpdateMu.Lock()
	defer m.windowUpdateMu.Unlock()

	if streamID == 0 {
		m.pendingConnWindowUpdate += increment
	} else {
		if m.pendingStreamUpdates[streamID] == nil {
			val := uint32(0)
			m.pendingStreamUpdates[streamID] = &val
		}
		*m.pendingStreamUpdates[streamID] += increment
	}
}

// FlushWindowUpdates sends accumulated WINDOW_UPDATE frames if threshold is met.
// Returns true if updates were sent.
func (m *Manager) FlushWindowUpdates(writer FrameWriter, force bool) bool {
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
		if pending != nil && *pending > 0 && (force || *pending >= windowUpdateThreshold) {
			val := *pending
			*pending = 0
			_ = writer.WriteWindowUpdate(sid, val)
			flushed = true
		}
	}

	return flushed
}
