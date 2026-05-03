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
	ID    uint32
	state atomic.Int32
	// WorkerID is the engine worker ID (0..NumWorkers-1) that owns this
	// stream's connection, or -1 when there is no meaningful worker identity
	// (e.g. the std engine where Go's runtime scheduler picks the goroutine).
	// Engines populate this once per connection at accept-time so handlers
	// can read affinity without a per-request ctx.Value() walk.
	WorkerID    int32
	WorkerIDSet bool
	// LazyRawHeaders, when non-nil on an H1 stream, holds raw request
	// header bytes that have NOT been appended to Headers yet — the H1
	// dispatch sets only the 4 pseudo-headers eagerly and defers the raw
	// header lowercase + slice append until a handler actually reads them
	// via Context.Header / Context.RequestHeaders. Materialized once per
	// request via MaterializeHeaders. Cleared on stream reset.
	LazyRawHeaders     [][2][]byte
	lazyHeadersBuilt   bool
	pseudoMaterialized bool
	// H1 dispatch sets these directly so extractRequestInfo / Host()
	// don't need to walk Headers[0..3]. Empty for H2 streams (where
	// pseudo-headers come from HPACK and live in s.Headers as before).
	Method    string
	Path      string
	Scheme    string
	Authority string
	// CachedRoute holds a (method, path) → (handlers, fullPath) cache
	// scoped to the connection. The router adapter populates it on the
	// first request and reuses it on subsequent requests on the same
	// keep-alive connection that match the same method+path AND produced
	// no path params (i.e. a fully-static route). Skips a static-route
	// map lookup on every request after the first. Per-conn lifetime;
	// reset on stream pool Release.
	CachedRouteMethod   string
	CachedRoutePath     string
	CachedRouteHandlers any // []celeris.HandlerFunc — typed any to avoid cycle
	CachedRouteFullPath string
	// StartTimeNs is the engine's cached time.Now().UnixNano() for the
	// recv that produced this request. populated by populateCachedStream
	// from the engine's worker-local clock cache so HandleStream avoids a
	// per-request time.Now() vDSO call. Zero means "unset, fall back to
	// time.Now()" (synthetic / std-engine path).
	StartTimeNs            int64
	manager                *Manager
	Headers                [][2]string
	Trailers               [][2]string
	Data                   *bytes.Buffer
	rawBody                []byte // zero-copy body slice (preferred over Data when set)
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
	h1Mode                 bool  // single-threaded H1 stream; skip mutex in GetHeaders
	protoMajor             uint8 // 0=infer from h1Mode, 1=HTTP/1.1, 2=HTTP/2
	flags                  atomic.Uint32
	doneCh                 atomic.Pointer[chan struct{}]
	phase                  Phase
	CachedCtx              any                     // per-connection cached context (avoids pool Get/Put per request)
	OnDetach               func()                  // called by Context.Detach to install write-thread safety
	OnWSUpgrade            func(func([]byte))      // called by Context to install WS data delivery callback
	OnWSRawWrite           func() func([]byte)     // returns raw write fn (bypasses chunked encoding)
	OnWSDetachClose        func(func())            // installs callback for engine-side connection close
	OnWSSetError           func(func(error))       // installs error handler for engine-side I/O failures
	OnWSReadPauser         func() (func(), func()) // returns (pause, resume) callbacks for inbound backpressure
	OnWSSetIdleDeadline    func(int64)             // sets the absolute idle deadline (Unix nanoseconds)
	hdrBuf                 [16][2]string
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
	s.windowSize.Store(65535)
	s.phase = PhaseInit
	s.Headers = s.hdrBuf[:0]
	return s
}

// NewH1Stream creates a lightweight stream optimized for H1 requests.
func NewH1Stream(id uint32) *Stream {
	s := streamPool.Get().(*Stream)
	s.ID = id
	s.state.Store(int32(StateIdle))
	s.h1Mode = true
	s.Headers = s.hdrBuf[:0]
	return s
}

// GetBuf lazily allocates the Data buffer and returns it.
func (s *Stream) GetBuf() *bytes.Buffer {
	if s.Data == nil {
		s.Data = getBuf()
	}
	return s.Data
}

// IsH1 returns true if this is an H1 stream (single-threaded, persistent per connection).
func (s *Stream) IsH1() bool { return s.h1Mode }

// ProtoMajor returns the HTTP major version (1 or 2).
func (s *Stream) ProtoMajor() uint8 {
	if s.protoMajor != 0 {
		return s.protoMajor
	}
	if s.h1Mode {
		return 1
	}
	return 2
}

// SetProtoMajor sets the HTTP major version explicitly.
func (s *Stream) SetProtoMajor(v uint8) { s.protoMajor = v }

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

// HasDoneCh reports whether a Done channel was created, indicating
// a derived context (e.g. context.WithTimeout) is watching this stream.
func (s *Stream) HasDoneCh() bool {
	return s.doneCh.Load() != nil
}

// Release returns pooled buffers, cancels the context, and returns the stream
// to its pool. Safe to call multiple times; subsequent calls are no-ops.
func (s *Stream) Release() {
	if !s.h1Mode {
		s.Cancel()
	}
	s.resetAndPool()
}

// ResetForPool returns pooled buffers and returns the stream to its pool
// WITHOUT cancelling the context. Use this in test harnesses where derived
// contexts (e.g. from context.WithTimeout) may have propagation goroutines
// that race with cancellation flag clearing.
func ResetForPool(s *Stream) {
	s.resetAndPool()
}

func (s *Stream) resetAndPool() {
	if s.Data != nil {
		s.Data.Reset()
		bufferPool.Put(s.Data)
		s.Data = nil
	}
	s.rawBody = nil
	if s.OutboundBuffer != nil {
		s.OutboundBuffer.Reset()
		bufferPool.Put(s.OutboundBuffer)
		s.OutboundBuffer = nil
	}
	s.ID = 0
	s.state.Store(0)
	s.manager = nil
	clear(s.hdrBuf[:])
	s.Headers = s.hdrBuf[:0]
	s.Trailers = s.Trailers[:cap(s.Trailers)]
	clear(s.Trailers)
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
	s.protoMajor = 0
	s.flags.Store(0)
	s.doneCh.Store(nil)
	s.phase = 0
	s.CachedCtx = nil
	s.OnDetach = nil
	s.OnWSUpgrade = nil
	s.OnWSRawWrite = nil
	s.OnWSDetachClose = nil
	s.OnWSSetError = nil
	s.OnWSReadPauser = nil
	s.OnWSSetIdleDeadline = nil
	s.LazyRawHeaders = nil
	s.lazyHeadersBuilt = false
	s.pseudoMaterialized = false
	s.Method = ""
	s.Path = ""
	s.Scheme = ""
	s.Authority = ""
	s.CachedRouteMethod = ""
	s.CachedRoutePath = ""
	s.CachedRouteHandlers = nil
	s.CachedRouteFullPath = ""
	s.WorkerID = 0
	s.WorkerIDSet = false
	s.StartTimeNs = 0
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
	s.rawBody = nil
	s.Headers = s.hdrBuf[:0]
	s.LazyRawHeaders = nil
	s.lazyHeadersBuilt = false
	s.pseudoMaterialized = false
	s.Method = ""
	s.Path = ""
	s.Scheme = ""
	s.Authority = ""
	s.headersSent.Store(false)
	s.EndStream = false
	s.ResponseWriter = nil
	s.IsHEAD = false
	s.state.Store(int32(StateIdle))
}

// MaterializeHeaders ensures that every header carried by this H1 stream
// has been appended to s.Headers. The H1 dispatch keeps the 4 pseudo-
// headers (:method/:path/:scheme/:authority) on direct Stream fields
// (Method/Path/Scheme/Authority) and defers BOTH the pseudo-header
// pretty-print and the raw-header lowercase+append until a handler
// actually reads them via Context.Header / Context.RequestHeaders.
// Idempotent.
func (s *Stream) MaterializeHeaders() {
	if s.lazyHeadersBuilt {
		return
	}
	s.lazyHeadersBuilt = true
	needed := len(s.LazyRawHeaders)
	if !s.pseudoMaterialized && s.Method != "" {
		needed += 4
	}
	if cap(s.Headers) < len(s.Headers)+needed {
		grown := make([][2]string, len(s.Headers), len(s.Headers)+needed)
		copy(grown, s.Headers)
		s.Headers = grown
	}
	if !s.pseudoMaterialized && s.Method != "" {
		s.Headers = append(s.Headers,
			[2]string{":method", s.Method},
			[2]string{":path", s.Path},
			[2]string{":scheme", s.Scheme},
			[2]string{":authority", s.Authority},
		)
		s.pseudoMaterialized = true
	}
	for _, rh := range s.LazyRawHeaders {
		s.Headers = append(s.Headers, [2]string{
			lazyLowerHeaderName(rh[0]),
			unsafeStringFromBytes(rh[1]),
		})
	}
}

// lazyLowerHeaderName mirrors h1.UnsafeLowerHeader (interned + lowercased)
// without importing the protocol package here (would create a cycle).
// Implemented as a function-pointer set by the H1 layer at init.
var lazyLowerHeaderName func(b []byte) string

// unsafeStringFromBytes is wired by the H1 layer at init for the same
// reason — the stream package does not import protocol/h1.
var unsafeStringFromBytes func(b []byte) string

// SetLazyHeaderHelpers wires the H1-package helpers used by
// MaterializeHeaders. Called from internal/conn package init.
func SetLazyHeaderHelpers(lower func([]byte) string, str func([]byte) string) {
	lazyLowerHeaderName = lower
	unsafeStringFromBytes = str
}

// ResetH2StreamInline performs a lightweight reset for inline H2 stream reuse.
// Similar to ResetH1Stream but for H2 inline handler path. Resets per-request
// fields while preserving the stream's pool buffers.
func ResetH2StreamInline(s *Stream, id uint32) {
	if s.Data != nil {
		s.Data.Reset()
	} else {
		s.Data = getBuf()
	}
	s.rawBody = nil
	if s.OutboundBuffer != nil {
		s.OutboundBuffer.Reset()
	} else {
		s.OutboundBuffer = getBuf()
	}
	s.ID = id
	s.Headers = s.hdrBuf[:0]
	s.Trailers = s.Trailers[:0]
	s.OutboundEndStream = false
	s.headersSent.Store(false)
	s.EndStream = false
	s.IsStreaming = false
	s.handlerStarted.Store(false)
	s.DeferResponse = false
	s.windowSize.Store(65535)
	s.ResponseWriter = nil
	s.RemoteAddr = ""
	s.ReceivedDataLen = 0
	s.ReceivedInitialHeaders = false
	s.ClosedByReset = false
	s.IsHEAD = false
	s.h1Mode = false
	s.protoMajor = 0
	s.flags.Store(0)
	s.doneCh.Store(nil)
	s.phase = PhaseInit
	s.CachedCtx = nil
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
//
// For H1 streams a raw body slice may be installed via SetRawBody — that
// slice is returned directly, avoiding the per-request memcpy that
// Data.Write would incur. H2 and the buffered fallback path still go
// through s.Data.
func (s *Stream) GetData() []byte {
	if s.h1Mode {
		// Single-threaded H1: no lock, no atomics on the read path.
		if s.rawBody != nil {
			return s.rawBody
		}
		if s.Data == nil {
			return nil
		}
		return s.Data.Bytes()
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.rawBody != nil {
		return s.rawBody
	}
	if s.Data == nil {
		return nil
	}
	return s.Data.Bytes()
}

// SetRawBody installs a zero-copy body slice on the stream. The caller
// must guarantee the backing array stays valid for the handler's
// lifetime. Intended for H1 and std-bridge paths where the body is
// already materialised as a contiguous slice (into the read buffer or a
// freshly allocated heap slice). Passing nil clears the override.
func (s *Stream) SetRawBody(b []byte) {
	s.rawBody = b
}

// GetHeaders returns the headers. For single-threaded H1 streams, returns
// the slice directly (no lock, no copy). For H2 streams, returns a safe copy
// under lock. Materializes any deferred raw H1 headers first.
func (s *Stream) GetHeaders() [][2]string {
	if s.h1Mode {
		s.MaterializeHeaders()
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
	if s.h1Mode {
		s.MaterializeHeaders()
		return len(s.Headers)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.Headers)
}

// ForEachHeader calls fn for each header under a read lock.
func (s *Stream) ForEachHeader(fn func(name, value string)) {
	if s.h1Mode {
		s.MaterializeHeaders()
		for _, h := range s.Headers {
			fn(h[0], h[1])
		}
		return
	}
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
		s.OutboundBuffer = getBuf()
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
