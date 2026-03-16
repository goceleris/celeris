package stream

import (
	"bytes"
	"context"
	"sync"
)

var bufferPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

// bgCtx is a cached context.Background() to avoid per-call interface boxing.
var bgCtx = context.Background()

func getBuf() *bytes.Buffer {
	b := bufferPool.Get().(*bytes.Buffer)
	b.Reset()
	return b
}

var streamPool = sync.Pool{New: func() any {
	return &Stream{
		ReceivedWindowUpd: make(chan int32, 16),
	}
}}

// Stream represents an HTTP/2 stream with its associated state and data.
type Stream struct {
	ID                     uint32
	State                  State
	manager                *Manager
	Headers                [][2]string
	Trailers               [][2]string
	Data                   *bytes.Buffer
	OutboundBuffer         *bytes.Buffer
	OutboundEndStream      bool
	HeadersSent            bool
	EndStream              bool
	IsStreaming            bool
	HandlerStarted         bool
	DeferResponse          bool
	WindowSize             int32
	ReceivedWindowUpd      chan int32 // buffered; consumed by engine layer during DATA writes
	mu                     sync.RWMutex
	writeMu                sync.Mutex
	ResponseWriter         ResponseWriter
	RemoteAddr             string
	ReceivedDataLen        int
	ReceivedInitialHeaders bool
	ClosedByReset          bool
	IsHEAD                 bool
	h1Mode                 bool // single-threaded H1 stream; skip mutex in GetHeaders
	ctx                    context.Context
	cancel                 context.CancelFunc
	phase                  Phase
	CachedCtx              any // per-connection cached context (avoids pool Get/Put per request)
}

// NewStream creates a new stream with full H2 initialization (context, buffers).
// Uses context.Background() directly (no WithCancel) to avoid allocation;
// cancel semantics are lazily created via EnsureCancel only when needed.
func NewStream(id uint32) *Stream {
	s := streamPool.Get().(*Stream)
	s.ID = id
	s.State = StateIdle
	s.Data = getBuf()
	s.OutboundBuffer = getBuf()
	s.WindowSize = 65535
	s.ctx = bgCtx
	s.phase = PhaseInit
	return s
}

// NewH1Stream creates a lightweight stream optimized for H1 requests.
// It uses context.Background() directly (no WithCancel), leaves Data and
// OutboundBuffer nil (lazy-allocated only when body data arrives), and skips
// H2-specific window initialization. This eliminates 2+ allocs per H1 request.
func NewH1Stream(id uint32) *Stream {
	s := streamPool.Get().(*Stream)
	s.ID = id
	s.State = StateIdle
	s.h1Mode = true
	s.ctx = bgCtx
	// Pre-allocate header capacity to avoid allocation in requestToStream.
	// After the first Release, capacity is preserved from the previous request.
	if cap(s.Headers) < 16 {
		s.Headers = make([][2]string, 0, 16)
	}
	return s
}

// GetBuf lazily allocates the Data buffer and returns it.
func (s *Stream) GetBuf() *bytes.Buffer {
	if s.Data == nil {
		s.Data = getBuf()
	}
	return s.Data
}

// Context returns the stream's context.
func (s *Stream) Context() context.Context {
	return s.ctx
}

// EnsureCancel lazily creates a cancellable context for this stream.
// Called when the stream needs cancel semantics (e.g., RST_STREAM).
func (s *Stream) EnsureCancel() {
	if s.cancel != nil {
		return
	}
	s.ctx, s.cancel = context.WithCancel(s.ctx)
}

// Cancel cancels the stream's context.
func (s *Stream) Cancel() {
	if s.cancel != nil {
		s.cancel()
	}
}

// Release returns pooled buffers, cancels the context, and returns the stream
// to its pool. Safe to call multiple times; subsequent calls are no-ops.
func (s *Stream) Release() {
	if s.ctx == nil {
		return // already released
	}
	s.Cancel()
	if s.Data != nil {
		s.Data.Reset()
		bufferPool.Put(s.Data)
		s.Data = nil
	}
	if s.OutboundBuffer != nil {
		s.OutboundBuffer.Reset()
		bufferPool.Put(s.OutboundBuffer)
		s.OutboundBuffer = nil
	}
	// Clear all fields to avoid retaining references.
	s.ID = 0
	s.State = 0
	s.manager = nil
	s.Headers = s.Headers[:0]
	s.Trailers = s.Trailers[:0]
	s.OutboundEndStream = false
	s.HeadersSent = false
	s.EndStream = false
	s.IsStreaming = false
	s.HandlerStarted = false
	s.DeferResponse = false
	s.WindowSize = 0
	s.ResponseWriter = nil
	s.RemoteAddr = ""
	s.ReceivedDataLen = 0
	s.ReceivedInitialHeaders = false
	s.ClosedByReset = false
	s.IsHEAD = false
	s.h1Mode = false
	s.ctx = nil
	s.cancel = nil
	s.phase = 0
	s.CachedCtx = nil
	// Drain the channel without blocking. Skip for H1 streams
	// (channel is never written to) to avoid select dispatch overhead.
	if len(s.ReceivedWindowUpd) > 0 {
		for {
			select {
			case <-s.ReceivedWindowUpd:
			default:
				goto drained
			}
		}
	drained:
	}
	streamPool.Put(s)
}

// ResetH1Stream performs a lightweight per-request reset for H1 stream reuse.
// Unlike Release(), it does NOT return the stream to the pool. It clears only
// the fields that change between requests, retaining header slice capacity
// and the context reference. Called between requests on keep-alive connections
// to avoid sync.Pool Get/Put overhead.
func ResetH1Stream(s *Stream) {
	if s.Data != nil {
		s.Data.Reset()
		bufferPool.Put(s.Data)
		s.Data = nil
	}
	s.Headers = s.Headers[:0]
	s.HeadersSent = false
	s.EndStream = false
	s.ResponseWriter = nil
	s.IsHEAD = false
	s.State = StateIdle
	s.ctx = bgCtx
}

// AddHeader adds a header to the stream.
func (s *Stream) AddHeader(name, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Headers = append(s.Headers, [2]string{name, value})
}

// AddHeadersBatch adds multiple headers under a single lock acquisition.
func (s *Stream) AddHeadersBatch(headers [][2]string) {
	s.mu.Lock()
	s.Headers = append(s.Headers, headers...)
	s.mu.Unlock()
}

// AddData adds data to the stream buffer.
func (s *Stream) AddData(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Data == nil {
		s.Data = getBuf()
	}
	_, err := s.Data.Write(data)
	return err
}

// GetData returns the buffered data.
func (s *Stream) GetData() []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.Data == nil {
		return nil
	}
	return s.Data.Bytes()
}

// GetHeaders returns the headers. For single-threaded H1 streams, returns
// the slice directly (no lock, no copy). For H2 streams, returns a safe copy
// under lock.
func (s *Stream) GetHeaders() [][2]string {
	if s.h1Mode {
		return s.Headers
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	headers := make([][2]string, len(s.Headers))
	copy(headers, s.Headers)
	return headers
}

// HeadersLen returns the number of headers on the stream.
func (s *Stream) HeadersLen() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.Headers)
}

// ForEachHeader calls fn for each header under a read lock.
func (s *Stream) ForEachHeader(fn func(name, value string)) {
	s.mu.RLock()
	for _, h := range s.Headers {
		fn(h[0], h[1])
	}
	s.mu.RUnlock()
}

// SetState sets the stream state.
func (s *Stream) SetState(state State) {
	s.mu.Lock()
	prev := s.State
	s.State = state
	s.mu.Unlock()
	if s.manager != nil {
		s.manager.mu.Lock()
		s.manager.markActiveTransition(prev, state)
		s.manager.mu.Unlock()
	}
}

// GetState returns the current stream state.
func (s *Stream) GetState() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.State
}

// GetWindowSize returns the current flow control window size.
func (s *Stream) GetWindowSize() int32 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.WindowSize
}

// DeductWindow subtracts n from the flow control window.
func (s *Stream) DeductWindow(n int32) {
	s.mu.Lock()
	s.WindowSize -= n
	s.mu.Unlock()
}

// BufferOutbound stores data that couldn't be sent due to flow control.
func (s *Stream) BufferOutbound(data []byte, endStream bool) {
	s.mu.Lock()
	if s.OutboundBuffer == nil {
		s.OutboundBuffer = new(bytes.Buffer)
	}
	s.OutboundBuffer.Write(data)
	s.OutboundEndStream = endStream
	s.mu.Unlock()
}

// SetPhase sets the stream phase.
func (s *Stream) SetPhase(p Phase) {
	s.mu.Lock()
	s.phase = p
	s.mu.Unlock()
}

// GetPhase returns the current stream phase.
func (s *Stream) GetPhase() Phase {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.phase
}

// WriteLock acquires the per-stream write lock.
func (s *Stream) WriteLock() { s.writeMu.Lock() }

// WriteUnlock releases the per-stream write lock.
func (s *Stream) WriteUnlock() { s.writeMu.Unlock() }

// MarkBuffered marks the stream as having buffered data.
func (s *Stream) MarkBuffered() {
	if s.manager != nil {
		s.manager.MarkStreamBuffered(s.ID)
	}
}

// MarkEmpty marks the stream as having no buffered data.
func (s *Stream) MarkEmpty() {
	if s.manager != nil {
		s.manager.MarkStreamEmpty(s.ID)
	}
}
