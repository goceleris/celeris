package stream

import (
	"sync"
	"sync/atomic"
)

// Manager manages multiple HTTP/2 streams.
type Manager struct {
	streams                 map[uint32]*Stream
	nextStreamID            uint32
	lastClientStream        atomic.Uint32
	maxStreamID             uint32
	mu                      sync.RWMutex
	connectionWindow        int32
	maxStreams              uint32
	priorityTree            *PriorityTree
	pushEnabled             bool
	nextPushID              uint32
	maxFrameSize            uint32 // peer's SETTINGS_MAX_FRAME_SIZE (caps what WE send)
	localMaxFrameSize       uint32 // our advertised SETTINGS_MAX_FRAME_SIZE (caps what PEER sends us)
	initialWindowSize       uint32
	activeStreams           atomic.Uint32
	headerTableSize         uint32 // peer's SETTINGS_HEADER_TABLE_SIZE (bound on our HPACK dynamic table)
	pendingConnWindowUpdate uint32
	pendingStreamUpdates    map[uint32]uint32
	windowUpdateMu          sync.Mutex
	hasPendingUpdates       atomic.Bool
	streamsWithData         map[uint32]struct{}
	RemoteAddr              string
}

// NewManager creates a new stream manager. Auxiliary maps
// (pendingStreamUpdates, streamsWithData) and the priority-tree maps are
// lazily allocated on first write — on connections that only serve a
// single default-priority stream (e.g. an h2c upgrade that closes right
// after the response) they stay nil and save three map allocations.
func NewManager() *Manager {
	return &Manager{
		streams:           make(map[uint32]*Stream),
		nextStreamID:      1,
		connectionWindow:  65535,
		maxStreams:        100,
		priorityTree:      NewPriorityTree(),
		pushEnabled:       true,
		nextPushID:        2,
		maxFrameSize:      16384, // peer's — RFC default until their SETTINGS lands
		localMaxFrameSize: 16384, // our advertised — overridden by caller via SetLocalMaxFrameSize
		initialWindowSize: 65535,
		headerTableSize:   4096, // RFC 7540 §6.5.2 default
	}
}

// SetLocalMaxFrameSize records the SETTINGS_MAX_FRAME_SIZE we advertised to
// the peer. Inbound frames are validated against this value (RFC 7540 §4.2).
// Safe to call from any goroutine.
func (m *Manager) SetLocalMaxFrameSize(v uint32) {
	if v < 16384 {
		v = 16384
	}
	atomic.StoreUint32(&m.localMaxFrameSize, v)
}

// GetLocalMaxFrameSize returns our advertised SETTINGS_MAX_FRAME_SIZE.
func (m *Manager) GetLocalMaxFrameSize() uint32 {
	v := atomic.LoadUint32(&m.localMaxFrameSize)
	if v == 0 {
		return 16384
	}
	return v
}

// CreateStream creates a new stream with the given ID.
func (m *Manager) CreateStream(id uint32) *Stream {
	m.mu.Lock()
	defer m.mu.Unlock()

	stream := NewStream(id)
	stream.manager = m
	stream.RemoteAddr = m.RemoteAddr
	//nolint:gosec // G115: safe conversion, initialWindowSize validated by protocol
	stream.SetWindowSize(int32(m.initialWindowSize))
	m.streams[id] = stream
	if id > m.maxStreamID {
		m.maxStreamID = id
	}
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
		st := s.GetState()
		if st == StateOpen || st == StateHalfClosedLocal || st == StateHalfClosedRemote {
			return s, true
		}
	}

	if m.activeStreams.Load() >= m.maxStreams {
		return nil, false
	}

	s := NewStream(id)
	s.manager = m
	s.RemoteAddr = m.RemoteAddr
	//nolint:gosec // G115: safe conversion, initialWindowSize validated by protocol
	s.SetWindowSize(int32(m.initialWindowSize))
	s.state.Store(int32(StateOpen))
	m.streams[id] = s
	if id > m.maxStreamID {
		m.maxStreamID = id
	}
	if id%2 == 1 {
		m.lastClientStream.Store(id)
	}
	m.activeStreams.Add(1)
	return s, true
}

// DeleteStream removes a stream and releases its pooled buffers.
// If the stream has an async handler goroutine running (asyncRunning=true),
// the stream is removed from the map but NOT released — the goroutine
// will release it upon completion via ReleaseAsyncStream.
func (m *Manager) DeleteStream(id uint32) {
	m.mu.Lock()
	s, ok := m.streams[id]
	if ok {
		delete(m.streams, id)
		m.priorityTree.RemoveStream(id)
	}
	m.mu.Unlock()

	if !ok {
		return
	}

	m.windowUpdateMu.Lock()
	delete(m.pendingStreamUpdates, id)
	m.windowUpdateMu.Unlock()

	if s.flags.Load()&flagAsyncRunning == 0 {
		s.Release()
	}
}

// RemoveStreamFromMap removes a stream from the manager's map without releasing it.
// Used by async handler goroutines that manage their own stream lifecycle.
func (m *Manager) RemoveStreamFromMap(id uint32) {
	m.mu.Lock()
	delete(m.streams, id)
	m.priorityTree.RemoveStream(id)
	m.mu.Unlock()

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
	id := m.maxStreamID
	m.mu.RUnlock()
	return id
}

// GetLastClientStreamID returns the highest client-initiated stream ID observed.
func (m *Manager) GetLastClientStreamID() uint32 {
	return m.lastClientStream.Load()
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
	return int(m.activeStreams.Load())
}

// updateActiveCount adjusts activeStreams atomically when a stream transitions
// between active and inactive states. No locks required.
func (m *Manager) updateActiveCount(prev State, next State) {
	wasActive := prev == StateOpen || prev == StateHalfClosedLocal || prev == StateHalfClosedRemote
	isActive := next == StateOpen || next == StateHalfClosedLocal || next == StateHalfClosedRemote
	if wasActive == isActive {
		return
	}
	if isActive {
		m.activeStreams.Add(1)
	} else {
		// Guard against underflow from duplicate transitions.
		for {
			old := m.activeStreams.Load()
			if old == 0 {
				return
			}
			if m.activeStreams.CompareAndSwap(old, old-1) {
				return
			}
		}
	}
}

// SetMaxConcurrentStreams sets the maximum number of concurrent peer-initiated streams allowed.
func (m *Manager) SetMaxConcurrentStreams(n uint32) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.maxStreams = n
}

// ApplySetting applies a single H2 SETTINGS entry. Used by the h2c upgrade
// path to seed the connection with the client's settings from the
// HTTP2-Settings header (RFC 7540 §3.2.1). This is a minimal, conservative
// application; complex side-effects (stream window resync on
// INITIAL_WINDOW_SIZE change) are not needed because no streams exist yet
// when this is called.
//
// The setting identifiers are from RFC 7540 §6.5.2:
//
//	0x1 HEADER_TABLE_SIZE        0x4 INITIAL_WINDOW_SIZE
//	0x2 ENABLE_PUSH              0x5 MAX_FRAME_SIZE
//	0x3 MAX_CONCURRENT_STREAMS   0x6 MAX_HEADER_LIST_SIZE
func (m *Manager) ApplySetting(id uint16, val uint32) {
	switch id {
	case 0x1: // HEADER_TABLE_SIZE
		// Peer's upper bound on our HPACK encoder dynamic table. Stash
		// it so we respect the limit when emitting headers. The inline
		// and stream encoders currently run with MaxDynamicTableSize=0,
		// which trivially satisfies any non-zero limit; if the encoder
		// ever enables the dynamic table, this value MUST bound it.
		m.mu.Lock()
		m.headerTableSize = val
		m.mu.Unlock()
	case 0x2: // ENABLE_PUSH
		m.mu.Lock()
		m.pushEnabled = val == 1
		m.mu.Unlock()
	case 0x3: // MAX_CONCURRENT_STREAMS (client's limit on server push)
		// Client-side setting; no server state to update.
	case 0x4: // INITIAL_WINDOW_SIZE
		if val > 0x7fffffff {
			return
		}
		m.mu.Lock()
		m.initialWindowSize = val
		m.mu.Unlock()
	case 0x5: // MAX_FRAME_SIZE
		if val < 16384 || val > 16777215 {
			return
		}
		atomic.StoreUint32(&m.maxFrameSize, val)
	}
}

// GetHeaderTableSize returns the peer's SETTINGS_HEADER_TABLE_SIZE (the
// maximum HPACK dynamic table size the peer will accept from the server).
// Defaults to 4096 (RFC 7540 §6.5.2) until the peer advertises otherwise.
func (m *Manager) GetHeaderTableSize() uint32 {
	m.mu.RLock()
	v := m.headerTableSize
	m.mu.RUnlock()
	return v
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
	if m.streamsWithData == nil {
		m.streamsWithData = make(map[uint32]struct{})
	}
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

// GetMaxFrameSize returns the current max frame size (atomic).
func (m *Manager) GetMaxFrameSize() uint32 { return atomic.LoadUint32(&m.maxFrameSize) }

// GetSendWindowsAndMaxFrameFast returns current connection window, stream window, and max frame size.
// It avoids Manager lock by using atomics and direct stream access.
func (m *Manager) GetSendWindowsAndMaxFrameFast(s *Stream) (connWindow int32, streamWindow int32, maxFrame uint32) {
	connWindow = atomic.LoadInt32(&m.connectionWindow)
	streamWindow = s.GetWindowSize()
	maxFrame = atomic.LoadUint32(&m.maxFrameSize)
	return
}

// Close releases all streams still held by the manager. Called when the
// H2 connection is closed to prevent stream objects from leaking in the map.
func (m *Manager) Close() {
	m.mu.Lock()
	for id, s := range m.streams {
		delete(m.streams, id)
		// Only release streams that aren't running async handlers.
		if s.flags.Load()&flagAsyncRunning == 0 {
			s.Release()
		}
	}
	m.mu.Unlock()

	m.windowUpdateMu.Lock()
	if m.pendingStreamUpdates != nil {
		clear(m.pendingStreamUpdates)
	}
	m.windowUpdateMu.Unlock()
}
