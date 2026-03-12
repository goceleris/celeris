package stream

import (
	"sync"
	"sync/atomic"
)

// Manager manages multiple HTTP/2 streams.
type Manager struct {
	streams                 map[uint32]*Stream
	nextStreamID            uint32
	lastClientStream        uint32
	mu                      sync.RWMutex
	connectionWindow        int32
	maxStreams              uint32
	priorityTree            *PriorityTree
	pushEnabled             bool
	nextPushID              uint32
	maxFrameSize            uint32
	initialWindowSize       uint32
	activeStreams           uint32
	pendingConnWindowUpdate uint32
	pendingStreamUpdates    map[uint32]*uint32
	windowUpdateMu          sync.Mutex
	streamsWithData         map[uint32]struct{}
	RemoteAddr              string
}

// NewManager creates a new stream manager.
func NewManager() *Manager {
	return &Manager{
		streams:                 make(map[uint32]*Stream),
		nextStreamID:            1,
		connectionWindow:        65535,
		maxStreams:              100,
		priorityTree:            NewPriorityTree(),
		pushEnabled:             true,
		nextPushID:              2,
		maxFrameSize:            16384,
		initialWindowSize:       65535,
		activeStreams:           0,
		pendingConnWindowUpdate: 0,
		pendingStreamUpdates:    make(map[uint32]*uint32),
		streamsWithData:         make(map[uint32]struct{}),
	}
}

// CreateStream creates a new stream with the given ID.
func (m *Manager) CreateStream(id uint32) *Stream {
	m.mu.Lock()
	defer m.mu.Unlock()

	stream := NewStream(id)
	stream.manager = m
	stream.RemoteAddr = m.RemoteAddr
	//nolint:gosec // G115: safe conversion, initialWindowSize validated by protocol
	stream.WindowSize = int32(m.initialWindowSize)
	m.streams[id] = stream
	return stream
}

// GetStream gets a stream by ID.
func (m *Manager) GetStream(id uint32) (*Stream, bool) {
	m.mu.RLock()
	s, ok := m.streams[id]
	m.mu.RUnlock()
	return s, ok
}

// TryOpenStream attempts to atomically open a new stream and mark it active.
// Returns the opened stream and true on success; returns false if the
// MAX_CONCURRENT_STREAMS limit would be exceeded.
func (m *Manager) TryOpenStream(id uint32) (*Stream, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if s, exists := m.streams[id]; exists {
		if s.State == StateOpen || s.State == StateHalfClosedLocal || s.State == StateHalfClosedRemote {
			return s, true
		}
	}

	if m.activeStreams >= m.maxStreams {
		return nil, false
	}

	s := NewStream(id)
	s.manager = m
	s.RemoteAddr = m.RemoteAddr
	//nolint:gosec // G115: safe conversion, initialWindowSize validated by protocol
	s.WindowSize = int32(m.initialWindowSize)
	s.State = StateOpen
	m.streams[id] = s
	m.activeStreams++
	return s, true
}

// DeleteStream removes a stream and releases its pooled buffers.
func (m *Manager) DeleteStream(id uint32) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if s, ok := m.streams[id]; ok {
		s.Release()
	}
	delete(m.streams, id)
	m.priorityTree.RemoveStream(id)

	m.windowUpdateMu.Lock()
	delete(m.pendingStreamUpdates, id)
	m.windowUpdateMu.Unlock()
}

// StreamCount returns the number of streams in the manager.
func (m *Manager) StreamCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.streams)
}

// GetLastStreamID returns the highest stream ID.
func (m *Manager) GetLastStreamID() uint32 {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var lastID uint32
	for id := range m.streams {
		if id > lastID {
			lastID = id
		}
	}
	return lastID
}

// GetLastClientStreamID returns the highest client-initiated stream ID observed.
func (m *Manager) GetLastClientStreamID() uint32 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.lastClientStream
}

// UpdateConnectionWindow atomically updates the connection-level flow control window.
func (m *Manager) UpdateConnectionWindow(delta int32) {
	atomic.AddInt32(&m.connectionWindow, delta)
}

// GetConnectionWindow atomically returns the current connection window size.
func (m *Manager) GetConnectionWindow() int32 {
	return atomic.LoadInt32(&m.connectionWindow)
}

// CountActiveStreams returns number of streams considered active for concurrency limits.
func (m *Manager) CountActiveStreams() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return int(m.activeStreams)
}

// markActiveTransition adjusts activeStreams when a stream transitions between active and inactive states.
// Caller must hold m.mu.
func (m *Manager) markActiveTransition(prev State, next State) {
	wasActive := prev == StateOpen || prev == StateHalfClosedLocal || prev == StateHalfClosedRemote
	isActive := next == StateOpen || next == StateHalfClosedLocal || next == StateHalfClosedRemote
	if wasActive == isActive {
		return
	}
	if isActive {
		m.activeStreams++
	} else if m.activeStreams > 0 {
		m.activeStreams--
	}
}

// SetMaxConcurrentStreams sets the maximum number of concurrent peer-initiated streams allowed.
func (m *Manager) SetMaxConcurrentStreams(n uint32) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.maxStreams = n
}

// GetMaxConcurrentStreams returns the currently configured max concurrent streams value.
func (m *Manager) GetMaxConcurrentStreams() uint32 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.maxStreams
}

// GetOrCreateStream gets an existing stream or creates a new one.
func (m *Manager) GetOrCreateStream(id uint32) *Stream {
	if stream, ok := m.GetStream(id); ok {
		return stream
	}
	return m.CreateStream(id)
}

// MarkStreamBuffered adds a stream to the set of streams with buffered data.
func (m *Manager) MarkStreamBuffered(id uint32) {
	m.mu.Lock()
	m.streamsWithData[id] = struct{}{}
	m.mu.Unlock()
}

// MarkStreamEmpty removes a stream from the set of streams with buffered data.
func (m *Manager) MarkStreamEmpty(id uint32) {
	m.mu.Lock()
	delete(m.streamsWithData, id)
	m.mu.Unlock()
}

// GetSendWindowsAndMaxFrame returns current connection window, stream window, and max frame size.
func (m *Manager) GetSendWindowsAndMaxFrame(streamID uint32) (connWindow int32, streamWindow int32, maxFrame uint32) {
	connWindow = atomic.LoadInt32(&m.connectionWindow)
	if s, ok := m.GetStream(streamID); ok {
		streamWindow = s.GetWindowSize()
	} else {
		//nolint:gosec // G115: safe conversion
		streamWindow = int32(m.initialWindowSize)
	}
	maxFrame = atomic.LoadUint32(&m.maxFrameSize)
	return
}

// GetSendWindowsAndMaxFrameFast returns current connection window, stream window, and max frame size.
// It avoids Manager lock by using atomics and direct stream access.
func (m *Manager) GetSendWindowsAndMaxFrameFast(s *Stream) (connWindow int32, streamWindow int32, maxFrame uint32) {
	connWindow = atomic.LoadInt32(&m.connectionWindow)
	streamWindow = s.GetWindowSize()
	maxFrame = atomic.LoadUint32(&m.maxFrameSize)
	return
}
