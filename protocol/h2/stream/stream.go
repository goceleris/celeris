package stream

import (
	"bytes"
	"context"
	"sync"
)

var bufferPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

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
	ReceivedDataLen        int
	ReceivedInitialHeaders bool
	ClosedByReset          bool
	ctx                    context.Context
	cancel                 context.CancelFunc
	phase                  Phase
}

// NewStream creates a new stream.
func NewStream(id uint32) *Stream {
	s := streamPool.Get().(*Stream)
	ctx, cancel := context.WithCancel(context.Background())
	s.ID = id
	s.State = StateIdle
	s.Data = getBuf()
	s.OutboundBuffer = getBuf()
	s.WindowSize = 65535
	s.ctx = ctx
	s.cancel = cancel
	s.phase = PhaseInit
	return s
}

// Context returns the stream's context.
func (s *Stream) Context() context.Context {
	return s.ctx
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
	s.ReceivedDataLen = 0
	s.ReceivedInitialHeaders = false
	s.ClosedByReset = false
	s.ctx = nil
	s.cancel = nil
	s.phase = 0
	// Drain the channel without blocking.
	for {
		select {
		case <-s.ReceivedWindowUpd:
		default:
			goto drained
		}
	}
drained:
	streamPool.Put(s)
}

// AddHeader adds a header to the stream.
func (s *Stream) AddHeader(name, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Headers = append(s.Headers, [2]string{name, value})
}

// AddData adds data to the stream buffer.
func (s *Stream) AddData(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.Data.Write(data)
	return err
}

// GetData returns the buffered data.
func (s *Stream) GetData() []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Data.Bytes()
}

// GetHeaders returns a copy of the headers.
func (s *Stream) GetHeaders() [][2]string {
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
