package stream

import (
	"bytes"
	"context"
	"sync"
	"sync/atomic"
	"time"
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
	return &Stream{}
}}

// Stream flag bits packed into flags atomic.Uint32.
const (
	flagCancelled    uint32 = 1 << 0
	flagAsyncRunning uint32 = 1 << 1
	flagDoneClosed   uint32 = 1 << 2
)

// Stream represents an HTTP/2 stream with its associated state and data.
type Stream struct {
	ID                     uint32
	state                  atomic.Int32
	manager                *Manager
	Headers                [][2]string
	Trailers               [][2]string
	Data                   *bytes.Buffer
	OutboundBuffer         *bytes.Buffer
	OutboundEndStream      bool
	headersSent            atomic.Bool
	EndStream              bool
	IsStreaming            bool
	handlerStarted         atomic.Bool
	DeferResponse          bool
	windowSize             atomic.Int32
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
	flags                  atomic.Uint32
	doneCh                 atomic.Pointer[chan struct{}]
	phase                  Phase
	CachedCtx              any // per-connection cached context (avoids pool Get/Put per request)
}

// streamContext is a zero-alloc context.Context for streams.
// It avoids the context.WithCancel allocation on the fast path.
type streamContext struct{ s *Stream }

func (c streamContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c streamContext) Value(_ any) any             { return nil }

func (c streamContext) Done() <-chan struct{} {
	if p := c.s.doneCh.Load(); p != nil {
		return *p
	}
	if c.s.flags.Load()&flagCancelled != 0 {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	ch := make(chan struct{})
	if c.s.doneCh.CompareAndSwap(nil, &ch) {
		if c.s.flags.Load()&flagCancelled != 0 {
			if old := c.s.flags.Or(flagDoneClosed); old&flagDoneClosed == 0 {
				close(ch)
			}
		}
		return ch
	}
	return *c.s.doneCh.Load()
}

func (c streamContext) Err() error {
	if c.s.flags.Load()&flagCancelled != 0 {
		return context.Canceled
	}
	return nil
}

// NewStream creates a new stream with full H2 initialization.
func NewStream(id uint32) *Stream {
	s := streamPool.Get().(*Stream)
	s.ID = id
	s.state.Store(int32(StateIdle))
	s.Data = getBuf()
	s.OutboundBuffer = getBuf()
	s.windowSize.Store(65535)
	s.phase = PhaseInit
	return s
}

// NewH1Stream creates a lightweight stream optimized for H1 requests.
func NewH1Stream(id uint32) *Stream {
	s := streamPool.Get().(*Stream)
	s.ID = id
	s.state.Store(int32(StateIdle))
	s.h1Mode = true
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
	if s.h1Mode {
		return bgCtx
	}
	return streamContext{s}
}

// Cancel cancels the stream's context.
func (s *Stream) Cancel() {
	old := s.flags.Or(flagCancelled)
	if old&flagCancelled != 0 {
		return // already cancelled
	}
	if p := s.doneCh.Load(); p != nil {
		if old2 := s.flags.Or(flagDoneClosed); old2&flagDoneClosed == 0 {
			close(*p)
		}
	}
}

// IsCancelled reports whether the stream has been cancelled.
func (s *Stream) IsCancelled() bool {
	return s.flags.Load()&flagCancelled != 0
}

// Release returns pooled buffers, cancels the context, and returns the stream
// to its pool. Safe to call multiple times; subsequent calls are no-ops.
func (s *Stream) Release() {
	if !s.h1Mode {
		s.Cancel()
	}
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
	s.ID = 0
	s.state.Store(0)
	s.manager = nil
	s.Headers = s.Headers[:0]
	s.Trailers = s.Trailers[:0]
	s.OutboundEndStream = false
	s.headersSent.Store(false)
	s.EndStream = false
	s.IsStreaming = false
	s.handlerStarted.Store(false)
	s.DeferResponse = false
	s.windowSize.Store(0)
	s.ResponseWriter = nil
	s.RemoteAddr = ""
	s.ReceivedDataLen = 0
	s.ReceivedInitialHeaders = false
	s.ClosedByReset = false
	s.IsHEAD = false
	s.h1Mode = false
	s.flags.Store(0)
	s.doneCh.Store(nil)
	s.phase = 0
	s.CachedCtx = nil
	if s.ReceivedWindowUpd != nil {
		for len(s.ReceivedWindowUpd) > 0 {
			<-s.ReceivedWindowUpd
		}
		s.ReceivedWindowUpd = nil
	}
	streamPool.Put(s)
}

// ResetH1Stream performs a lightweight per-request reset for H1 stream reuse.
func ResetH1Stream(s *Stream) {
	if s.Data != nil {
		s.Data.Reset()
		bufferPool.Put(s.Data)
		s.Data = nil
	}
	s.Headers = s.Headers[:0]
	s.headersSent.Store(false)
	s.EndStream = false
	s.ResponseWriter = nil
	s.IsHEAD = false
	s.state.Store(int32(StateIdle))
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

// SetState sets the stream state and atomically updates the manager's active count.
func (s *Stream) SetState(state State) {
	prev := State(s.state.Swap(int32(state)))
	if s.manager != nil {
		s.manager.updateActiveCount(prev, state)
	}
}

// StoreState is a lightweight SetState for H1 streams (no manager, no Swap).
// Avoids the atomic.Swap + updateActiveCount overhead on the H1 hot path.
func (s *Stream) StoreState(state State) {
	s.state.Store(int32(state))
}

// GetState returns the current stream state.
func (s *Stream) GetState() State {
	return State(s.state.Load())
}

// GetWindowSize returns the current flow control window size.
func (s *Stream) GetWindowSize() int32 {
	return s.windowSize.Load()
}

// DeductWindow subtracts n from the flow control window.
func (s *Stream) DeductWindow(n int32) {
	s.windowSize.Add(-n)
}

// SetWindowSize stores a new window size value. Used during initialization.
func (s *Stream) SetWindowSize(v int32) {
	s.windowSize.Store(v)
}

// AddWindowSize atomically adds delta to the window size and returns the new value.
func (s *Stream) AddWindowSize(delta int32) int32 {
	return s.windowSize.Add(delta)
}

// LoadWindowSize is an alias for GetWindowSize, used for clarity in CAS loops.
func (s *Stream) LoadWindowSize() int32 {
	return s.windowSize.Load()
}

// CompareAndSwapWindowSize atomically compares and swaps the window size.
func (s *Stream) CompareAndSwapWindowSize(old, val int32) bool {
	return s.windowSize.CompareAndSwap(old, val)
}

// GetHeadersSent reports whether headers have been sent.
func (s *Stream) GetHeadersSent() bool {
	return s.headersSent.Load()
}

// SetHeadersSent marks headers as sent.
func (s *Stream) SetHeadersSent() {
	s.headersSent.Store(true)
}

// GetHandlerStarted reports whether the handler has started.
func (s *Stream) GetHandlerStarted() bool {
	return s.handlerStarted.Load()
}

// SetHandlerStarted marks the handler as started.
func (s *Stream) SetHandlerStarted() {
	s.handlerStarted.Store(true)
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

// GetOrCreateWindowUpdateChan lazily allocates the ReceivedWindowUpd channel.
// Used by streaming handlers that need to wait for flow control updates.
func (s *Stream) GetOrCreateWindowUpdateChan() chan int32 {
	if s.ReceivedWindowUpd == nil {
		s.ReceivedWindowUpd = make(chan int32, 16)
	}
	return s.ReceivedWindowUpd
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
