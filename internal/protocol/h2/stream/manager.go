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

	// outboundHeld counts the bytes the connection's streams hold in their
	// OutboundBuffers, waiting for the peer's window; sendWindowWaiters the
	// pool handlers waiting in AwaitSendWindow for a window to open, and
	// sendWindowCh the channel that wakes them, closed and replaced under
	// sendWindowMu (a leaf lock) by notifySendWindow (celeris#893,
	// outbound.go).
	outboundHeld      atomic.Int64
	sendWindowWaiters atomic.Int32
	sendWindowMu      sync.Mutex
	sendWindowCh      chan struct{}

	// retired holds the pool-handler streams whose handler has returned and
	// which are out of the map, waiting for the event loop to return them to
	// the stream pool (retire, drainRetired; celeris#951). hasRetired is
	// retired's non-emptiness, so a frame batch with nothing retired pays one
	// atomic load. closed (Close) says there is no event loop any more.
	// retired and closed are guarded by mu.
	retired    []*Stream
	hasRetired atomic.Bool
	closed     bool
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
// will release it when its handler returns (executeHandler).
//
// A stream that is still in an active state when it is deleted (e.g. a
// server-initiated RST_STREAM on a stalled stream, or a half-closed-local
// stream whose outbound was fully flushed) must free its
// MAX_CONCURRENT_STREAMS slot here. updateActiveCount is idempotent — if the
// caller already transitioned the stream to Closed (the normal completion
// path) the state is no longer active and no second decrement occurs — so
// this is the single choke point that guarantees every removed stream frees
// its slot exactly once, regardless of which close path reached it.
func (m *Manager) DeleteStream(id uint32) {
	m.mu.Lock()
	s, ok := m.streams[id]
	if !ok {
		m.mu.Unlock()
		return
	}
	release := m.takeLocked(id, s)
	m.mu.Unlock()
	m.afterTake(id, s, release)
}

// resetStream acts on a RST_STREAM from the peer: the stream is closed,
// marked ClosedByReset, cancelled and deleted, as DeleteStream deletes it. It
// reports false, and does nothing, when no stream has the ID.
//
// Everything it does to the stream happens under m.mu, while the stream is
// still in the map. A pool handler's goroutine takes its stream out of the
// map under m.mu before it releases it, so until then the object is still
// this stream. Looked up and then touched outside the lock, it could already
// be another stream's, on any connection: the reset closed, marked and
// cancelled that stream instead (celeris#951).
func (m *Manager) resetStream(id uint32) bool {
	m.mu.Lock()
	s, ok := m.streams[id]
	if !ok {
		m.mu.Unlock()
		return false
	}
	s.ClosedByReset = true
	// Cancel takes no lock: it sets a flag and closes the context's Done
	// channel, if it has one. What waits on the context wakes on its own
	// goroutine.
	s.Cancel()
	release := m.takeLocked(id, s)
	m.mu.Unlock()
	m.afterTake(id, s, release)
	return true
}

// takeLocked takes s, the stream m.streams[id], out of the map and closes
// it. m.mu must be held. It reports whether the caller is to release s
// (afterTake): yes unless a pool handler's goroutine still runs on it
// (flagAsyncRunning), which then releases it itself.
//
// That is decided here, under m.mu, because that is where the goroutine
// hands the stream over: it takes its stream out of the map under m.mu
// before it releases it (executeHandler), and it gives a stream whose
// response is still buffered to the event loop under m.mu too
// (handOffBuffered). So while s is in the map, the flag says who owns it.
// Read after the unlock, the flag could already have been cleared by the
// goroutine's own release (resetAndPool stores 0): the stream was then
// released a second time and put in the stream pool twice, so two later
// streams, on any connections, shared one object (celeris#950). Nothing
// touches s after the unlock unless it is the caller's to release.
func (m *Manager) takeLocked(id uint32, s *Stream) (release bool) {
	delete(m.streams, id)
	m.priorityTree.RemoveStream(id)
	prev := State(s.state.Swap(int32(StateClosed)))
	m.updateActiveCount(prev, StateClosed)
	return s.flags.Load()&flagAsyncRunning == 0
}

// afterTake finishes what takeLocked began, after m.mu is released: it drops
// the stream's pending WINDOW_UPDATE credit and releases the stream if it was
// the caller's to release.
func (m *Manager) afterTake(id uint32, s *Stream, release bool) {
	m.windowUpdateMu.Lock()
	delete(m.pendingStreamUpdates, id)
	m.windowUpdateMu.Unlock()

	if release {
		s.Release()
	}
}

// handOffBuffered gives a pool stream whose handler has returned with part
// of its response still buffered for the peer's window to the event loop,
// which sends the rest on WINDOW_UPDATE and releases the stream (or a
// RST_STREAM, GOAWAY or Close does). The stream stays in the map. It does
// this under m.mu, where those decide whether to release a stream
// (takeLocked, handleGoAway, Close).
//
// It reports false, and hands nothing over, when the stream is no longer in
// the map: a RST_STREAM, a GOAWAY, Close or the WINDOW_UPDATE flush took it
// out while its handler ran, and left its release to the handler's
// goroutine, which must then release it. Handed over anyway, as it was, no
// flush could find it again and nothing released it: its buffered bytes
// stayed charged to the connection's outbound budget for good (celeris#948).
func (m *Manager) handOffBuffered(s *Stream) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.streams[s.ID] != s {
		return false
	}
	s.flags.And(^flagAsyncRunning)
	return true
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

// retire is a pool handler's goroutine giving up its stream when the handler
// has returned and the response is out of the stream's hands: it ends the
// stream's use, takes the stream out of the map and queues it for the event
// loop, which returns it to the stream pool (drainRetired). The goroutine
// must not touch s after the call.
//
// The goroutine does not release the stream itself because the event loop
// may still hold it. The loop looks a stream up in the map and then keeps
// using the *Stream (a WINDOW_UPDATE, a SETTINGS flush, a RST_STREAM it
// sends), and the stream's handler can return at any moment of that. Released
// then, the object was back in the stream pool, and often another stream's on
// another connection, while the loop set its state, cancelled its context and
// credited its window (celeris#951). The rule now: a stream a pool handler
// runs on is returned to the pool by the event loop, at the end of a frame
// batch (FlushInlineCleanup), when it holds no stream; by Close; or, when the
// connection has closed already, here. So every *Stream the loop holds stays
// the same stream until the loop lets go of it.
//
// What must not wait for the loop is done here: the stream's context ends
// (a derived context, and a handler waiting on Done, wake), and the bytes it
// still buffers go back to the connection's outbound budget (celeris#893),
// so the next stream's budget check does not count them.
//
// Lock order: s.mu alone (the budget), then m.mu alone, then windowUpdateMu
// alone. Nothing here nests one in another, so it cannot deadlock with
// OutboundPending (m.mu, then s.mu) or with Close.
func (m *Manager) retire(s *Stream) {
	s.Cancel()
	s.endCtx()
	s.mu.Lock()
	if buf := s.OutboundBuffer; buf != nil {
		m.refundOutbound(buf.Len())
		buf.Reset()
	}
	id := s.ID
	if s.flags.Load()&flagDetached != 0 {
		// A detached Context may read the request after the handler returned
		// (the stream is never pooled, so it can only be this request's):
		// it sees nothing, as it did when this goroutine reset the stream.
		s.resetRequestLocked()
	}
	s.mu.Unlock()

	m.mu.Lock()
	if m.streams[id] == s {
		delete(m.streams, id)
		m.priorityTree.RemoveStream(id)
	}
	closed := m.closed
	if !closed {
		m.retired = append(m.retired, s)
		m.hasRetired.Store(true)
	}
	m.mu.Unlock()

	m.windowUpdateMu.Lock()
	delete(m.pendingStreamUpdates, id)
	m.windowUpdateMu.Unlock()

	if closed {
		s.Release() // no event loop is left to hold it
	}
}

// drainRetired returns the retired streams to the stream pool. Only the
// event loop calls it, where it holds no stream: at the end of a frame batch
// (FlushInlineCleanup). It costs one atomic load when nothing is retired.
func (m *Manager) drainRetired() {
	for m.hasRetired.Load() {
		m.mu.Lock()
		n := len(m.retired)
		if n == 0 {
			m.hasRetired.Store(false)
			m.mu.Unlock()
			return
		}
		s := m.retired[n-1]
		m.retired[n-1] = nil
		m.retired = m.retired[:n-1]
		if n == 1 {
			m.hasRetired.Store(false)
		}
		m.mu.Unlock()
		s.Release()
	}
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

// Close cancels and releases all streams still held by the manager. Called
// when the H2 connection is closed to prevent stream objects from leaking in
// the map, and to tell any handler still running on one that its client is
// gone.
func (m *Manager) Close() {
	m.mu.Lock()
	for id, s := range m.streams {
		delete(m.streams, id)
		// Cancel every stream, async or not. A detached long-lived handler
		// (an SSE stream parked on client.Context().Done()) learns the peer
		// is gone only through its stream context: H2 streams carry none of
		// the OnWS* hooks the H1 path uses for that signal, so without this
		// the handler, its heartbeat and the release goroutine leak for the
		// process lifetime — celeris#494's shape on H2 (celeris#498).
		// Cancelling only signals; releasing stays the async goroutine's job,
		// as in handleGoAway, because it still owns the stream object.
		s.Cancel()
		if s.flags.Load()&flagAsyncRunning == 0 {
			s.Release()
		}
	}
	// The event loop is done with the connection: the streams pool handlers
	// retired since its last frame batch go to the pool now, and a handler
	// that returns from here on releases its stream itself (retire).
	m.closed = true
	retired := m.retired
	m.retired = nil
	m.hasRetired.Store(false)
	m.mu.Unlock()
	for _, s := range retired {
		s.Release()
	}

	m.windowUpdateMu.Lock()
	if m.pendingStreamUpdates != nil {
		clear(m.pendingStreamUpdates)
	}
	m.windowUpdateMu.Unlock()
}
