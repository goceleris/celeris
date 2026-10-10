package stream

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// ContinuationState tracks the state of CONTINUATION frames.
type ContinuationState struct {
	streamID      uint32
	headerBlock   []byte
	endStream     bool
	expectingMore bool
	isTrailers    bool
}

var headerBlockPool = sync.Pool{New: func() any { b := make([]byte, 0, 4096); return &b }}

var headersSlicePoolIn = sync.Pool{New: func() any { s := make([][2]string, 0, 16); return &s }}

// h2WorkerPool is a global goroutine pool for executing H2 stream handlers.
// A single pool is shared across all connections to avoid per-connection
// goroutine overhead (8 goroutines × N connections was catastrophic).
//
// A task is only ever QUEUED when a worker is parked waiting for one. That
// rule is the whole design: an H2 handler is not guaranteed to return. A
// Server-Sent Events stream, a long poll or any handler that streams for the
// life of its client holds its worker until the peer goes away, so a pool
// that queues into a buffered channel whenever the channel has room wedges as
// soon as `size` such handlers are running -- every later stream sits in the
// buffer behind workers that will never come back, on EVERY connection in the
// process, because the pool is global (celeris#520). Work that finds no
// parked worker runs on its own goroutine instead, which is what net/http
// does for every stream unconditionally.
type h2WorkerPool struct {
	work chan h2Task
	// idle is (workers parked in the receive) - (tasks sitting in work).
	// A worker credits it before blocking on the channel; Submit claims a
	// credit before it is allowed to queue. It is therefore positive only
	// when a parked worker will pick the task up promptly, and drops to
	// zero the moment the queue catches up with the parked workers -- see
	// the invariant proof in TestH2Pool_IdleCreditTracksParkedWorkers.
	idle atomic.Int32
}

type h2Task struct {
	proc   *Processor
	stream *Stream
}

// globalH2Pool is the shared worker pool for all H2 connections.
// Initialized lazily on first use, never closed (process-lifetime).
var globalH2Pool = newH2WorkerPool(runtime.GOMAXPROCS(0) * 4)

func newH2WorkerPool(size int) *h2WorkerPool {
	p := &h2WorkerPool{work: make(chan h2Task, size*16)}
	for range size {
		go p.run()
	}
	return p
}

// run is one pool worker.
func (p *h2WorkerPool) run() {
	for {
		// Credit the pool before parking. Submit spends exactly one
		// credit per queued task, so the counter stays equal to
		// parked-workers minus queued-tasks no matter which worker
		// ends up taking which task.
		p.idle.Add(1)
		task, ok := <-p.work
		if !ok {
			p.idle.Add(-1)
			return
		}
		task.proc.executeHandler(task.stream)
	}
}

// Submit dispatches a stream handler. It queues onto the shared pool while a
// worker is parked waiting for work, and otherwise runs the handler on its
// own goroutine.
//
// It never blocks the event loop: blocking there would stall frame
// processing for every stream on the connection.
//
// Growing the pool instead of spawning a one-shot goroutine was measured and
// rejected: a surplus worker that joins the pool and retires on an idle TTL
// made the dispatch benchmark SLOWER (865 ns/op vs 793) while adding a
// window in which a traffic spike is held as parked goroutines. The spawn is
// what net/http pays for every stream, unconditionally.
func (p *h2WorkerPool) Submit(proc *Processor, s *Stream) {
	for {
		n := p.idle.Load()
		if n <= 0 {
			// No parked worker. Queueing here is what used to wedge the
			// pool (celeris#520): a streaming handler holds its worker
			// for the life of the stream, so a queued task would wait
			// behind handlers that never return.
			go proc.executeHandler(s)
			return
		}
		if p.idle.CompareAndSwap(n, n-1) {
			break
		}
	}
	select {
	case p.work <- h2Task{proc, s}:
	default:
		// Unreachable while the buffer is size*16 and we only queue
		// against a parked worker, but a dropped task would hang a
		// stream forever, so hand it a goroutine rather than trust that.
		p.idle.Add(1)
		go proc.executeHandler(s)
	}
}

// h2Conn combines ResponseWriter and H2Controller for processor-internal use.
// The H2 connection adapter implements both interfaces on a single type.
type h2Conn interface {
	ResponseWriter
	H2Controller
}

// Processor processes incoming HTTP/2 frames and manages streams.
type Processor struct {
	manager              *Manager
	handler              Handler
	writer               FrameWriter
	currentConn          ResponseWriter
	connWriter           h2Conn
	hpackDecoder         *hpack.Decoder
	hpackTarget          *[][2]string // current HPACK decode target slice (avoids closure alloc per request)
	joinCookies          bool         // the block being decoded is a request's: hpackEmit joins its cookie fields (celeris#944)
	cookieAt             int          // index in *hpackTarget of the block's first cookie field, or -1
	cookieJoined         bool         // cookieBuf holds the joined value of 2 or more cookie fields
	cookieBuf            []byte       // the joined cookie value, reused
	continuationState    *ContinuationState
	continuationStateMu  sync.Mutex
	continuationActive   atomic.Bool
	InlineCachedCtx      any             // per-connection cached app context for inline handlers (avoids sync.Pool)
	hasMoreFrames        bool            // true when more frames follow in current recv (defers inline cleanup)
	pendingInlineCleanup []inlineCleanup // inline-completed streams deferred until frame loop exits
	InlineWriter         h2Conn          // direct-to-outBuf writer for inline handlers (set by conn layer)
	InlineCount          uint64          // number of requests handled inline (for metrics)
	MaxRequestBodySize   int64           // 0 = use default (100 MB)

	// asyncResolver, when set, lets per-stream dispatch honor the
	// per-route .Async() flag: a stream whose route is async is forced
	// onto the worker pool instead of running inline on the event loop.
	// perRouteAsync caches asyncResolver.HasAsyncRoutes() so a pure-sync
	// server skips per-stream route resolution entirely (zero added cost
	// on the inline hot path).
	asyncResolver AsyncRouteResolver
	perRouteAsync bool

	// CVE-2023-44487 "HTTP/2 Rapid Reset" mitigation. Track RST_STREAM
	// rate per connection; if a peer exceeds the threshold we emit
	// GOAWAY(ENHANCE_YOUR_CALM). Window is a sliding per-second bucket:
	// simple, cheap, and enough to shut down a flood loop.
	rstCount     uint32 // RST_STREAM count within the current second
	rstWindowSec int64  // unix-second marker for the current window

	// connFlushScratch is reused by flushConnWindowStalledStreams to snapshot
	// candidate streams without allocating on every WINDOW_UPDATE(0). Frame
	// processing is single-threaded per connection (event loop under
	// H2State.mu), so a per-processor scratch slice is safe.
	connFlushScratch []*Stream

	// outboundSink, when set (SetOutboundSink), carries the DATA that
	// flushStreamOutbound and the SETTINGS_INITIAL_WINDOW_SIZE re-flush send.
	outboundSink OutboundSend

	// poolRunning counts this connection's streams whose handler has been
	// handed to the shared worker pool and has not returned yet
	// (celeris#759). The native engines' graceful shutdown waits for it to
	// reach zero before it closes the connection: those handlers run off the
	// event loop, and the drain that waits for the loop's own work did not
	// see them. Incremented in runHandler before Submit, decremented after
	// executeHandler has returned.
	poolRunning atomic.Int32

	// goAwaySent records that a GOAWAY has gone out, and goAwayLastID the
	// last stream it named (celeris#759). A stream the client opens above
	// it is not served (runHandler): its client counts it as not processed
	// and may retry it on another connection (RFC 9113 §6.8). Written by
	// SendGoAway, read by runHandler, both on the frame-processing path
	// (event loop under H2State.mu).
	goAwaySent   bool
	goAwayLastID uint32
}

// PoolHandlersRunning reports whether a stream of this connection has a
// handler running on the shared worker pool (celeris#759). Safe from any
// goroutine.
func (p *Processor) PoolHandlersRunning() bool {
	return p.poolRunning.Load() > 0
}

// OutboundPending reports whether a stream of this connection has response
// DATA buffered, waiting for the client's WINDOW_UPDATE: its handler has
// returned, and the rest of its response goes out only as the client grants
// window (celeris#759). The native engines' graceful shutdown waits for it,
// as for the pool handlers. Not for the hot path.
//
// Lock order: the manager's mu (read), then each stream's mu (read), so that
// no stream is released (RemoveStreamFromMap and DeleteStream take the
// manager's mu for writing) while it is read. Nothing takes a stream's mu and
// then the manager's: flushStreamOutbound reserves windows under the stream's
// mu with atomics only, and flushConnWindowStalledStreams releases the
// manager's mu before it takes a stream's.
func (p *Processor) OutboundPending() bool {
	p.manager.mu.RLock()
	defer p.manager.mu.RUnlock()
	for _, s := range p.manager.streams {
		s.mu.RLock()
		pending := s.OutboundBuffer != nil && s.OutboundBuffer.Len() > 0
		s.mu.RUnlock()
		if pending {
			return true
		}
	}
	return false
}

// rstRateLimit and rstBurstLimit bound RST_STREAM arrivals. An honest
// client resets at most a handful of streams per second (client abort
// of a pipelined request, EARLY_HINTS race, etc.). 100 resets per
// second is well above any legitimate pattern we've observed and well
// below the thousands-per-second required to amplify the CVE attack.
const (
	rstRateLimitPerSec = 100
	rstBurstMax        = 200
)

func (p *Processor) maxBodySize() int64 {
	return p.MaxRequestBodySize // 0 = unlimited (limit > 0 guard at call sites)
}

// SetHasMoreFrames tells the processor whether more frames follow the
// current one in the recv buffer. Used to suppress inline handler execution
// when the frame loop has more frames to process.
func (p *Processor) SetHasMoreFrames(v bool) { p.hasMoreFrames = v }

// inlineCleanup is a stream whose cleanup executeHandlerInline deferred to
// FlushInlineCleanup, with the ID it had then.
type inlineCleanup struct {
	s  *Stream
	id uint32
}

// FlushInlineCleanup transitions and removes streams that completed inline
// during the frame loop but had cleanup deferred (pending outbound data or
// more frames to process), and then returns the streams the pool handlers
// retired to the stream pool (Manager.retire). Called after the frame loop,
// under H2State.mu: the one place the loop holds no *Stream.
func (p *Processor) FlushInlineCleanup() {
	for _, e := range p.pendingInlineCleanup {
		// The stream may have been released since, by a RST_STREAM later in
		// the read or by handleWindowUpdate/handleSettings. Its object is
		// then back in the pool, and a HEADERS later in the read can have
		// it again under a new ID: look it up by the ID it had, never by
		// the object's current one, which would release that new stream
		// while its handler runs (celeris#947).
		s := e.s
		if existing, ok := p.manager.GetStream(e.id); !ok || existing != s {
			continue
		}
		state := s.GetState()
		if state == StateOpen || state == StateHalfClosedRemote {
			if state == StateHalfClosedRemote {
				s.SetState(StateClosed)
			} else {
				s.SetState(StateHalfClosedLocal)
			}
		}
		p.manager.RemoveStreamFromMap(e.id)
		s.Release()
	}
	p.pendingInlineCleanup = p.pendingInlineCleanup[:0]
	// The frame batch is over and the loop holds no stream: the pool
	// handlers' streams that returned meanwhile go to the stream pool now
	// (celeris#951).
	p.manager.drainRetired()
}

// NewProcessor creates a new stream processor. The conn parameter must
// implement both ResponseWriter and H2Controller (all H2 engine adapters do).
// The HPACK decoder is lazily initialized on the first header-block write —
// on RFC 7540 §3.2 h2c upgrades where stream 1 is injected locally (so no
// HEADERS frame is ever decoded from the wire) and the connection closes
// before any subsequent request, this avoids a ~4 KB dynamic-table
// allocation the decoder would otherwise hold for the connection's life.
func NewProcessor(handler Handler, writer FrameWriter, conn h2Conn) *Processor {
	p := &Processor{
		manager:    NewManager(),
		handler:    handler,
		writer:     writer,
		connWriter: conn,
		cookieAt:   -1,
	}
	if r, ok := handler.(AsyncRouteResolver); ok {
		p.asyncResolver = r
		p.perRouteAsync = r.HasAsyncRoutes()
	}
	return p
}

// ensureHPACKDecoder initializes p.hpackDecoder on first use. Called from
// every site that writes into the decoder (all are serialized under
// H2State.mu, so no atomic dance is needed).
func (p *Processor) ensureHPACKDecoder() {
	if p.hpackDecoder == nil {
		// Set emit function ONCE per connection. The hpackTarget field is
		// updated before each decode to point at the current pooled slice,
		// eliminating a closure allocation per HEADERS/CONTINUATION frame.
		p.hpackDecoder = hpack.NewDecoder(4096, p.hpackEmit)
		// SETTINGS_MAX_HEADER_LIST_SIZE enforcement: cap the total
		// uncompressed header list size the decoder is willing to
		// accept. Without this a single HEADERS frame can grow the
		// decode target unboundedly (DoS). 64 KiB matches the H1
		// MaxHeaderSize default and leaves wide margin for real
		// requests.
		p.hpackDecoder.SetMaxStringLength(64 << 10)
	}
}

// hpackEmit is the persistent HPACK emit callback. It appends decoded headers
// to the slice pointed to by p.hpackTarget (set before each decode call).
//
// In a request's header block the cookie fields are joined into one with
// "; " (RFC 9113 §8.2.3: a client may split the Cookie header, "to allow for
// better compression efficiency", and the server must join the fields before
// it hands them to anything that reads a single HTTP/1.1-style field), as
// net/http's HTTP/2 server does with strings.Join(cookies, "; "). The first
// field keeps its place in the list and its value is replaced when the block
// ends (endHeaderDecode). The pieces are gathered in one buffer, so the work
// is linear in the block, however many fields it splits the header into: a
// field as small as one HPACK byte (the static table's cookie entry) must not
// make joining quadratic (celeris#944).
func (p *Processor) hpackEmit(hf hpack.HeaderField) {
	t := p.hpackTarget
	if p.joinCookies && hf.Name == "cookie" {
		if p.cookieAt < 0 {
			p.cookieAt = len(*t)
		} else {
			if !p.cookieJoined {
				p.cookieBuf = append(p.cookieBuf[:0], (*t)[p.cookieAt][1]...)
				p.cookieJoined = true
			}
			p.cookieBuf = append(p.cookieBuf, "; "...)
			p.cookieBuf = append(p.cookieBuf, hf.Value...)
			return
		}
	}
	*t = append(*t, [2]string{internH2HeaderName(hf.Name), hf.Value})
}

// beginHeaderDecode points hpackEmit at target for the header block about to
// be decoded. requestHeaders says it is a request's header block, whose cookie
// fields hpackEmit joins; a trailer block is left as the peer sent it.
func (p *Processor) beginHeaderDecode(target *[][2]string, requestHeaders bool) {
	p.hpackTarget = target
	p.joinCookies = requestHeaders
	p.cookieAt = -1
	p.cookieJoined = false
}

// endHeaderDecode finishes a header block hpackEmit decoded: if it carried
// more than one cookie field, the first one's value becomes the joined value.
func (p *Processor) endHeaderDecode() {
	if p.cookieJoined {
		(*p.hpackTarget)[p.cookieAt][1] = string(p.cookieBuf)
		p.cookieJoined = false
		if cap(p.cookieBuf) > 4<<10 {
			p.cookieBuf = nil // do not pin a large buffer for the connection's life
		}
	}
	p.cookieAt = -1
}

// GetManager returns the stream manager.
func (p *Processor) GetManager() *Manager {
	return p.manager
}

// GetCurrentConn returns the current connection.
func (p *Processor) GetCurrentConn() ResponseWriter {
	if p.currentConn != nil {
		return p.currentConn
	}
	return p.connWriter
}

// GetConnection returns the permanent connection writer.
func (p *Processor) GetConnection() ResponseWriter {
	return p.connWriter
}

// IsExpectingContinuation reports whether the processor is in the middle of
// receiving a header block.
func (p *Processor) IsExpectingContinuation() bool {
	return p.continuationActive.Load()
}

// GetExpectedContinuationStreamID returns the stream ID we're expecting CONTINUATION frames on.
func (p *Processor) GetExpectedContinuationStreamID() (uint32, bool) {
	if !p.continuationActive.Load() {
		return 0, false
	}
	p.continuationStateMu.Lock()
	defer p.continuationStateMu.Unlock()
	if p.continuationState != nil && p.continuationState.expectingMore {
		return p.continuationState.streamID, true
	}
	return 0, false
}

// ProcessFrame processes an incoming HTTP/2 frame.
//
//nolint:gocyclo // complex frame dispatch logic
func (p *Processor) ProcessFrame(ctx context.Context, frame http2.Frame) error {
	inContinuation := p.continuationActive.Load()
	expectingStreamID := uint32(0)
	if inContinuation {
		p.continuationStateMu.Lock()
		if p.continuationState != nil && p.continuationState.expectingMore {
			expectingStreamID = p.continuationState.streamID
		} else {
			inContinuation = false
		}
		p.continuationStateMu.Unlock()
	}

	if inContinuation {
		header := frame.Header()
		if header.StreamID == expectingStreamID {
			if _, isContinuation := frame.(*http2.ContinuationFrame); !isContinuation {
				return p.GoAwayErr(0, http2.ErrCodeProtocol, []byte("non-CONTINUATION on stream expecting CONTINUATION"),
					fmt.Errorf("received non-CONTINUATION frame on stream %d while expecting CONTINUATION", header.StreamID))
			}
		} else {
			// RFC 7540 §6.10: any frame on a different stream during a header
			// block is a connection error of type PROTOCOL_ERROR.
			return p.GoAwayErr(p.manager.GetLastStreamID(), http2.ErrCodeProtocol,
				[]byte("frame on another stream during header block"),
				fmt.Errorf("frame type %T on stream %d while header block is open on %d", frame, header.StreamID, expectingStreamID))
		}
	}

	header := frame.Header()
	// RFC 7540 §4.2: inbound frames must not exceed OUR advertised
	// SETTINGS_MAX_FRAME_SIZE (localMaxFrameSize). The peer's advertised
	// value (manager.maxFrameSize) bounds what WE send, not what we accept.
	localMaxFrame := p.manager.GetLocalMaxFrameSize()

	switch frame.(type) {
	case *http2.DataFrame:
		if header.Length > localMaxFrame {
			if header.StreamID == 0 {
				return p.GoAwayErr(0, http2.ErrCodeFrameSize, []byte("DATA frame too large"),
					fmt.Errorf("DATA frame exceeds MAX_FRAME_SIZE: %d > %d", header.Length, localMaxFrame))
			}
			_ = p.writer.WriteRSTStream(header.StreamID, http2.ErrCodeFrameSize)
			p.flush()
			return fmt.Errorf("DATA frame exceeds MAX_FRAME_SIZE: %d > %d", header.Length, localMaxFrame)
		}
	case *http2.HeadersFrame:
		if header.Length > localMaxFrame {
			return p.GoAwayErr(0, http2.ErrCodeFrameSize, []byte("HEADERS frame too large"),
				fmt.Errorf("HEADERS frame exceeds MAX_FRAME_SIZE: %d > %d", header.Length, localMaxFrame))
		}
	}

	switch f := frame.(type) {
	case *http2.SettingsFrame:
		return p.handleSettings(f)
	case *http2.HeadersFrame:
		return p.handleHeaders(ctx, f)
	case *http2.DataFrame:
		return p.handleData(ctx, f)
	case *http2.WindowUpdateFrame:
		return p.handleWindowUpdate(f)
	case *http2.RSTStreamFrame:
		return p.handleRSTStream(f)
	case *http2.PriorityFrame:
		return p.handlePriority(f)
	case *http2.GoAwayFrame:
		return p.handleGoAway(f)
	case *http2.PingFrame:
		return p.handlePing(f)
	case *http2.ContinuationFrame:
		return p.handleContinuation(ctx, f)
	case *http2.PushPromiseFrame:
		return p.GoAwayErr(0, http2.ErrCodeProtocol, []byte("client sent PUSH_PROMISE"),
			fmt.Errorf("client sent PUSH_PROMISE frame"))
	default:
		return nil
	}
}

// ProcessFrameWithConn processes a frame with a connection context.
func (p *Processor) ProcessFrameWithConn(ctx context.Context, frame http2.Frame, conn ResponseWriter) error {
	p.currentConn = conn
	defer func() { p.currentConn = nil }()

	return p.ProcessFrame(ctx, frame)
}

// handleSettings processes SETTINGS frames.
//
//nolint:gocyclo // complex settings negotiation logic
func (p *Processor) handleSettings(f *http2.SettingsFrame) error {
	if f.IsAck() {
		return nil
	}

	type bufferedFlush struct {
		streamID  uint32
		data      []byte
		endStream bool
	}

	var validationErr error
	// What a SETTINGS_INITIAL_WINDOW_SIZE releases of the streams' buffered
	// DATA goes through sendBuffered, after the SETTINGS ACK on the wire. With
	// the conn layer's sink that holds by construction (the ACK is written to
	// the connection ahead of the queue); written to the frame writer, which
	// has no sink, the DATA is held here until the ACK is written.
	var pendingFlushes []bufferedFlush
	// Streams whose last buffered bytes went out with END_STREAM; closed once
	// the manager's lock is released.
	var finishedFlushes []uint32
	send := OutboundSend(p.sendBuffered)
	if p.outboundSink == nil {
		send = func(id uint32, endStream bool, data []byte) {
			pendingFlushes = append(pendingFlushes, bufferedFlush{streamID: id, data: append([]byte(nil), data...), endStream: endStream})
		}
	}

	_ = f.ForeachSetting(func(s http2.Setting) error {
		switch s.ID {
		case http2.SettingHeaderTableSize:
			// Peer's upper bound on our HPACK encoder dynamic table.
			// Stash the value so encoders can honor it.
			p.manager.mu.Lock()
			p.manager.headerTableSize = s.Val
			p.manager.mu.Unlock()
		case http2.SettingEnablePush:
			if s.Val != 0 && s.Val != 1 {
				validationErr = fmt.Errorf("SETTINGS_ENABLE_PUSH must be 0 or 1, got %d", s.Val)
				return validationErr
			}
			p.manager.mu.Lock()
			p.manager.pushEnabled = s.Val == 1
			p.manager.mu.Unlock()
		case http2.SettingMaxConcurrentStreams:
			// Client's MAX_CONCURRENT_STREAMS limits server-initiated streams (push)
		case http2.SettingInitialWindowSize:
			if s.Val > 0x7fffffff {
				validationErr = fmt.Errorf("SETTINGS_INITIAL_WINDOW_SIZE too large: %d", s.Val)
				_ = p.SendGoAway(p.manager.GetLastStreamID(), http2.ErrCodeFlowControl, []byte(validationErr.Error()))
				return validationErr
			}
			p.manager.mu.Lock()
			//nolint:gosec // G115: safe conversion, values validated <= 2^31-1 above
			oldWindowSize := int32(p.manager.initialWindowSize)
			//nolint:gosec // G115: safe conversion, values validated <= 2^31-1 above
			newWindowSize := int32(s.Val)
			delta := newWindowSize - oldWindowSize
			p.manager.initialWindowSize = s.Val

			for sid, stream := range p.manager.streams {
				oldWin := stream.windowSize.Load()
				if delta > 0 && oldWin > 2147483647-delta {
					p.manager.mu.Unlock()
					validationErr = fmt.Errorf("stream %d window overflow", sid)
					_ = p.SendGoAway(p.manager.GetLastStreamID(), http2.ErrCodeFlowControl, []byte(validationErr.Error()))
					return validationErr
				}
				newWin := stream.windowSize.Add(delta)

				// If the window became positive and the stream has buffered
				// data, send what both windows allow. A stream whose last
				// bytes went out with END_STREAM is closed below, after the
				// manager's lock is released.
				if newWin > 0 && p.manager.FlushOutbound(stream, send) {
					finishedFlushes = append(finishedFlushes, sid)
				}
			}
			p.manager.mu.Unlock()
			// The windows changed: a handler waiting for one re-checks
			// (celeris#893).
			p.manager.notifySendWindow()
		case http2.SettingMaxFrameSize:
			if s.Val < 16384 {
				validationErr = fmt.Errorf("SETTINGS_MAX_FRAME_SIZE too small: %d", s.Val)
				return validationErr
			}
			if s.Val > 16777215 {
				validationErr = fmt.Errorf("SETTINGS_MAX_FRAME_SIZE too large: %d", s.Val)
				return validationErr
			}
			p.manager.mu.Lock()
			atomic.StoreUint32(&p.manager.maxFrameSize, s.Val)
			p.manager.mu.Unlock()
			// Propagate to the frame writer so WriteData fragments by the
			// peer's SETTINGS_MAX_FRAME_SIZE (RFC 7540 §4.2). The interface
			// doesn't require SetMaxFrameSize — native engines implement it
			// on *frame.Writer, stdlib/test mocks may not.
			if setter, ok := p.writer.(interface{ SetMaxFrameSize(uint32) }); ok {
				setter.SetMaxFrameSize(s.Val)
			}
		case http2.SettingMaxHeaderListSize:
			// No specific validation
		}
		return nil
	})

	if validationErr != nil {
		return p.GoAwayErr(p.manager.GetLastStreamID(), http2.ErrCodeProtocol,
			[]byte(validationErr.Error()), validationErr)
	}

	// Send SETTINGS_ACK before flushing any pending DATA frames.
	// Peers expect the ACK to confirm settings are applied before
	// seeing frames that depend on the new settings.
	if err := p.writer.WriteSettingsAck(); err != nil {
		return err
	}
	p.flush()

	for _, pf := range pendingFlushes {
		_ = p.writer.WriteData(pf.streamID, pf.endStream, pf.data)
		p.flush()
	}
	// A stream whose buffered DATA all went out with END_STREAM is done.
	for _, sid := range finishedFlushes {
		if s, ok := p.manager.GetStream(sid); ok {
			switch s.GetState() {
			case StateHalfClosedRemote:
				s.SetState(StateClosed)
			case StateOpen:
				s.SetState(StateHalfClosedLocal)
			}
			p.manager.DeleteStream(sid)
		}
	}

	return nil
}

// canRunInline returns true if the stream can be executed inline on the event
// loop. Eligible: END_STREAM set (GET/HEAD), no CONTINUATION pending,
// connection window > 0 (can send response immediately).
func (p *Processor) canRunInline(stream *Stream) bool {
	if !stream.EndStream || p.continuationActive.Load() {
		return false
	}
	// Per-route async: a stream whose matched route opted into async
	// dispatch must run on the worker pool, not inline on the event
	// loop, so a blocking handler (DB, etc.) doesn't stall frame
	// processing for every other stream on the connection. Gated by
	// perRouteAsync so pure-sync servers skip the header scan + lookup.
	if p.perRouteAsync && p.streamRouteAsync(stream) {
		return false
	}
	return true
}

// streamRouteAsync extracts :method and :path from the stream's received
// pseudo-headers and asks the resolver whether the matched route is async.
// Returns false when the resolver is absent or the pseudo-headers are
// missing (malformed request — handled elsewhere; default to inline-eligible).
func (p *Processor) streamRouteAsync(stream *Stream) bool {
	if p.asyncResolver == nil {
		return false
	}
	var method, path string
	for _, h := range stream.Headers {
		switch h[0] {
		case ":method":
			method = h[1]
		case ":path":
			path = h[1]
		}
		if method != "" && path != "" {
			break
		}
	}
	if method == "" || path == "" {
		return false
	}
	// Strip the query string — the router matches on path only (mirrors
	// the Context's :path → c.path split at '?').
	for i := 0; i < len(path); i++ {
		if path[i] == '?' {
			path = path[:i]
			break
		}
	}
	return p.asyncResolver.RouteAsync(method, path)
}

// runHandler dispatches the stream handler. For inline-eligible streams
// (GET/HEAD with END_STREAM), executes synchronously on the event loop.
// Otherwise dispatches to the worker pool for concurrent execution.
func (p *Processor) runHandler(stream *Stream) {
	if p.handler == nil {
		return
	}
	// A stream opened above the last stream a GOAWAY named: its client
	// counts it as not processed and may retry it elsewhere, so its handler
	// must not run (celeris#759). REFUSED_STREAM says exactly that (RFC 9113
	// §8.7). Its headers were decoded all the same, so the HPACK state stays
	// in step with the client's. Once the RST_STREAM is written,
	// sendRSTStreamAndMarkClosed has deleted the stream, which puts it back
	// in the stream pool, so stream is not touched after the call: another
	// connection can have it by then.
	if p.goAwaySent && stream.ID > p.goAwayLastID {
		id := stream.ID
		if err := p.sendRSTStreamAndMarkClosed(id, http2.ErrCodeRefusedStream); err != nil {
			p.manager.DeleteStream(id)
		}
		return
	}

	// celeris#893: while the connection holds OutboundBudget bytes of
	// response DATA for the peer's window, a handler does not run inline. On
	// the event loop it could not wait for room, and whatever the windows
	// refused would be buffered on top; on the worker pool it sends what the
	// windows allow and waits for the rest (TryBufferOutbound). Its stream is
	// not held back: a response that fits the windows still goes out at once.
	inline := p.canRunInline(stream)
	if inline && !p.manager.overOutboundBudget() {
		p.executeHandlerInline(stream)
		return
	}

	// Check cancellation after inline (rare path, avoids atomic load on hot inline path).
	if stream.IsCancelled() {
		return
	}

	run := flagAsyncRunning
	if inline {
		// On the pool only for the budget: none of its response is buffered,
		// even if the budget has room again when it writes (flagBudgetPool,
		// TryBufferOutbound).
		run |= flagBudgetPool
	}
	stream.flags.Or(run)
	p.poolRunning.Add(1)
	globalH2Pool.Submit(p, stream)
}

// executeHandlerInline runs the handler synchronously on the event loop.
// Because it runs ON the event-loop thread (under H2State.mu), a buffered
// response (WriteResponse — the common inline GET/HEAD END_STREAM path) goes
// straight to outBuf via InlineWriter, skipping the sharded write queue's mutex
// + pooled buffer and — the real win — the eventfd self-wake syscall Enqueue
// fires on every response. Incremental streaming (StreamWriter) still routes
// through the queue: InlineWriter is a stream.Streamer that delegates its
// streaming methods to connWriter, so SSE/chunked stays safe (celeris#408).
// Falls back to connWriter when InlineWriter is unset (Processors built without
// the conn layer, e.g. unit tests).
func (p *Processor) executeHandlerInline(stream *Stream) {
	p.InlineCount++
	if p.InlineWriter != nil {
		stream.ResponseWriter = p.InlineWriter
	} else {
		stream.ResponseWriter = p.connWriter
	}

	if p.InlineCachedCtx != nil {
		stream.CachedCtx = p.InlineCachedCtx
	}

	var keepAlive bool

	defer func() {
		if r := recover(); r != nil {
			_ = r
		}

		if stream.Data != nil {
			stream.Data.Reset()
			bufferPool.Put(stream.Data)
			stream.Data = nil
		}

		// Save cached context before Release clears it.
		if stream.CachedCtx != nil {
			p.InlineCachedCtx = stream.CachedCtx
			stream.CachedCtx = nil
		}

		if keepAlive {
			return
		}

		// Free the MAX_CONCURRENT_STREAMS slot before removal. A handler that
		// returned without writing a response (error return, empty/204-style
		// handler, recovered panic) or a stream left half-closed-local still
		// counts as active, and RemoveStreamFromMap does NOT touch
		// activeStreams — so transition to Closed here, decrementing exactly
		// once via updateActiveCount. Idempotent: a fully-completed stream
		// already transitioned to Closed above is a no-op (no double-decrement).
		stream.SetState(StateClosed)
		p.manager.RemoveStreamFromMap(stream.ID)
		stream.Release()
	}()

	stream.SetHandlerStarted()

	err := p.handler.HandleStream(bgCtx, stream)
	// The handler is over: on HTTP/2 the stream ends with it, detached or not
	// (Context.Detach). A StreamWriter call from a goroutine it started is
	// refused from here on, before the check for buffered DATA below, so
	// nothing is added to what the stream keeps (celeris#904).
	stream.EndUse()
	if err != nil {
		return
	}

	if !stream.GetHeadersSent() {
		if stream.ResponseWriter != nil {
			_ = stream.ResponseWriter.WriteResponse(stream, 200, nil, nil)
		}
		return
	}

	// Keep the stream alive if outbound DATA is buffered (flow control
	// window=0). handleWindowUpdate/handleSettings will flush the DATA
	// and clean up the stream when the window opens. Without this, the
	// stream is removed and buffered DATA is never sent.
	// Fixes h2spec: WINDOW_UPDATE/PRIORITY on half-closed + negative SETTINGS.
	// A handler that returned without ending its response (a StreamWriter
	// never closed) has the buffer sent all the same, but no END_STREAM, and
	// the stream is released when it is out (AbandonOutbound): it would
	// otherwise hold its slot and its budget until the connection closes.
	if stream.AbandonOutbound() {
		keepAlive = true
		return
	}

	// Transition the stream state NOW so a fully-completed stream (END_STREAM
	// sent, no buffered outbound) stops counting toward MAX_CONCURRENT_STREAMS
	// immediately. Per RFC 7540 §5.1.2 only open / half-closed streams count,
	// so a closed stream must free its slot at once — otherwise a single recv
	// buffer carrying more than MAX_CONCURRENT_STREAMS pipelined complete
	// requests (e.g. 64 KB GETs whose responses backpressure TCP) would see
	// the 101st+ HEADERS spuriously REFUSED even though streams 1..100 are
	// already done. Inline handlers run sequentially, so at most one stream is
	// genuinely open at a time; a still-open stream never reaches this point
	// (it returns early above), so the concurrency limit stays enforced for
	// real concurrent streams.
	state := stream.GetState()
	if state == StateOpen || state == StateHalfClosedRemote {
		if state == StateHalfClosedRemote {
			stream.SetState(StateClosed)
		} else {
			stream.SetState(StateHalfClosedLocal)
		}
	}

	// When more frames follow in the recv buffer, defer only the map removal
	// and Release to FlushInlineCleanup (after the frame loop) so later frames
	// in the same batch that reference this stream still find it. The state
	// transition above already ran; FlushInlineCleanup re-applies it
	// idempotently before removing the stream.
	if p.hasMoreFrames {
		p.pendingInlineCleanup = append(p.pendingInlineCleanup, inlineCleanup{stream, stream.ID})
		keepAlive = true
		return
	}
}

// executeHandler executes the handler on a worker pool goroutine. It owns the
// stream lifecycle: on completion it removes the stream from the manager and
// releases it back to the pool. If outbound data is buffered (flow control),
// the stream stays in the map for the event loop to flush via WINDOW_UPDATE.
func (p *Processor) executeHandler(stream *Stream) {
	// Deferred first, so it runs last: the response is queued and the
	// stream settled before a shutdown that waits for this count can close
	// the connection (celeris#759).
	defer p.poolRunning.Add(-1)
	defer func() {
		if r := recover(); r != nil {
			_ = r // last-resort panic recovery
		}

		// Eagerly release the input buffer now that the handler is done.
		// This reduces peak concurrent buffer memory under high stream concurrency.
		if stream.Data != nil {
			stream.Data.Reset()
			bufferPool.Put(stream.Data)
			stream.Data = nil
		}

		// If there's buffered outbound data waiting for WINDOW_UPDATE,
		// keep the stream in the map so the event loop can flush it:
		// handOffBuffered clears asyncRunning, so DeleteStream (on a
		// RST_STREAM or connection close) will release it, and
		// handleWindowUpdate cleans up after a full flush. A stream that
		// was taken out of the map while its handler ran is not handed
		// over: nothing would find it again, so it is retired here (the loop
		// releases it), with what it still buffers (celeris#948).
		stream.mu.RLock()
		hasPending := stream.OutboundBuffer != nil && stream.OutboundBuffer.Len() > 0
		stream.mu.RUnlock()
		if hasPending && p.manager.handOffBuffered(stream) {
			return
		}

		// Free the MAX_CONCURRENT_STREAMS slot before removal (see
		// executeHandlerInline): a no-write handler, an error return, or a
		// half-closed-local stream still counts as active and the removal
		// does not decrement, so transition to Closed here (idempotent for an
		// already-Closed stream — no double-decrement).
		//
		// The stream is not released here: the event loop may be using it,
		// from a lookup, this very moment. retire takes it out of the map and
		// leaves its release to the loop (celeris#951), and this goroutine
		// does not touch it again.
		stream.SetState(StateClosed)
		p.manager.retire(stream)
	}()

	stream.SetHandlerStarted()

	err := p.handler.HandleStream(bgCtx, stream)
	stream.EndUse() // as in executeHandlerInline
	if err != nil {
		return
	}

	headersSent := stream.GetHeadersSent()
	state := stream.GetState()

	if !headersSent {
		if stream.ResponseWriter != nil {
			_ = stream.ResponseWriter.WriteResponse(stream, 200, nil, nil)
		}
		return
	}

	if state == StateOpen || state == StateHalfClosedRemote {
		// Under the stream's lock, as every access of the buffer is: the
		// event loop sends and resets it on a WINDOW_UPDATE (celeris#822).
		stream.mu.RLock()
		pending := stream.OutboundBuffer != nil && stream.OutboundBuffer.Len() > 0
		stream.mu.RUnlock()
		if pending {
			return
		}

		if state == StateHalfClosedRemote {
			stream.SetState(StateClosed)
		} else {
			stream.SetState(StateHalfClosedLocal)
		}
	}
}

// handleHeaders processes HEADERS frames.
//
//nolint:gocyclo // complex header block assembly and stream lifecycle logic
func (p *Processor) handleHeaders(_ context.Context, f *http2.HeadersFrame) error {
	existingStream, exists := p.manager.GetStream(f.StreamID)

	if !f.HeadersEnded() {
		headerBlock := f.HeaderBlockFragment()
		frag := make([]byte, len(headerBlock))
		copy(frag, headerBlock)
		p.continuationStateMu.Lock()
		p.continuationState = &ContinuationState{
			streamID:      f.StreamID,
			headerBlock:   frag,
			endStream:     f.StreamEnded(),
			expectingMore: true,
			isTrailers:    false,
		}
		p.continuationActive.Store(true)
		p.continuationStateMu.Unlock()
		return nil
	}

	if !exists {
		lastClientStream := p.manager.lastClientStream.Load()

		if f.StreamID <= lastClientStream {
			_ = p.SendGoAway(lastClientStream, http2.ErrCodeProtocol, []byte("HEADERS on closed stream (reused id)"))
			return fmt.Errorf("HEADERS frame on closed stream %d (last stream: %d)", f.StreamID, lastClientStream)
		}

		if err := validateStreamID(f.StreamID, lastClientStream, false); err != nil {
			return p.GoAwayErr(lastClientStream, http2.ErrCodeProtocol, []byte(err.Error()), err)
		}

		// Frame processing is serial (under H2State.mu), so Store is safe.
		if f.StreamID > lastClientStream {
			p.manager.lastClientStream.Store(f.StreamID)
		}
	}

	if exists {
		state := existingStream.GetState()
		switch state {
		case StateClosed:
			_ = p.SendGoAway(p.manager.GetLastStreamID(), http2.ErrCodeStreamClosed, []byte("HEADERS on closed stream (state closed)"))
			return fmt.Errorf("HEADERS frame on closed stream %d", f.StreamID)
		case StateHalfClosedRemote:
			_ = p.SendGoAway(p.manager.GetLastStreamID(), http2.ErrCodeStreamClosed, []byte("HEADERS on half-closed stream"))
			return fmt.Errorf("HEADERS frame on half-closed (remote) stream %d", f.StreamID)
		}

		if existingStream.ReceivedInitialHeaders {
			if !f.StreamEnded() {
				return p.GoAwayErr(p.manager.GetLastStreamID(), http2.ErrCodeProtocol, []byte("second HEADERS without END_STREAM"),
					fmt.Errorf("second HEADERS without END_STREAM on stream %d", f.StreamID))
			}
			headerBlock := f.HeaderBlockFragment()
			if !f.HeadersEnded() {
				frag := make([]byte, len(headerBlock))
				copy(frag, headerBlock)
				p.continuationStateMu.Lock()
				p.continuationState = &ContinuationState{
					streamID:      f.StreamID,
					headerBlock:   frag,
					endStream:     f.StreamEnded(),
					expectingMore: true,
					isTrailers:    true,
				}
				p.continuationActive.Store(true)
				p.continuationStateMu.Unlock()
				return nil
			}

			pooledTrailers := headersSlicePoolIn.Get().(*[][2]string)
			*pooledTrailers = (*pooledTrailers)[:0]
			defer func() {
				*pooledTrailers = (*pooledTrailers)[:0]
				headersSlicePoolIn.Put(pooledTrailers)
			}()
			p.beginHeaderDecode(pooledTrailers, false)
			p.ensureHPACKDecoder()
			if _, err := p.hpackDecoder.Write(headerBlock); err != nil {
				return p.GoAwayErr(0, http2.ErrCodeCompression, []byte("HPACK decoding failed"),
					fmt.Errorf("failed to decode trailers: %w", err))
			}
			if err := p.hpackDecoder.Close(); err != nil {
				return p.GoAwayErr(0, http2.ErrCodeCompression, []byte("HPACK decoding failed"),
					fmt.Errorf("failed to finalize trailers: %w", err))
			}
			p.endHeaderDecode()
			trailers := *pooledTrailers
			if err := validateTrailerHeaders(trailers); err != nil {
				_ = p.sendRSTStreamAndMarkClosed(f.StreamID, http2.ErrCodeProtocol)
				return fmt.Errorf("invalid trailers: %w", err)
			}
			existingStream.AddHeadersBatch(trailers)

			if f.StreamEnded() {
				existingStream.EndStream = true
				existingStream.SetState(StateHalfClosedRemote)
				p.runHandler(existingStream)
			}
			return nil
		}
	}

	stream, ok := p.manager.TryOpenStream(f.StreamID)
	if !ok {
		_ = p.sendRSTStreamAndMarkClosed(f.StreamID, http2.ErrCodeRefusedStream)
		return fmt.Errorf("exceeds MAX_CONCURRENT_STREAMS")
	}

	if err := validateStreamState(stream.GetState(), http2.FrameHeaders, f.StreamEnded()); err != nil {
		sendStreamError(p.writer, f.StreamID, http2.ErrCodeStreamClosed)
		return err
	}

	if stream.ResponseWriter == nil {
		stream.ResponseWriter = p.connWriter
	}

	headerBlock := f.HeaderBlockFragment()

	if dependency, weight, exclusive, hasPriority := ParsePriorityFromHeaders(f); hasPriority {
		if dependency == f.StreamID {
			return p.GoAwayErr(p.manager.GetLastStreamID(), http2.ErrCodeProtocol, []byte("stream depends on itself"),
				fmt.Errorf("stream %d depends on itself", f.StreamID))
		}
		p.manager.priorityTree.UpdateFromFrame(f.StreamID, dependency, weight, exclusive)
	}

	if !f.HeadersEnded() {
		pooled := headerBlockPool.Get().(*[]byte)
		frag := (*pooled)[:0]
		frag = append(frag, headerBlock...)
		p.continuationStateMu.Lock()
		p.continuationState = &ContinuationState{
			streamID:      f.StreamID,
			headerBlock:   frag,
			endStream:     f.StreamEnded(),
			expectingMore: true,
			isTrailers:    false,
		}
		p.continuationActive.Store(true)
		p.continuationStateMu.Unlock()
		// TryOpenStream already set state to StateOpen; skip redundant Swap.
		if stream.ResponseWriter == nil {
			stream.ResponseWriter = p.connWriter
		}
		return nil
	}

	pooledHeadersIn := headersSlicePoolIn.Get().(*[][2]string)
	*pooledHeadersIn = (*pooledHeadersIn)[:0]
	defer func() {
		*pooledHeadersIn = (*pooledHeadersIn)[:0]
		headersSlicePoolIn.Put(pooledHeadersIn)
	}()
	p.beginHeaderDecode(pooledHeadersIn, true)
	p.ensureHPACKDecoder()
	if _, err := p.hpackDecoder.Write(headerBlock); err != nil {
		return p.GoAwayErr(0, http2.ErrCodeCompression, []byte("HPACK decoding failed"),
			fmt.Errorf("failed to decode headers: %w", err))
	}
	if err := p.hpackDecoder.Close(); err != nil {
		return p.GoAwayErr(0, http2.ErrCodeCompression, []byte("HPACK decoding failed"),
			fmt.Errorf("failed to finalize headers: %w", err))
	}
	p.endHeaderDecode()

	headers := *pooledHeadersIn
	if err := validateRequestHeaders(headers); err != nil {
		sendStreamError(p.writer, f.StreamID, http2.ErrCodeProtocol)
		return fmt.Errorf("invalid headers: %w", err)
	}
	for _, h := range headers {
		if h[0] == ":method" && h[1] == "HEAD" {
			stream.IsHEAD = true
			break
		}
	}
	stream.AddHeadersBatch(headers)
	stream.ReceivedInitialHeaders = true

	if f.StreamEnded() {
		stream.EndStream = true
		stream.SetState(StateHalfClosedRemote)

		if err := validateContentLength(stream.Headers, stream.ReceivedDataLen); err != nil {
			sendStreamError(p.writer, f.StreamID, http2.ErrCodeProtocol)
			return fmt.Errorf("content-length mismatch: %w", err)
		}

		p.runHandler(stream)
	}

	return nil
}

// ProcessRawHeaders handles a HEADERS frame from raw bytes, bypassing the
// x/net framer's *HeadersFrame allocation. Only valid for simple HEADERS
// (END_HEADERS set, no PADDED, no PRIORITY, not during CONTINUATION).
// All RFC 7540 validations are preserved.
func (p *Processor) ProcessRawHeaders(streamID uint32, endStream bool, headerBlock []byte) error {
	existingStream, exists := p.manager.GetStream(streamID)

	if !exists {
		lastClientStream := p.manager.lastClientStream.Load()

		if streamID <= lastClientStream {
			_ = p.SendGoAway(lastClientStream, http2.ErrCodeProtocol, []byte("HEADERS on closed stream (reused id)"))
			return fmt.Errorf("HEADERS frame on closed stream %d (last stream: %d)", streamID, lastClientStream)
		}

		if err := validateStreamID(streamID, lastClientStream, false); err != nil {
			return p.GoAwayErr(lastClientStream, http2.ErrCodeProtocol, []byte(err.Error()), err)
		}

		if streamID > lastClientStream {
			p.manager.lastClientStream.Store(streamID)
		}
	}

	if exists {
		state := existingStream.GetState()
		switch state {
		case StateClosed:
			_ = p.SendGoAway(p.manager.GetLastStreamID(), http2.ErrCodeStreamClosed, []byte("HEADERS on closed stream (state closed)"))
			return fmt.Errorf("HEADERS frame on closed stream %d", streamID)
		case StateHalfClosedRemote:
			_ = p.SendGoAway(p.manager.GetLastStreamID(), http2.ErrCodeStreamClosed, []byte("HEADERS on half-closed stream"))
			return fmt.Errorf("HEADERS frame on half-closed (remote) stream %d", streamID)
		}

		if existingStream.ReceivedInitialHeaders {
			if !endStream {
				return p.GoAwayErr(p.manager.GetLastStreamID(), http2.ErrCodeProtocol, []byte("second HEADERS without END_STREAM"),
					fmt.Errorf("second HEADERS without END_STREAM on stream %d", streamID))
			}
			// Trailers on existing stream.
			pooledTrailers := headersSlicePoolIn.Get().(*[][2]string)
			*pooledTrailers = (*pooledTrailers)[:0]
			defer func() {
				*pooledTrailers = (*pooledTrailers)[:0]
				headersSlicePoolIn.Put(pooledTrailers)
			}()
			p.beginHeaderDecode(pooledTrailers, false)
			p.ensureHPACKDecoder()
			if _, err := p.hpackDecoder.Write(headerBlock); err != nil {
				return p.GoAwayErr(0, http2.ErrCodeCompression, []byte("HPACK decoding failed"),
					fmt.Errorf("failed to decode trailers: %w", err))
			}
			if err := p.hpackDecoder.Close(); err != nil {
				return p.GoAwayErr(0, http2.ErrCodeCompression, []byte("HPACK decoding failed"),
					fmt.Errorf("failed to finalize trailers: %w", err))
			}
			p.endHeaderDecode()
			trailers := *pooledTrailers
			if err := validateTrailerHeaders(trailers); err != nil {
				_ = p.sendRSTStreamAndMarkClosed(streamID, http2.ErrCodeProtocol)
				return fmt.Errorf("invalid trailers: %w", err)
			}
			existingStream.AddHeadersBatch(trailers)

			if endStream {
				existingStream.EndStream = true
				existingStream.SetState(StateHalfClosedRemote)
				p.runHandler(existingStream)
			}
			return nil
		}
	}

	// New stream — open it.
	stream, ok := p.manager.TryOpenStream(streamID)
	if !ok {
		sendStreamError(p.writer, streamID, http2.ErrCodeRefusedStream)
		return nil
	}
	if stream.ResponseWriter == nil {
		stream.ResponseWriter = p.connWriter
	}

	// No PRIORITY handling needed (flag checked by caller).

	pooledHeadersIn := headersSlicePoolIn.Get().(*[][2]string)
	*pooledHeadersIn = (*pooledHeadersIn)[:0]
	defer func() {
		*pooledHeadersIn = (*pooledHeadersIn)[:0]
		headersSlicePoolIn.Put(pooledHeadersIn)
	}()
	p.beginHeaderDecode(pooledHeadersIn, true)
	p.ensureHPACKDecoder()
	if _, err := p.hpackDecoder.Write(headerBlock); err != nil {
		return p.GoAwayErr(0, http2.ErrCodeCompression, []byte("HPACK decoding failed"),
			fmt.Errorf("failed to decode headers: %w", err))
	}
	if err := p.hpackDecoder.Close(); err != nil {
		return p.GoAwayErr(0, http2.ErrCodeCompression, []byte("HPACK decoding failed"),
			fmt.Errorf("failed to finalize headers: %w", err))
	}
	p.endHeaderDecode()

	headers := *pooledHeadersIn
	if err := validateRequestHeaders(headers); err != nil {
		sendStreamError(p.writer, streamID, http2.ErrCodeProtocol)
		return fmt.Errorf("invalid headers: %w", err)
	}
	for _, h := range headers {
		if h[0] == ":method" && h[1] == "HEAD" {
			stream.IsHEAD = true
			break
		}
	}
	stream.AddHeadersBatch(headers)
	stream.ReceivedInitialHeaders = true

	if endStream {
		stream.EndStream = true
		stream.SetState(StateHalfClosedRemote)

		if err := validateContentLength(stream.Headers, stream.ReceivedDataLen); err != nil {
			sendStreamError(p.writer, streamID, http2.ErrCodeProtocol)
			return fmt.Errorf("content-length mismatch: %w", err)
		}

		p.runHandler(stream)
	}

	return nil
}

// handleData processes DATA frames.
func (p *Processor) handleData(_ context.Context, f *http2.DataFrame) error {
	stream, ok := p.manager.GetStream(f.StreamID)
	if !ok {
		return p.GoAwayErr(0, http2.ErrCodeProtocol, []byte("DATA on idle stream"),
			fmt.Errorf("DATA frame on idle stream %d", f.StreamID))
	}

	state := stream.GetState()
	if state == StateClosed {
		_ = p.writer.WriteRSTStream(f.StreamID, http2.ErrCodeStreamClosed)
		p.flush()
		return fmt.Errorf("DATA frame on closed stream %d", f.StreamID)
	}

	if err := validateStreamState(state, http2.FrameData, f.StreamEnded()); err != nil {
		_ = p.sendRSTStreamAndMarkClosed(f.StreamID, http2.ErrCodeStreamClosed)
		return err
	}

	dataLen := len(f.Data())
	stream.ReceivedDataLen += dataLen

	// Reject streams that exceed the maximum request body size (100 MB).
	if limit := p.maxBodySize(); limit > 0 && int64(stream.ReceivedDataLen) > limit {
		_ = p.sendRSTStreamAndMarkClosed(f.StreamID, http2.ErrCodeCancel)
		return fmt.Errorf("request body exceeds %d bytes on stream %d", limit, f.StreamID)
	}

	if err := stream.AddData(f.Data()); err != nil {
		return err
	}

	//nolint:gosec // G115: safe conversion, dataLen is frame payload size
	updateLen := uint32(dataLen)

	p.manager.AccumulateWindowUpdate(f.StreamID, updateLen)
	p.manager.AccumulateWindowUpdate(0, updateLen)

	if f.StreamEnded() {
		p.manager.FlushWindowUpdates(p.writer, true)
		stream.EndStream = true
		stream.SetState(StateHalfClosedRemote)

		if err := validateContentLength(stream.Headers, stream.ReceivedDataLen); err != nil {
			sendStreamError(p.writer, stream.ID, http2.ErrCodeProtocol)
			return fmt.Errorf("content-length mismatch: %w", err)
		}

		p.runHandler(stream)
	} else {
		p.manager.FlushWindowUpdates(p.writer, false)
	}

	return nil
}

// handleWindowUpdate processes WINDOW_UPDATE frames.
func (p *Processor) handleWindowUpdate(f *http2.WindowUpdateFrame) error {
	if f.Increment == 0 {
		if f.StreamID == 0 {
			return p.GoAwayErr(0, http2.ErrCodeProtocol, []byte("WINDOW_UPDATE increment is 0"),
				fmt.Errorf("WINDOW_UPDATE with 0 increment"))
		}
		// RFC 7540 §6.9.1: zero increment on a stream is a stream error.
		_ = p.sendRSTStreamAndMarkClosed(f.StreamID, http2.ErrCodeProtocol)
		return fmt.Errorf("WINDOW_UPDATE with 0 increment on stream %d", f.StreamID)
	}

	if f.StreamID == 0 {
		for {
			oldWin := atomic.LoadInt32(&p.manager.connectionWindow)
			newWin := int64(oldWin) + int64(f.Increment)
			if newWin > 0x7fffffff {
				return p.GoAwayErr(0, http2.ErrCodeFlowControl, []byte("connection window overflow"),
					fmt.Errorf("connection window overflow: %d + %d > 2^31-1", oldWin, f.Increment))
			}
			//nolint:gosec // G115: safe conversion, newWin validated above
			if atomic.CompareAndSwapInt32(&p.manager.connectionWindow, oldWin, int32(newWin)) {
				break
			}
		}
		// Connection-level credit may unblock streams that stalled on the
		// CONNECTION window (their per-stream window had room but the shared
		// connection window was exhausted). Re-flush every such stream — the
		// mirror of the per-stream branch below. Without this a stream that
		// ran the connection window to 0 would never resume and the response
		// would hang.
		p.flushConnWindowStalledStreams()
		p.manager.notifySendWindow() // celeris#893
		return nil
	}

	stream, ok := p.manager.GetStream(f.StreamID)
	if !ok {
		if f.StreamID <= p.manager.GetLastClientStreamID() {
			return nil
		}
		return p.GoAwayErr(p.manager.GetLastStreamID(), http2.ErrCodeProtocol,
			[]byte("WINDOW_UPDATE on idle stream"),
			fmt.Errorf("WINDOW_UPDATE on idle stream %d", f.StreamID))
	}

	streamState := stream.GetState()
	if streamState == StateClosed {
		return nil
	}

	// CAS loop for atomic window update with overflow check.
	for {
		old := stream.LoadWindowSize()
		newWindow := int64(old) + int64(f.Increment)
		if newWindow > 0x7fffffff {
			// RST and reclaim the slot via the shared close path so the
			// active stream's MAX_CONCURRENT_STREAMS slot is freed.
			_ = p.sendRSTStreamAndMarkClosed(f.StreamID, http2.ErrCodeFlowControl)
			return fmt.Errorf("stream %d window overflow: %d + %d > 2^31-1", f.StreamID, old, f.Increment)
		}
		//nolint:gosec // G115: safe conversion, newWindow validated <= 2^31-1 above
		if stream.CompareAndSwapWindowSize(old, int32(newWindow)) {
			break
		}
	}

	// A pool handler waiting for this stream's window (celeris#893).
	p.manager.notifySendWindow()

	// The channel is read before the flush: a flush that finishes the stream
	// deletes it, which returns it to the stream pool, and the pooled object
	// is any connection's next stream (celeris#951).
	windowUpd := stream.ReceivedWindowUpd

	// Flush buffered outbound data now that per-stream window space is
	// available (clamped + debited against the connection window too).
	if p.flushStreamOutbound(stream) {
		switch streamState {
		case StateHalfClosedRemote:
			stream.SetState(StateClosed)
		case StateOpen:
			stream.SetState(StateHalfClosedLocal)
		}
		p.manager.DeleteStream(f.StreamID)
	}

	if windowUpd != nil {
		select {
		//nolint:gosec // G115: safe conversion
		case windowUpd <- int32(f.Increment):
		default:
		}
	}
	return nil
}

// flushStreamOutbound sends as much of a stream's buffered outbound DATA as
// the current per-stream AND connection send windows allow, debiting both
// windows (RFC 9113 §6.9.1). It returns true only when the entire buffer was
// flushed AND it carried END_STREAM — i.e. the stream is now fully sent and
// the caller should transition it to closed and reclaim its slot. A stream
// that still has bytes buffered (because either window was exhausted mid-flush)
// returns false and stays alive for the next WINDOW_UPDATE. The DATA goes
// through sendBuffered, behind the stream's HEADERS (celeris#903).
func (p *Processor) flushStreamOutbound(s *Stream) bool {
	return p.manager.FlushOutbound(s, p.sendBuffered)
}

// SetOutboundSink makes send the way the event loop's flushes of buffered DATA
// (a WINDOW_UPDATE, a SETTINGS_INITIAL_WINDOW_SIZE) reach the connection, in
// place of the frame writer. The conn layer passes its write queue, so a
// stream's buffered DATA follows, in the stream's order, the HEADERS and the
// DATA its handler has queued; written to the frame writer instead, it could
// reach the connection ahead of a HEADERS that was still in the queue, and the
// peer would see DATA on a stream with no HEADERS (celeris#903).
//
// send is called on the event loop, from ProcessFrame, with the stream's lock
// held. It must consume or copy the bytes before it returns, and the frames it
// produces must reach the connection after what ProcessFrame wrote to the
// frame writer (the SETTINGS ACK before the DATA a SETTINGS frame releases).
func (p *Processor) SetOutboundSink(send OutboundSend) { p.outboundSink = send }

// sendBuffered writes buffered DATA the event loop flushes: to the outbound
// sink when the conn layer set one, to the frame writer otherwise.
func (p *Processor) sendBuffered(id uint32, endStream bool, data []byte) {
	if p.outboundSink != nil {
		p.outboundSink(id, endStream, data)
		return
	}
	_ = p.writer.WriteData(id, endStream, data)
	p.flush()
}

// flushConnWindowStalledStreams re-flushes every stream that still has
// buffered outbound DATA after a connection-level WINDOW_UPDATE(0) credited
// the shared window. A stream whose per-stream window had room but which
// stalled because the connection window hit zero is invisible to the
// per-stream WINDOW_UPDATE path (the peer only sends connection credit), so
// this is the sole place such a stream resumes. Fully-sent streams are
// transitioned to closed and have their MAX_CONCURRENT_STREAMS slot reclaimed,
// mirroring the per-stream branch.
func (p *Processor) flushConnWindowStalledStreams() {
	// Nothing can be unblocked if the connection window is still empty.
	if atomic.LoadInt32(&p.manager.connectionWindow) <= 0 {
		return
	}
	// Snapshot the candidate streams under the manager lock; flushing itself
	// takes per-stream locks and may call DeleteStream, so it must run outside
	// the manager read lock. The scratch slice is reused across calls to avoid
	// allocating on this path.
	candidates := p.connFlushScratch[:0]
	p.manager.mu.RLock()
	for _, s := range p.manager.streams {
		if s.GetState() == StateClosed {
			continue
		}
		candidates = append(candidates, s)
	}
	p.manager.mu.RUnlock()

	for _, s := range candidates {
		// Bail out early once the connection window is exhausted again —
		// no further stream can make progress until the next credit.
		if atomic.LoadInt32(&p.manager.connectionWindow) <= 0 {
			break
		}
		streamState := s.GetState()
		if streamState == StateClosed {
			continue
		}
		if p.flushStreamOutbound(s) {
			switch streamState {
			case StateHalfClosedRemote:
				s.SetState(StateClosed)
			case StateOpen:
				s.SetState(StateHalfClosedLocal)
			}
			p.manager.DeleteStream(s.ID)
		}
	}

	// Retain the grown backing array for the next call but drop the *Stream
	// references so released (pooled) streams aren't pinned between calls.
	clear(candidates)
	p.connFlushScratch = candidates[:0]
}

// handleRSTStream processes RST_STREAM frames. Enforces a rate limit
// to mitigate CVE-2023-44487 "Rapid Reset" — a client that opens and
// RST_STREAMs a stream in a tight loop consumes per-stream resources
// (HPACK state, priority tree, handler dispatch) without paying the
// usual connection-level flow-control cost.
func (p *Processor) handleRSTStream(f *http2.RSTStreamFrame) error {
	// Sliding one-second window counter.
	nowSec := time.Now().Unix()
	if nowSec != p.rstWindowSec {
		p.rstWindowSec = nowSec
		p.rstCount = 0
	}
	p.rstCount++
	if p.rstCount > rstBurstMax {
		return p.GoAwayErr(p.manager.GetLastStreamID(), http2.ErrCodeEnhanceYourCalm,
			[]byte("RST_STREAM flood"),
			fmt.Errorf("RST_STREAM rate exceeded %d/s (burst %d)", rstRateLimitPerSec, rstBurstMax))
	}

	// Closed, marked, cancelled (which signals a pool handler still
	// running) and deleted in one step under the manager's lock: the stream
	// is not looked up first and touched after, when its pool handler's
	// goroutine may have released it (resetStream, celeris#951). The
	// goroutine still releases it if its handler runs.
	if p.manager.resetStream(f.StreamID) {
		return nil
	}
	if f.StreamID <= p.manager.GetLastClientStreamID() {
		return nil
	}
	return p.GoAwayErr(p.manager.GetLastStreamID(), http2.ErrCodeProtocol,
		[]byte("RST_STREAM on idle stream"),
		fmt.Errorf("RST_STREAM on idle stream %d", f.StreamID))
}

// handlePriority processes PRIORITY frames.
func (p *Processor) handlePriority(f *http2.PriorityFrame) error {
	if f.StreamDep == f.StreamID {
		return p.GoAwayErr(p.manager.GetLastStreamID(), http2.ErrCodeProtocol, []byte("stream depends on itself"),
			fmt.Errorf("stream %d depends on itself", f.StreamID))
	}

	// Reject PRIORITY on stream IDs far beyond the last-opened
	// client stream — an attacker could otherwise blast PRIORITY
	// frames on monotonically-increasing IDs and grow the priority
	// tree unboundedly (maps + GetOrCreateStream inserts). Honest
	// clients only reference streams they've opened or are about
	// to open; anything more than 1024 ahead is an anomaly.
	lastClient := p.manager.GetLastClientStreamID()
	if f.StreamID > lastClient+2048 {
		return p.GoAwayErr(p.manager.GetLastStreamID(), http2.ErrCodeProtocol,
			[]byte("PRIORITY stream ID too far ahead"),
			fmt.Errorf("PRIORITY stream %d > last client %d + window", f.StreamID, lastClient))
	}

	_, exists := p.manager.GetStream(f.StreamID)
	if !exists {
		stream := p.manager.GetOrCreateStream(f.StreamID)
		stream.SetState(StateIdle)
	}

	p.manager.priorityTree.UpdateFromFrame(
		f.StreamID,
		f.StreamDep,
		f.Weight,
		f.Exclusive,
	)
	return nil
}

// handleGoAway processes GOAWAY frames.
func (p *Processor) handleGoAway(f *http2.GoAwayFrame) error {
	lastStreamID := f.LastStreamID

	p.manager.mu.Lock()
	defer p.manager.mu.Unlock()

	for streamID, stream := range p.manager.streams {
		if streamID > lastStreamID {
			prev := stream.GetState()
			stream.state.Store(int32(StateClosed))
			p.manager.updateActiveCount(prev, StateClosed)
			stream.Cancel()
			if stream.flags.Load()&flagAsyncRunning == 0 {
				stream.Release()
			}
			delete(p.manager.streams, streamID)
			p.manager.priorityTree.RemoveStream(streamID)
		}
	}

	return nil
}

// handlePing processes PING frames.
func (p *Processor) handlePing(f *http2.PingFrame) error {
	if f.Header().StreamID != 0 {
		return p.GoAwayErr(0, http2.ErrCodeProtocol, []byte("PING on non-zero stream"),
			fmt.Errorf("PING frame with non-zero stream id: %d", f.Header().StreamID))
	}

	if !f.IsAck() {
		if err := p.writer.WritePing(true, f.Data); err != nil {
			return err
		}
		p.flush()
	}
	return nil
}

// handleContinuation processes CONTINUATION frames.
func (p *Processor) handleContinuation(_ context.Context, f *http2.ContinuationFrame) error {
	p.continuationStateMu.Lock()
	defer p.continuationStateMu.Unlock()

	if p.continuationState == nil || !p.continuationState.expectingMore {
		return p.GoAwayErr(0, http2.ErrCodeProtocol, []byte("unexpected CONTINUATION"),
			fmt.Errorf("unexpected CONTINUATION frame on stream %d", f.StreamID))
	}

	if p.continuationState.streamID != f.StreamID {
		return p.GoAwayErr(0, http2.ErrCodeProtocol, []byte("CONTINUATION on wrong stream"),
			fmt.Errorf("CONTINUATION frame on wrong stream: expected %d, got %d",
				p.continuationState.streamID, f.StreamID))
	}

	p.continuationState.headerBlock = append(
		p.continuationState.headerBlock,
		f.HeaderBlockFragment()...,
	)

	if f.HeadersEnded() {
		stream := p.manager.GetOrCreateStream(f.StreamID)
		stream.SetState(StateOpen)

		pooledHeadersIn := headersSlicePoolIn.Get().(*[][2]string)
		*pooledHeadersIn = (*pooledHeadersIn)[:0]
		defer func() {
			*pooledHeadersIn = (*pooledHeadersIn)[:0]
			headersSlicePoolIn.Put(pooledHeadersIn)
		}()
		p.beginHeaderDecode(pooledHeadersIn, !p.continuationState.isTrailers)

		p.ensureHPACKDecoder()
		if _, err := p.hpackDecoder.Write(p.continuationState.headerBlock); err != nil {
			p.continuationState = nil
			p.continuationActive.Store(false)
			return p.GoAwayErr(0, http2.ErrCodeCompression, []byte("HPACK decoding failed"),
				fmt.Errorf("failed to decode headers: %w", err))
		}
		if err := p.hpackDecoder.Close(); err != nil {
			p.continuationState = nil
			p.continuationActive.Store(false)
			return p.GoAwayErr(0, http2.ErrCodeCompression, []byte("HPACK decoding failed"),
				fmt.Errorf("failed to finalize headers: %w", err))
		}
		p.endHeaderDecode()

		headers := *pooledHeadersIn
		if p.continuationState.isTrailers {
			if err := validateTrailerHeaders(headers); err != nil {
				p.continuationState = nil
				p.continuationActive.Store(false)
				_ = p.sendRSTStreamAndMarkClosed(f.StreamID, http2.ErrCodeProtocol)
				return fmt.Errorf("invalid trailers: %w", err)
			}
			stream.AddHeadersBatch(headers)
		} else {
			if err := validateRequestHeaders(headers); err != nil {
				p.continuationState = nil
				p.continuationActive.Store(false)
				sendStreamError(p.writer, f.StreamID, http2.ErrCodeProtocol)
				return fmt.Errorf("invalid headers: %w", err)
			}
			for _, h := range headers {
				if h[0] == ":method" && h[1] == "HEAD" {
					stream.IsHEAD = true
					break
				}
			}
			stream.AddHeadersBatch(headers)
			stream.ReceivedInitialHeaders = true
			if stream.ResponseWriter == nil {
				stream.ResponseWriter = p.connWriter
			}
		}

		if p.continuationState.endStream {
			stream.EndStream = true
			stream.SetState(StateHalfClosedRemote)
			p.runHandler(stream)
		}

		if p.continuationState != nil {
			b := p.continuationState.headerBlock
			pooled := b[:0]
			headerBlockPool.Put(&pooled)
		}
		p.continuationState = nil
		p.continuationActive.Store(false)
	}

	return nil
}

// flush flushes the writer if it supports the Flush method.
func (p *Processor) flush() {
	if flusher, ok := p.writer.(interface{ Flush() error }); ok {
		_ = flusher.Flush()
	}
}

// GoAwayErr sends a GOAWAY frame and returns the given error.
// SendGoAway handles flushing internally.
func (p *Processor) GoAwayErr(lastStreamID uint32, code http2.ErrCode, debug []byte, err error) error {
	_ = p.SendGoAway(lastStreamID, code, debug)
	return err
}

// SendGoAway sends a GOAWAY frame, and records it: no stream above
// lastStreamID is served from then on (runHandler).
func (p *Processor) SendGoAway(lastStreamID uint32, code http2.ErrCode, debugData []byte) error {
	if !p.goAwaySent || lastStreamID < p.goAwayLastID {
		p.goAwaySent, p.goAwayLastID = true, lastStreamID
	}
	if p.connWriter != nil {
		return p.connWriter.SendGoAway(lastStreamID, code, debugData)
	}
	if err := p.writer.WriteGoAway(lastStreamID, code, debugData); err != nil {
		return err
	}
	p.flush()
	return nil
}

// sendRSTStreamAndMarkClosed sends RST_STREAM and marks the stream as closed.
func (p *Processor) sendRSTStreamAndMarkClosed(streamID uint32, code http2.ErrCode) error {
	stream, ok := p.manager.GetStream(streamID)
	if ok {
		stream.Cancel()
	}

	if p.connWriter != nil {
		p.connWriter.MarkStreamClosed(streamID)
	}

	if p.connWriter != nil {
		if err := p.connWriter.WriteRSTStreamPriority(streamID, code); err != nil {
			return err
		}
	} else {
		if err := p.writer.WriteRSTStream(streamID, code); err != nil {
			return err
		}
		p.flush()
	}

	p.manager.DeleteStream(streamID)
	return nil
}

// GetStreamPriority returns the priority score for a stream.
func (p *Processor) GetStreamPriority(streamID uint32) int {
	return p.manager.priorityTree.CalculateStreamPriority(streamID)
}

// HandleRawWindowUpdate processes a WINDOW_UPDATE frame directly from raw bytes.
// Payload must be exactly 4 bytes per RFC 7540 §6.9.
func (p *Processor) HandleRawWindowUpdate(streamID uint32, payload []byte) error {
	if len(payload) != 4 {
		if streamID == 0 {
			return p.GoAwayErr(0, http2.ErrCodeFrameSize, []byte("WINDOW_UPDATE wrong size"),
				fmt.Errorf("WINDOW_UPDATE frame size %d, want 4", len(payload)))
		}
		_ = p.sendRSTStreamAndMarkClosed(streamID, http2.ErrCodeFrameSize)
		return fmt.Errorf("WINDOW_UPDATE frame size %d, want 4", len(payload))
	}

	increment := (uint32(payload[0])<<24 | uint32(payload[1])<<16 | uint32(payload[2])<<8 | uint32(payload[3])) & 0x7fffffff

	if increment == 0 {
		if streamID == 0 {
			return p.GoAwayErr(0, http2.ErrCodeProtocol, []byte("WINDOW_UPDATE increment is 0"),
				fmt.Errorf("WINDOW_UPDATE with 0 increment"))
		}
		_ = p.sendRSTStreamAndMarkClosed(streamID, http2.ErrCodeProtocol)
		return fmt.Errorf("WINDOW_UPDATE with 0 increment on stream %d", streamID)
	}

	if streamID == 0 {
		for {
			oldWin := atomic.LoadInt32(&p.manager.connectionWindow)
			newWin := int64(oldWin) + int64(increment)
			if newWin > 0x7fffffff {
				return p.GoAwayErr(0, http2.ErrCodeFlowControl, []byte("connection window overflow"),
					fmt.Errorf("connection window overflow: %d + %d > 2^31-1", oldWin, increment))
			}
			//nolint:gosec // G115: safe conversion, newWin validated above
			if atomic.CompareAndSwapInt32(&p.manager.connectionWindow, oldWin, int32(newWin)) {
				break
			}
		}
		p.flushConnWindowStalledStreams()
		p.manager.notifySendWindow() // celeris#893
		return nil
	}

	stream, ok := p.manager.GetStream(streamID)
	if !ok {
		if streamID <= p.manager.GetLastClientStreamID() {
			return nil
		}
		return p.GoAwayErr(p.manager.GetLastStreamID(), http2.ErrCodeProtocol,
			[]byte("WINDOW_UPDATE on idle stream"),
			fmt.Errorf("WINDOW_UPDATE on idle stream %d", streamID))
	}

	streamState := stream.GetState()
	if streamState == StateClosed {
		return nil
	}

	for {
		old := stream.LoadWindowSize()
		newWindow := int64(old) + int64(increment)
		if newWindow > 0x7fffffff {
			// RST and reclaim the slot via the shared close path so the
			// active stream's MAX_CONCURRENT_STREAMS slot is freed.
			_ = p.sendRSTStreamAndMarkClosed(streamID, http2.ErrCodeFlowControl)
			return fmt.Errorf("stream %d window overflow: %d + %d > 2^31-1", streamID, old, increment)
		}
		//nolint:gosec // G115: safe conversion
		if stream.CompareAndSwapWindowSize(old, int32(newWindow)) {
			break
		}
	}

	// A pool handler waiting for this stream's window (celeris#893).
	p.manager.notifySendWindow()

	// The channel is read before the flush: a flush that finishes the stream
	// deletes it, which returns it to the stream pool, and the pooled object
	// is any connection's next stream (celeris#951).
	windowUpd := stream.ReceivedWindowUpd

	// Flush buffered outbound data now that per-stream window space is
	// available (clamped + debited against the connection window too).
	if p.flushStreamOutbound(stream) {
		switch streamState {
		case StateHalfClosedRemote:
			stream.SetState(StateClosed)
		case StateOpen:
			stream.SetState(StateHalfClosedLocal)
		}
		p.manager.DeleteStream(streamID)
	}

	if windowUpd != nil {
		select {
		//nolint:gosec // G115: safe conversion
		case windowUpd <- int32(increment):
		default:
		}
	}

	return nil
}

// HandleRawPing processes a PING frame directly from raw bytes.
// Payload must be exactly 8 bytes per RFC 7540 §6.7.
func (p *Processor) HandleRawPing(flags byte, payload []byte) error {
	if len(payload) != 8 {
		return p.GoAwayErr(0, http2.ErrCodeFrameSize, []byte("PING wrong size"),
			fmt.Errorf("PING frame size %d, want 8", len(payload)))
	}

	// ACK flag = 0x01
	if flags&0x01 != 0 {
		return nil // ACK, ignore
	}

	var data [8]byte
	copy(data[:], payload)
	if err := p.writer.WritePing(true, data); err != nil {
		return err
	}
	p.flush()
	return nil
}

// InjectStreamHeaders opens a new stream with the given ID, populates it
// with the provided headers + body (no HPACK round-trip — headers are
// already decoded), and dispatches the handler. Used by the h2c upgrade
// path (RFC 7540 §3.2) to replay the original H1 request as stream 1 on
// the newly-promoted H2 connection.
//
// Invariants:
//   - streamID must not already exist in the manager.
//   - headers must include all H2 pseudo-headers (:method, :path, :scheme,
//     :authority); the caller synthesizes these from the H1 request line.
//   - If endStream=true, body must be the complete request body and the
//     stream transitions to HalfClosedRemote immediately.
func (p *Processor) InjectStreamHeaders(streamID uint32, endStream bool, headers [][2]string, body []byte) error {
	s, ok := p.manager.TryOpenStream(streamID)
	if !ok {
		return fmt.Errorf("injectStreamHeaders: could not open stream %d", streamID)
	}
	if s.ResponseWriter == nil {
		s.ResponseWriter = p.connWriter
	}

	// Detect HEAD (mirrors ProcessRawHeaders behavior).
	for _, h := range headers {
		if h[0] == ":method" && h[1] == "HEAD" {
			s.IsHEAD = true
			break
		}
	}
	s.AddHeadersBatch(headers)
	s.ReceivedInitialHeaders = true

	if len(body) > 0 {
		_, _ = s.GetBuf().Write(body) // bytes.Buffer.Write never returns error
		s.ReceivedDataLen = len(body)
	}
	if endStream {
		s.EndStream = true
		s.SetState(StateHalfClosedRemote)
	}

	// lastClientStream must reflect the injected stream so subsequent
	// HEADERS validate correctly (odd, strictly increasing).
	if streamID%2 == 1 && streamID > p.manager.lastClientStream.Load() {
		p.manager.lastClientStream.Store(streamID)
	}

	p.runHandler(s)
	return nil
}
