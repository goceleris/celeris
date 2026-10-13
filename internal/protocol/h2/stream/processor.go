package stream

import (
	"context"
	"fmt"
	"runtime"
	"runtime/debug"
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
	// continuations counts the CONTINUATION frames of the block so far, empty
	// ones included (maxContinuationFrames).
	continuations int
	// priority is the PRIORITY field of the HEADERS frame that started the
	// block, when hasPriority says it had one; it applies when the block is
	// admitted. It is held by value: a pointer here was one heap allocation
	// per split block that carried the flag (celeris#889).
	priority    headerPriority
	hasPriority bool
}

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
//
// A handler that calls runtime.Goexit ends the goroutine that runs it, and
// recover() cannot stop that, so the worker would be gone: it would not credit
// idle again, and the process-wide pool would shrink by one for every such
// handler, until every stream ran on a goroutine of its own (celeris#889). The
// deferred respawn replaces it. The credit stays right: Submit spent the dead
// worker's credit when it queued the task, and the new worker adds its own
// before it parks, as a worker that had returned would.
func (p *h2WorkerPool) run() {
	closed := false
	defer func() {
		if !closed {
			go p.run()
		}
	}()
	for {
		// Credit the pool before parking. Submit spends exactly one
		// credit per queued task, so the counter stays equal to
		// parked-workers minus queued-tasks no matter which worker
		// ends up taking which task.
		p.idle.Add(1)
		task, ok := <-p.work
		if !ok {
			p.idle.Add(-1)
			closed = true
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
	headerListMax        int          // the most a header block may decode to, as net/http counts it (headerFieldOverhead per field); maxHeaderListSize
	headerListSize       int          // what the block being decoded has decoded to so far
	headerListTooLarge   bool         // the block being decoded is over headerListMax: hpackEmit drops the rest
	continuationState    *ContinuationState
	continuationStateMu  sync.Mutex
	continuationActive   atomic.Bool
	InlineCachedCtx      any             // per-connection cached app context for inline handlers (avoids sync.Pool)
	hasMoreFrames        bool            // true when more frames follow in current recv (defers inline cleanup)
	pendingInlineCleanup []inlineCleanup // inline-completed streams deferred until frame loop exits
	InlineWriter         h2Conn          // direct-to-outBuf writer for inline handlers (set by conn layer)
	InlineCount          uint64          // number of requests handled inline (for metrics)
	MaxRequestBodySize   int64           // 0 = unlimited; resource.Config.WithDefaults resolves the default

	// OnHandlerPanic, when set, is told of a panic that escaped a stream's
	// handler and reached the last-resort recover of executeHandlerInline or
	// executeHandler (the celeris router answers a handler's panic with a 500
	// first, so only a panic out of the router itself, a response writer or
	// middleware outside it gets here): the stream's id, the panic value and
	// the stack at the recover. It runs on the goroutine that ran the handler
	// (the event loop, for an inline one), first thing in the handler's
	// deferred cleanup and before the stream is retired, so it must not block.
	// A panic in the hook itself is recovered and dropped. Set it before the
	// first frame.
	OnHandlerPanic func(streamID uint32, r any, stack []byte)
	handlerPanics  atomic.Uint64 // panics that reached the recover, hook or not

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

	// refusedFields is the decode target of a refused stream's block (emit is
	// off, so nothing is appended): a field, so a refusal allocates no slice.
	// Last in the struct: the fast path's fields keep their offsets.
	refusedFields [][2]string

	// Refusals per second, like rstCount: noteRefusal counts the requests the
	// processor refused and ends the connection past a burst no honest client
	// makes. Frame processing is serial, so no atomics. After refusedFields
	// for the same reason.
	refusalWindowSec   int64  // unix-second marker for the current window
	refusedHeaderLists uint32 // header lists over the bound refused in the current second
	refusedStreams     uint32 // streams refused for MAX_CONCURRENT_STREAMS in the current second
	// nowUnix is the clock noteRefusal reads (nil: the wall clock); tests set it.
	nowUnix func() int64

	// resetIDs remembers which stream identifiers this server reset
	// (markServerReset, serverReset): the DATA of a request that was already on
	// the wire is ignored (handleData), where DATA on any other closed stream
	// is still a connection error. After refusedFields for the same reason.
	resetIDs serverResets

	// OnHandlerGoexit, when set, is told that a stream's handler ended its
	// goroutine with runtime.Goexit (celeris#889): the stream's id and the
	// stack at the deferred cleanup, which shows the Goexit and the frames it
	// unwound. A Goexit cannot be recovered, so on the event loop it still ends
	// the loop (celeris#848); on the worker pool the worker is replaced. Like
	// OnHandlerPanic it runs first thing in the handler's deferred cleanup, on
	// the goroutine that ran the handler, and must not block. Set it before the
	// first frame.
	OnHandlerGoexit func(streamID uint32, stack []byte)
	// OnHandlerUnfinished, when set, is told that a handler is over without
	// having finished its response (celeris#889): the stream's id, whether the
	// processor reset the stream (true: RST_STREAM) or ended it (false:
	// END_STREAM, for a complete response that was not closed), the body bytes
	// the handler wrote, the content-length it declared (-1: none; DeclaredInvalid: one that
	// cannot be held to, such as "+5" or "5, 6") and the
	// error it returned, if any. It runs on the goroutine that ran the
	// handler, after the stream was ended or reset, and must not block. Set it
	// before the first frame.
	OnHandlerUnfinished func(streamID uint32, reset bool, written, declared int64, err error)
	handlerGoexits      atomic.Uint64 // Goexits that reached the deferred cleanup, hook or not
	unfinishedHandlers  atomic.Uint64 // handlers that returned without finishing their response, hook or not
}

// HandlerPanics is how many panics have escaped a stream's handler of this
// connection to the last-resort recover (OnHandlerPanic). Safe from any
// goroutine.
func (p *Processor) HandlerPanics() uint64 { return p.handlerPanics.Load() }

// handlerPanicked records a panic that escaped the handler of stream id, and
// tells OnHandlerPanic. It runs inside the deferred recover of the handler's
// goroutine, so the stack still holds the panic's frames.
func (p *Processor) handlerPanicked(id uint32, r any) {
	p.handlerPanics.Add(1)
	hook := p.OnHandlerPanic
	if hook == nil {
		return
	}
	// The hook is somebody else's code, running in a deferred function that
	// nothing above recovers: on the event loop, a panic out of it would end
	// the process.
	defer func() { _ = recover() }()
	hook(id, r, debug.Stack())
}

// HandlerGoexits is how many handlers of this connection ended their
// goroutine with runtime.Goexit (OnHandlerGoexit). Safe from any goroutine.
func (p *Processor) HandlerGoexits() uint64 { return p.handlerGoexits.Load() }

// UnfinishedResponses is how many handlers of this connection returned without
// finishing their response, and had their stream ended or reset for it
// (OnHandlerUnfinished). Safe from any goroutine.
func (p *Processor) UnfinishedResponses() uint64 { return p.unfinishedHandlers.Load() }

// handlerGoexited records that the handler of stream id ended its goroutine
// with runtime.Goexit, and tells OnHandlerGoexit. It runs in the deferred
// cleanup of the handler's goroutine, so the stack still holds the Goexit's
// frames.
func (p *Processor) handlerGoexited(id uint32) {
	p.handlerGoexits.Add(1)
	hook := p.OnHandlerGoexit
	if hook == nil {
		return
	}
	defer func() { _ = recover() }() // as handlerPanicked: somebody else's code
	hook(id, debug.Stack())
}

// handlerUnfinished records that a handler is over without having finished
// its response, and tells OnHandlerUnfinished.
func (p *Processor) handlerUnfinished(id uint32, reset bool, written, declared int64, err error) {
	p.unfinishedHandlers.Add(1)
	hook := p.OnHandlerUnfinished
	if hook == nil {
		return
	}
	defer func() { _ = recover() }()
	hook(id, reset, written, declared, err)
}

// resetHandlerStream resets the stream whose handler is over with
// INTERNAL_ERROR (RFC 9113 §7: "the endpoint encountered an unexpected
// internal error"; CANCEL says the stream is no longer needed, which is not
// what a handler that failed to answer means, and x/net's HTTP/2 server resets
// the stream of a handler that panicked or called runtime.Goexit with
// INTERNAL_ERROR too). What the stream still
// buffers is dropped, so a flush sends nothing after the RST_STREAM, and the
// RST_STREAM is queued behind the frames the stream already queued (all of a
// stream's frames go to one shard, in order): sent straight to the connection,
// it could reach the peer before the HEADERS or DATA still in the queue, and
// DATA on a closed stream is an error for the peer. A pool goroutine cannot
// use the connection's writer, which belongs to the event loop, so the
// ResponseWriter does it (Resetter). The request has ended before any
// handler runs (every frame path sets EndStream first), so the peer has no
// DATA in flight for the stream and nothing is marked in resetIDs.
//
// Lock order: DiscardOutbound takes the stream's lock alone and releases it
// before the reset is queued (shard lock, then the wakeup), as a StreamWriter's
// write does in the other direction (stream lock, then shard lock); nothing is
// held across a hook.
func (p *Processor) resetHandlerStream(stream *Stream) {
	p.manager.DiscardOutbound(stream)
	switch w := stream.ResponseWriter.(type) {
	case Resetter:
		w.ResetStream(stream, http2.ErrCodeInternal) // cancels the stream as well
	default:
		// A writer that cannot queue behind the stream's frames (a test
		// recorder): the connection's own, which is the event loop's, so only
		// for a stream whose handler ran there. A pool stream is cancelled
		// and nothing is written: the connection's writer is not safe from a
		// pool goroutine.
		if p.connWriter != nil && !stream.RunsOnPool() {
			_ = p.connWriter.WriteRSTStreamPriority(stream.ID, http2.ErrCodeInternal)
		}
		stream.Cancel()
	}
}

// settleStreamed runs when a handler that streamed its response returned
// without closing the writer. Go's net/http ends a response when its handler
// returns, so a response that is complete is ended with END_STREAM: the headers
// are out and, for a declared content-length, exactly that many bytes were
// written (a response with none declared is complete whatever it wrote). A body
// that is not as long as declared is reset instead, never ended: END_STREAM
// would make a truncated body look complete to the client (celeris#965). The
// same goes for a body longer than declared, and for a declaration that cannot
// be held to (a content-length that is not a plain number: DeclaredInvalid).
// Either way the peer is no longer left waiting for its own timeout. The caller has ended the
// stream's use (EndUse), so the writer's own calls are refused from here on;
// Streamer.Close takes the use token afresh.
func (p *Processor) settleStreamed(stream *Stream) {
	o, written, declared := stream.responseOutcome()
	if o != outcomeComplete && o != outcomeShort || stream.IsCancelled() {
		return // not a streamed response, or already ended, or the peer reset it
	}
	if o == outcomeComplete {
		st, ok := stream.ResponseWriter.(Streamer)
		if ok {
			err := st.Close(stream)
			if err == nil {
				p.handlerUnfinished(stream.ID, false, written, declared, nil)
				return
			}
			if stream.IsCancelled() {
				return // the peer reset the stream while this ran
			}
		}
	}
	p.resetHandlerStream(stream)
	p.handlerUnfinished(stream.ID, true, written, declared, nil)
}

// settleFailed runs when a handler is over without having answered: it
// returned an error (celeris#889), panicked, or called runtime.Goexit. Unless
// its response is whole (written whole, or its END_STREAM is sent or queued
// behind buffered DATA, which the caller then keeps the stream for), the
// stream is reset: the peer is told, where it used to wait for its own
// timeout. handled says the panic or Goexit hook has already told of it, so
// OnHandlerUnfinished does not count it again.
func (p *Processor) settleFailed(stream *Stream, err error, handled bool) {
	if stream.IsCancelled() {
		return
	}
	o, written, declared := stream.responseOutcome()
	if o == outcomeSettled {
		return
	}
	p.resetHandlerStream(stream)
	if !handled {
		p.handlerUnfinished(stream.ID, true, written, declared, err)
	}
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

// headerListRefusalBurstMax and streamRefusalBurstMax bound the requests the
// processor refuses per second on one connection (noteRefusal), the way
// rstBurstMax bounds the resets. A refusal costs the peer a few bytes (13 for
// a stream refused for the limit) and the server the decode of the whole
// block, with emit off, and an RST_STREAM.
//
// A header list over the bound (maxHeaderListSize) is no request an honest
// client sends, let alone repeatedly, and each such block builds about 5 times
// its own size in strings for the dynamic table: the burst is small.
//
// A stream refused for SETTINGS_MAX_CONCURRENT_STREAMS can be honest, in a
// burst: "Initially, there is no limit to this value" (RFC 9113 section
// 6.5.2), so a client that has not read the server's SETTINGS yet may open
// every stream it has queued, and each above the limit is refused. 2000 is a
// burst of that kind, twice the 1000 streams of a client that has that many
// requests ready. A client that has read SETTINGS keeps within the limit and
// is refused only by a race.
const (
	headerListRefusalBurstMax = 100
	streamRefusalBurstMax     = 2000
)

// serverResetWindow is how many of the most recent odd stream identifiers
// serverResets remembers: 1024, that is the last 2048 stream identifiers. A
// client that opens 1000 streams at once against a limit of 100 has 900 of
// them refused, and their DATA frames can trail their HEADERS frames by as
// many streams.
const serverResetWindow = 1024

// serverResets is a bitset over a sliding window of odd stream identifiers:
// the ones this server sent a RST_STREAM for. An identifier older than the
// window is forgotten, which makes its DATA a connection error, as it was
// before the set existed. It is 128 bytes, allocates nothing, and is marked on
// the paths that reset a stream (sendRSTStreamAndMarkClosed) in a few
// instructions. The set is the only thing that tells a stream the server
// reset (RFC 9113 section 5.1: the server must be prepared for the frames
// already on the wire) from one that is closed because the peer reset it or
// the exchange ended (any frame but PRIORITY on those is an error).
type serverResets struct {
	top  uint32 // one past the highest index marked (index = id>>1); 0: nothing marked
	bits [serverResetWindow / 64]uint64
}

// mark records that the server reset stream id. Even identifiers and
// identifiers older than the window are not recorded.
func (r *serverResets) mark(id uint32) {
	if id&1 == 0 {
		return
	}
	idx := id >> 1
	if idx >= r.top {
		// Slide the window up to idx: the positions it takes over from the
		// identifiers it drops are cleared.
		if idx-r.top >= serverResetWindow || r.top == 0 {
			r.bits = [serverResetWindow / 64]uint64{}
		} else {
			for k := r.top; k <= idx; k++ {
				r.bits[(k%serverResetWindow)/64] &^= 1 << (k % 64)
			}
		}
		r.top = idx + 1
	} else if idx+serverResetWindow < r.top {
		return // older than the window
	}
	r.bits[(idx%serverResetWindow)/64] |= 1 << (idx % 64)
}

// has reports whether stream id is one mark recorded and the window still holds.
func (r *serverResets) has(id uint32) bool {
	if id&1 == 0 {
		return false
	}
	idx := id >> 1
	if idx >= r.top || idx+serverResetWindow < r.top {
		return false
	}
	return r.bits[(idx%serverResetWindow)/64]&(1<<(idx%64)) != 0
}

// maxContinuationFrames is the most CONTINUATION frames one header block may
// have, empty ones included. The byte cap on a block (headerBlockExpansion)
// does not bound them: a frame with an empty fragment adds nothing to it.
// A block of the cap's size (256 KiB) takes 16 frames of the default
// SETTINGS_MAX_FRAME_SIZE (16 KiB); 32 leaves room for a peer that writes
// frames half that size.
const maxContinuationFrames = 32

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

// maxHeaderListSize is the most an HTTP/2 request's header list (or its
// trailer list) may decode to: the sum, over every field, of len(name) +
// len(value) + headerFieldOverhead, as RFC 9113 §6.5.2 defines
// SETTINGS_MAX_HEADER_LIST_SIZE and net/http counts it. 64 KiB is the native
// HTTP/1 limit (h1.MaxHeaderSize). The server tells the peer: its SETTINGS
// carry it (MaxHeaderListSize).
//
// The HPACK decoder bounds one string only (SetMaxStringLength): a
// one-byte reference to a 4 KB dynamic-table entry costs the peer one byte and
// the server a field, so without a bound on the list a 16 KB header block was
// a list of 48 MB, and with the cookie fields joined one string of 48 MB
// (celeris#944).
const maxHeaderListSize = MaxHeaderListSize

// MaxHeaderListSize is the SETTINGS_MAX_HEADER_LIST_SIZE the native HTTP/2
// connection advertises: the bound it holds a request's (or trailer's) header
// list to. The conn layer writes it in the server's SETTINGS.
const MaxHeaderListSize = 64 << 10

// headerFieldOverhead is the 32 bytes RFC 7541 §4.1 and RFC 9113 §6.5.2 add
// to each field's size.
const headerFieldOverhead = 32

// headerBlockExpansion is how many times longer, on the wire, the HPACK block
// of a header list within the bound can be than the bound itself. It sets the
// cap on a block still being assembled from CONTINUATION frames
// (handleContinuation): the bound counts decoded bytes (hpackEmit), a block
// is wire bytes, and a field can take more of those than the bound charges it.
//
// An encoder may Huffman-code a string even when the code is longer than the
// string (RFC 7541 §5.2): up to 30 bits for a byte (19 for a printable one,
// the backslash), that is 3.75 wire bytes for a byte. A string length is an
// integer of up to 4 bytes, a literal field carries two of them and an
// opcode (9 bytes), and each string rounds up to a whole byte. So a field of
// n name and v value bytes is at most 3.75(n+v)+11 bytes on the wire, and the
// bound charges it n+v+32: four times the charge, 4(n+v)+128, is never short
// of the encoding, with 0.25(n+v)+117 bytes to spare for every field
// (TestFieldEncodingNeverExceedsTheBlockCap944 checks it over the size
// classes). A list within the bound is therefore a block within the cap.
//
// What is left over is the table-size updates of RFC 7541 §6.3, which are no
// field: an encoder sends at most two (about 10 bytes, inside the spare of
// any one field), and only a block padded with many of them, which the
// decoder accepts while its table is empty, can pass the cap with a list
// that is within the bound; that is not a header list.
//
// It is a worst case, not an exact count: a block between the bound and the
// cap is assembled, decoded, and refused by the exact bound (RST_STREAM on
// its stream), never by this cap (GOAWAY, which ends the connection). The
// server keeps at most this many bytes of a block per connection: the first
// fragment is held to the cap as the later ones are (handleHeaders,
// appendContinuation), and a block that is complete in one HEADERS frame is
// decoded, not kept.
const headerBlockExpansion = 4

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

		headerListMax: maxHeaderListSize,
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
		// One string is at most 64 KiB. That bounds a string only, not
		// the list: hpackEmit bounds the list (maxHeaderListSize),
		// handleContinuation the block still being assembled.
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
// make joining quadratic (celeris#944). The header list is bounded
// (maxHeaderListSize) before a field is joined, so the buffer is too.
func (p *Processor) hpackEmit(hf hpack.HeaderField) {
	// Every field is charged before anything is kept of it, cookie fields
	// that are joined included: the joined value is never longer than the
	// fields' own sizes. Past the bound the decoder is told to stop emitting
	// (it finishes the block without building the strings, which keeps its
	// dynamic table in step with the peer's encoder, as net/http's does) and
	// the block is refused when it ends (endHeaderDecode).
	if p.headerListSize += len(hf.Name) + len(hf.Value) + headerFieldOverhead; p.headerListSize > p.headerListMax {
		p.headerListTooLarge = true
		p.hpackDecoder.SetEmitEnabled(false)
		return
	}
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
	p.ensureHPACKDecoder()
	// A block over the bound turned emit off; whatever ended it, this one
	// starts with it on (not only the end of the block that tripped it:
	// a block that fails to decode returns a connection error before that).
	p.hpackDecoder.SetEmitEnabled(true)
	p.hpackTarget = target
	p.headerListSize = 0
	p.headerListTooLarge = false
	p.joinCookies = requestHeaders
	p.cookieAt = -1
	p.cookieJoined = false
}

// endHeaderDecode finishes a header block hpackEmit decoded: if it carried
// more than one cookie field, the first one's value becomes the joined value.
// It reports true when the block decoded to more than the header list bound:
// what hpackEmit kept of it is not a request, and the caller must refuse it
// (refuseHeaderList) and not use the list.
func (p *Processor) endHeaderDecode() (tooLarge bool) {
	if p.headerListTooLarge {
		p.headerListTooLarge = false
		p.cookieJoined = false
		p.cookieAt = -1
		if cap(p.cookieBuf) > 4<<10 {
			p.cookieBuf = nil
		}
		return true
	}
	if p.cookieJoined {
		(*p.hpackTarget)[p.cookieAt][1] = string(p.cookieBuf)
		p.cookieJoined = false
		if cap(p.cookieBuf) > 4<<10 {
			p.cookieBuf = nil // do not pin a large buffer for the connection's life
		}
	}
	p.cookieAt = -1
	return false
}

// refuseHeaderList answers a header block that decoded to more than the
// header list bound: RST_STREAM ENHANCE_YOUR_CALM for its stream (net/http
// answers 431), the stream freed with its MAX_CONCURRENT_STREAMS slot. The
// block was decoded to its end, so the HPACK state is in step with the
// peer's and the connection goes on. The request never reaches a handler.
// A connection that keeps sending such blocks does not go on (noteRefusal).
func (p *Processor) refuseHeaderList(streamID uint32) error {
	if err := p.noteRefusal(false); err != nil {
		return err
	}
	return p.sendRSTStreamAndMarkClosed(streamID, http2.ErrCodeEnhanceYourCalm)
}

// noteRefusal counts a request the processor refused (streamLimit: for
// SETTINGS_MAX_CONCURRENT_STREAMS; otherwise for its header list), in a window
// of one second as handleRSTStream counts resets, and ends the connection
// with GOAWAY(ENHANCE_YOUR_CALM) once the window has seen more than the burst
// (headerListRefusalBurstMax, streamRefusalBurstMax). A refusal is an answer
// the peer asked for with a few bytes, so without a count one connection could
// ask for it for ever. The GOAWAY names the last stream the server opened
// (GetLastStreamID); for a header list over the bound that includes the stream
// being refused, which was opened for the decode, so a client treats that one
// as possibly processed though it was not: the safe side, and only an abusive
// peer sees it. A stream refused for the limit was never opened and is not named.
func (p *Processor) noteRefusal(streamLimit bool) error {
	var now int64
	if p.nowUnix != nil {
		now = p.nowUnix()
	} else {
		now = time.Now().Unix()
	}
	if now != p.refusalWindowSec {
		p.refusalWindowSec = now
		p.refusedHeaderLists, p.refusedStreams = 0, 0
	}
	if streamLimit {
		if p.refusedStreams++; p.refusedStreams > streamRefusalBurstMax {
			return p.GoAwayErr(p.manager.GetLastStreamID(), http2.ErrCodeEnhanceYourCalm, []byte("refused streams flood"),
				fmt.Errorf("streams refused for MAX_CONCURRENT_STREAMS more than %d times in a second", streamRefusalBurstMax))
		}
		return nil
	}
	if p.refusedHeaderLists++; p.refusedHeaderLists > headerListRefusalBurstMax {
		return p.GoAwayErr(p.manager.GetLastStreamID(), http2.ErrCodeEnhanceYourCalm, []byte("refused header lists flood"),
			fmt.Errorf("header lists over %d bytes refused more than %d times in a second", p.headerListMax, headerListRefusalBurstMax))
	}
	return nil
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
				return p.GoAwayErr(p.manager.GetLastStreamID(), http2.ErrCodeProtocol, []byte("non-CONTINUATION on stream expecting CONTINUATION"),
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
				return p.GoAwayErr(p.manager.GetLastStreamID(), http2.ErrCodeFrameSize, []byte("DATA frame too large"),
					fmt.Errorf("DATA frame exceeds MAX_FRAME_SIZE: %d > %d", header.Length, localMaxFrame))
			}
			_ = p.writer.WriteRSTStream(header.StreamID, http2.ErrCodeFrameSize)
			p.flush()
			return fmt.Errorf("DATA frame exceeds MAX_FRAME_SIZE: %d > %d", header.Length, localMaxFrame)
		}
	case *http2.HeadersFrame:
		if header.Length > localMaxFrame {
			return p.GoAwayErr(p.manager.GetLastStreamID(), http2.ErrCodeFrameSize, []byte("HEADERS frame too large"),
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
		return p.GoAwayErr(p.manager.GetLastStreamID(), http2.ErrCodeProtocol, []byte("client sent PUSH_PROMISE"),
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
// loop. Eligible: the request has ended (EndStream, which every frame path sets
// before runHandler, whatever the request carried: a POST with a body is as
// eligible as a GET), no CONTINUATION pending, and a route that is not async.
// The connection's outbound budget is checked in runHandler, not here.
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

// runHandler dispatches the stream handler. A stream whose request has ended
// (canRunInline) and whose connection is within the outbound budget runs
// synchronously on the event loop; any other goes to the worker pool for
// concurrent execution.
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
	// returned is set when HandleStream has returned. A deferred function
	// cannot tell a handler that called runtime.Goexit from one that returned:
	// recover() is nil for both (celeris#889).
	returned := false

	defer func() {
		if r := recover(); r != nil {
			p.handlerPanicked(stream.ID, r)
			stream.EndUse()
			p.settleFailed(stream, nil, true)
			keepAlive = stream.AbandonOutbound()
		} else if !returned {
			p.handlerGoexited(stream.ID)
			stream.EndUse()
			p.settleFailed(stream, nil, true)
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
	returned = true
	// The handler is over: on HTTP/2 the stream ends with it, detached or not
	// (Context.Detach). A StreamWriter call from a goroutine it started is
	// refused from here on, before the check for buffered DATA below, so
	// nothing is added to what the stream keeps (celeris#904).
	stream.EndUse()
	if err != nil {
		// Nothing answered, or a response begun and not finished: the stream
		// is reset instead of left to the client's own timeout (celeris#889).
		p.settleFailed(stream, err, false)
		// A response the handler closed while its tail waited for the peer's
		// window is whole: the stream stays for the flush that sends the tail
		// and its END_STREAM. A reset one has nothing buffered any more.
		keepAlive = stream.AbandonOutbound()
		return
	}

	if !stream.GetHeadersSent() {
		if stream.ResponseWriter != nil {
			_ = stream.ResponseWriter.WriteResponse(stream, 200, nil, nil)
		}
		return
	}

	// A StreamWriter response the handler did not close is ended here, as
	// net/http ends a response when its handler returns, or reset when it is
	// short of its declared content-length (celeris#889). It comes before the
	// check for buffered DATA: a complete response's END_STREAM then rides on
	// the last of that DATA (Manager.EndOutbound), and a reset one's buffer is
	// dropped.
	if stream.flags.Load()&flagStreamed != 0 {
		p.settleStreamed(stream)
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

// executeHandler executes the handler on a worker pool goroutine. On
// completion it retires the stream (Manager.retire): it ends its use and takes
// it out of the manager, and the event loop returns it to the pool, never this
// goroutine (celeris#951). If outbound data is buffered (flow control), the
// stream stays in the map for the event loop to flush via WINDOW_UPDATE.
func (p *Processor) executeHandler(stream *Stream) {
	// Deferred first, so it runs last: the response is queued and the
	// stream settled before a shutdown that waits for this count can close
	// the connection (celeris#759).
	defer p.poolRunning.Add(-1)
	returned := false // see executeHandlerInline: a Goexit is invisible to recover()
	defer func() {
		if r := recover(); r != nil { // last-resort panic recovery
			p.handlerPanicked(stream.ID, r)
			stream.EndUse()
			p.settleFailed(stream, nil, true)
		} else if !returned {
			p.handlerGoexited(stream.ID)
			stream.EndUse()
			p.settleFailed(stream, nil, true)
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
	returned = true
	stream.EndUse() // as in executeHandlerInline
	if err != nil {
		p.settleFailed(stream, err, false)
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

	if stream.flags.Load()&flagStreamed != 0 { // as in executeHandlerInline
		p.settleStreamed(stream)
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

// headerPriority is the PRIORITY field of a HEADERS frame.
type headerPriority struct {
	dependency uint32
	weight     uint8
	exclusive  bool
}

// admission is what admitHeaders made of a complete header block.
type admission uint8

const (
	// admitNew: the block is the request headers of a stream that is open.
	admitNew admission = iota
	// admitTrailers: the stream already has its request headers; the block
	// is its trailers.
	admitTrailers
	// admitRefused: the stream was refused (RST_STREAM REFUSED_STREAM) and
	// the block was decoded and dropped; there is nothing more to do.
	admitRefused
)

// admitHeaders is the admission of a complete header block (HEADERS with
// END_HEADERS, or HEADERS and the CONTINUATION frames that end it, whole):
// the one place that decides whether the block opens a stream, is the
// trailers of one, or is refused. Every way a header block reaches the
// processor (ProcessRawHeaders, handleHeaders, handleContinuation) calls it
// before it decodes the block, so none of them can skip a check another
// makes (celeris#981: the CONTINUATION path opened its streams with
// GetOrCreateStream, which checks nothing).
//
// For a stream the manager does not hold (an idle one, which includes a
// placeholder PRIORITY left behind): its identifier must be odd and greater
// than the last client stream (RFC 9113 §5.1.1; a lower or equal one is a
// closed stream's, a connection error), and is then used up, refused or not;
// the stream is opened if the peer's SETTINGS_MAX_CONCURRENT_STREAMS has room
// (TryOpenStream), and refused with RST_STREAM REFUSED_STREAM if it has none
// (§5.1.2; the client may retry it at once, and the connection serves the
// next request). For a stream the manager holds: HEADERS on a closed or
// half-closed (remote) stream is a connection error (STREAM_CLOSED); a second
// header block is the stream's trailers and must end it.
//
// It returns the stream for admitNew and admitTrailers. endStream is the
// END_STREAM flag of the HEADERS frame, block the whole header block (a
// refused one is decoded, to keep the HPACK state in step with the peer's).
// Frame processing is serial (under H2State.mu), so the load and the store of
// the last client stream need no more than the atomics they are.
//
// A connection error here names the last stream the server opened
// (GetLastStreamID), as every other site does, and not the last client stream,
// which a refused stream moves forward: the client got REFUSED_STREAM for that
// one, and it was not processed.
func (p *Processor) admitHeaders(id uint32, endStream bool, block []byte) (*Stream, admission, error) {
	m := p.manager
	existing, exists := m.GetStream(id)
	if exists && existing.GetState() == StateIdle {
		// A PRIORITY frame put it there; the stream is not open, and its
		// identifier is as new as one the manager has never seen.
		exists = false
	}

	if !exists {
		last := m.lastClientStream.Load()

		if id <= last {
			_ = p.SendGoAway(m.GetLastStreamID(), http2.ErrCodeProtocol, []byte("HEADERS on closed stream (reused id)"))
			return nil, admitNew, fmt.Errorf("HEADERS frame on closed stream %d (last stream: %d)", id, last)
		}

		if err := validateStreamID(id, last, false); err != nil {
			return nil, admitNew, p.GoAwayErr(m.GetLastStreamID(), http2.ErrCodeProtocol, []byte(err.Error()), err)
		}

		// Used up now, not when the stream opens: a stream refused below
		// still makes every identifier up to its own a closed one's.
		m.lastClientStream.Store(id)
	} else {
		switch existing.GetState() {
		case StateClosed:
			_ = p.SendGoAway(m.GetLastStreamID(), http2.ErrCodeStreamClosed, []byte("HEADERS on closed stream (state closed)"))
			return nil, admitNew, fmt.Errorf("HEADERS frame on closed stream %d", id)
		case StateHalfClosedRemote:
			_ = p.SendGoAway(m.GetLastStreamID(), http2.ErrCodeStreamClosed, []byte("HEADERS on half-closed stream"))
			return nil, admitNew, fmt.Errorf("HEADERS frame on half-closed (remote) stream %d", id)
		}

		if existing.ReceivedInitialHeaders {
			if !endStream {
				return nil, admitNew, p.GoAwayErr(m.GetLastStreamID(), http2.ErrCodeProtocol, []byte("second HEADERS without END_STREAM"),
					fmt.Errorf("second HEADERS without END_STREAM on stream %d", id))
			}
			return existing, admitTrailers, nil
		}
	}

	stream, ok := m.TryOpenStream(id)
	if !ok {
		return nil, admitRefused, p.refuseStream(id, block)
	}
	if stream.ResponseWriter == nil {
		stream.ResponseWriter = p.connWriter
	}
	return stream, admitNew, nil
}

// refuseStream refuses a stream the peer's SETTINGS_MAX_CONCURRENT_STREAMS
// has no room for: RST_STREAM REFUSED_STREAM (a stream error: the request
// was not processed, the client may retry it, and the connection goes on).
// The header block is decoded to its end and dropped first, as
// refuseHeaderList's is: HPACK state is the connection's, and a block that
// added a field to the dynamic table must add it here too, or the peer's next
// block references an entry this decoder does not have.
func (p *Processor) refuseStream(id uint32, block []byte) error {
	// Counted before the block is decoded: past the burst the connection ends,
	// and the decode would be for nothing (noteRefusal).
	if err := p.noteRefusal(true); err != nil {
		return err
	}
	p.beginHeaderDecode(&p.refusedFields, false)
	p.hpackDecoder.SetEmitEnabled(false) // decode, build nothing
	// A block that does not decode is a connection error (COMPRESSION_ERROR).
	// The GOAWAY names the last stream the server opened, not 0 (RFC 9113
	// §6.8: 0 lets the client replay every request it has in flight), as the
	// over-long header block site does. An idle PRIORITY placeholder is no
	// stream the server opened and does not move that identifier
	// (Manager.CreatePlaceholder, TestDecodeFailureGoAwayIgnoresPriorityPlaceholder889).
	if _, err := p.hpackDecoder.Write(block); err != nil {
		return p.GoAwayErr(p.manager.GetLastStreamID(), http2.ErrCodeCompression, []byte("HPACK decoding failed"),
			fmt.Errorf("failed to decode headers: %w", err))
	}
	if err := p.hpackDecoder.Close(); err != nil {
		return p.GoAwayErr(p.manager.GetLastStreamID(), http2.ErrCodeCompression, []byte("HPACK decoding failed"),
			fmt.Errorf("failed to finalize headers: %w", err))
	}
	p.endHeaderDecode()
	return p.sendRSTStreamAndMarkClosed(id, http2.ErrCodeRefusedStream)
}

// serveHeaderBlock handles a complete header block: admission
// (admitHeaders), the PRIORITY field of a block that opened a stream, then
// the block itself (completeHeaderBlock). prio is nil for a HEADERS frame
// without the PRIORITY flag.
func (p *Processor) serveHeaderBlock(id uint32, endStream bool, prio *headerPriority, block []byte) error {
	stream, adm, err := p.admitHeaders(id, endStream, block)
	if err != nil || adm == admitRefused {
		return err
	}
	if prio != nil && adm == admitNew {
		if prio.dependency == id {
			return p.GoAwayErr(p.manager.GetLastStreamID(), http2.ErrCodeProtocol, []byte("stream depends on itself"),
				fmt.Errorf("stream %d depends on itself", id))
		}
		p.manager.priorityTree.UpdateFromFrame(id, prio.dependency, prio.weight, prio.exclusive)
	}
	return p.completeHeaderBlock(stream, adm, id, endStream, block)
}

// completeHeaderBlock decodes the header block of an admitted stream and
// acts on it: request headers (validated, the handler run when endStream) or
// trailers (validated, the handler run when endStream).
func (p *Processor) completeHeaderBlock(stream *Stream, adm admission, id uint32, endStream bool, block []byte) error {
	pooled := headersSlicePoolIn.Get().(*[][2]string)
	*pooled = (*pooled)[:0]
	defer func() {
		*pooled = (*pooled)[:0]
		headersSlicePoolIn.Put(pooled)
	}()
	trailers := adm == admitTrailers
	// A block that does not decode is a connection error (COMPRESSION_ERROR).
	// The GOAWAY names the last stream the server opened, not 0, as refuseStream's
	// does (RFC 9113 §6.8: 0 lets the client replay every request it has in flight).
	// The stream of this block is open already (admitHeaders), so it is named too:
	// the client takes it as possibly processed, which is the safe side. An idle
	// PRIORITY placeholder is not named (Manager.CreatePlaceholder).
	p.beginHeaderDecode(pooled, !trailers)
	if _, err := p.hpackDecoder.Write(block); err != nil {
		return p.GoAwayErr(p.manager.GetLastStreamID(), http2.ErrCodeCompression, []byte("HPACK decoding failed"),
			fmt.Errorf("failed to decode headers: %w", err))
	}
	if err := p.hpackDecoder.Close(); err != nil {
		return p.GoAwayErr(p.manager.GetLastStreamID(), http2.ErrCodeCompression, []byte("HPACK decoding failed"),
			fmt.Errorf("failed to finalize headers: %w", err))
	}
	if p.endHeaderDecode() {
		return p.refuseHeaderList(id)
	}
	headers := *pooled

	if trailers {
		if err := validateTrailerHeaders(headers); err != nil {
			_ = p.sendRSTStreamAndMarkClosed(id, http2.ErrCodeProtocol)
			return fmt.Errorf("invalid trailers: %w", err)
		}
		stream.AddHeadersBatch(headers)
		if endStream {
			stream.EndStream = true
			stream.SetState(StateHalfClosedRemote)
			p.runHandler(stream)
		}
		return nil
	}

	if err := validateRequestHeaders(headers); err != nil {
		sendStreamError(p.writer, id, http2.ErrCodeProtocol)
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
			sendStreamError(p.writer, id, http2.ErrCodeProtocol)
			return fmt.Errorf("content-length mismatch: %w", err)
		}

		p.runHandler(stream)
	}
	return nil
}

// handleHeaders processes HEADERS frames. A HEADERS frame without END_HEADERS
// starts a header block that handleContinuation completes; the block is
// admitted, as any other, when it is complete (admitHeaders).
func (p *Processor) handleHeaders(_ context.Context, f *http2.HeadersFrame) error {
	dependency, weight, exclusive, hasPriority := ParsePriorityFromHeaders(f)

	if !f.HeadersEnded() {
		headerBlock := f.HeaderBlockFragment()
		// The first fragment is held until the block ends, as the later ones
		// are, so it is held to the same cap (appendContinuation), before it is
		// copied: a frame can be as long as SETTINGS_MAX_FRAME_SIZE.
		if len(headerBlock) > p.headerListMax*headerBlockExpansion {
			return p.headerBlockTooLarge(f.StreamID)
		}
		frag := make([]byte, len(headerBlock))
		copy(frag, headerBlock)
		cs := &ContinuationState{
			streamID:      f.StreamID,
			headerBlock:   frag,
			endStream:     f.StreamEnded(),
			expectingMore: true,
		}
		if hasPriority {
			cs.priority = headerPriority{dependency: dependency, weight: weight, exclusive: exclusive}
			cs.hasPriority = true
		}
		p.continuationStateMu.Lock()
		p.continuationState = cs
		p.continuationActive.Store(true)
		p.continuationStateMu.Unlock()
		return nil
	}

	if hasPriority {
		prio := headerPriority{dependency: dependency, weight: weight, exclusive: exclusive}
		return p.serveHeaderBlock(f.StreamID, f.StreamEnded(), &prio, f.HeaderBlockFragment())
	}
	return p.serveHeaderBlock(f.StreamID, f.StreamEnded(), nil, f.HeaderBlockFragment())
}

// ProcessRawHeaders handles a HEADERS frame from raw bytes, bypassing the
// x/net framer's *HeadersFrame allocation. Only valid for simple HEADERS
// (END_HEADERS set, no PADDED, no PRIORITY, not during CONTINUATION).
// All RFC 7540 validations are preserved: it admits the block as handleHeaders
// and handleContinuation do (admitHeaders).
func (p *Processor) ProcessRawHeaders(streamID uint32, endStream bool, headerBlock []byte) error {
	return p.serveHeaderBlock(streamID, endStream, nil, headerBlock)
}

// handleData processes DATA frames.
func (p *Processor) handleData(_ context.Context, f *http2.DataFrame) error {
	stream, ok := p.manager.GetStream(f.StreamID)
	if !ok {
		// A stream the manager does not hold that the server reset without
		// ending the connection (a request refused for the stream limit, for its
		// header list, or after the GOAWAY the server sent; every other reset
		// returns an error, and the connection ends): RFC 9113 section 5.1
		// (closed) says the endpoint that sent RST_STREAM "MUST be prepared to
		// receive any frames that were sent before it processed the RST_STREAM",
		// and a POST sent without waiting for the answer has its DATA on the
		// wire. The frame is ignored, and its length goes back to the
		// connection's receive window like any DATA's (the peer spent that much
		// of it). The stream's own window is not credited, so a client that
		// honours it stops by itself; the native path enforces no receive
		// window, so one that does not can go on for as long as the identifier
		// stays in the reset set, at the cost of parsing its frames (nothing is
		// buffered). DATA on a stream that is closed for another reason (the
		// peer reset it, or the exchange ended), on one that was never opened
		// (above the last client stream, even, stream 0) is still a connection
		// error: h2spec 5.1 and 6.1 pin that. The last client stream check is
		// defensive: a reset of an identifier above it ends the connection.
		if f.StreamID <= p.manager.GetLastClientStreamID() && p.resetIDs.has(f.StreamID) {
			p.manager.AccumulateWindowUpdate(0, f.Header().Length)
			p.manager.FlushWindowUpdates(p.writer, false)
			return nil
		}
		return p.GoAwayErr(p.manager.GetLastStreamID(), http2.ErrCodeProtocol, []byte("DATA on idle stream"),
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

	// Reject streams that exceed the maximum request body size.
	if limit := p.maxBodySize(); limit > 0 && int64(stream.ReceivedDataLen) > limit {
		_ = p.sendRSTStreamAndMarkClosed(f.StreamID, http2.ErrCodeCancel)
		return fmt.Errorf("request body exceeds %d bytes on stream %d", limit, f.StreamID)
	}

	if err := stream.AddData(f.Data()); err != nil {
		return err
	}

	// The whole payload is flow-controlled, padding included (RFC 9113
	// sections 6.1 and 6.9.1): that much goes back to both windows.
	updateLen := f.Header().Length

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
			return p.GoAwayErr(p.manager.GetLastStreamID(), http2.ErrCodeProtocol, []byte("WINDOW_UPDATE increment is 0"),
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
				return p.GoAwayErr(p.manager.GetLastStreamID(), http2.ErrCodeFlowControl, []byte("connection window overflow"),
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

	// A stream the manager holds, open or a placeholder from an earlier frame,
	// takes the new priority. One it does not hold is a closed stream's when its
	// identifier is one the peer has used (not above the last client stream),
	// and an even one is never opened; for those, and for an idle one past
	// the bound on placeholders (CreatePlaceholder), the frame is ignored:
	// priority signals are advisory (RFC 9113 section 5.3.2), and nothing is
	// created for a stream that will not open, in the manager or the tree.
	if _, exists := p.manager.GetStream(f.StreamID); !exists {
		if f.StreamID <= lastClient || f.StreamID&1 == 0 {
			return nil
		}
		if _, ok := p.manager.CreatePlaceholder(f.StreamID); !ok {
			return nil
		}
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
			p.manager.dropPlaceholderLocked(stream) // before Release clears its flags
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
		return p.GoAwayErr(p.manager.GetLastStreamID(), http2.ErrCodeProtocol, []byte("PING on non-zero stream"),
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

// handleContinuation processes CONTINUATION frames. The fragment is added to
// the block under continuationStateMu; the lock is released before the
// finished block is served (serveHeaderBlock), so a handler that runs inline
// on this goroutine does not run inside the critical section.
func (p *Processor) handleContinuation(_ context.Context, f *http2.ContinuationFrame) error {
	cs, err := p.appendContinuation(f)
	if err != nil || cs == nil {
		return err
	}
	// The block is complete: serveHeaderBlock admits it as it admits a HEADERS
	// frame with END_HEADERS, the stream-ID order, the
	// MAX_CONCURRENT_STREAMS limit and the stream's state included. The
	// stream is not created here: GetOrCreateStream checks none of them
	// (celeris#981). cs is detached: continuationActive is already false, so
	// canRunInline may pick the event loop, as it does for a plain HEADERS.
	// The field is passed by address into cs, which was heap allocated when the
	// block began and is nobody else's now: no copy, no allocation.
	var prio *headerPriority
	if cs.hasPriority {
		prio = &cs.priority
	}
	return p.serveHeaderBlock(f.StreamID, cs.endStream, prio, cs.headerBlock)
}

// appendContinuation adds a CONTINUATION fragment to the pending header
// block. It returns the block's state, detached from the processor, when the
// fragment ended the block (END_HEADERS), nil while more is expected.
func (p *Processor) appendContinuation(f *http2.ContinuationFrame) (*ContinuationState, error) {
	p.continuationStateMu.Lock()
	defer p.continuationStateMu.Unlock()

	if p.continuationState == nil || !p.continuationState.expectingMore {
		return nil, p.GoAwayErr(p.manager.GetLastStreamID(), http2.ErrCodeProtocol, []byte("unexpected CONTINUATION"),
			fmt.Errorf("unexpected CONTINUATION frame on stream %d", f.StreamID))
	}

	if p.continuationState.streamID != f.StreamID {
		return nil, p.GoAwayErr(p.manager.GetLastStreamID(), http2.ErrCodeProtocol, []byte("CONTINUATION on wrong stream"),
			fmt.Errorf("CONTINUATION frame on wrong stream: expected %d, got %d",
				p.continuationState.streamID, f.StreamID))
	}

	// A header block longer than any encoding of a list within the bound is
	// refused as soon as it is that long (headerBlockExpansion: the wire
	// bytes of a field can exceed what the bound charges for it, so the cap
	// is a multiple of the bound, not the bound), and a peer that never ends
	// it does not make the server keep the pieces after that (CONTINUATION
	// flood). A block shorter than the cap but over the bound is refused
	// when it ends, by the exact bound, as a stream error.
	if len(p.continuationState.headerBlock)+len(f.HeaderBlockFragment()) > p.headerListMax*headerBlockExpansion {
		p.continuationState = nil
		p.continuationActive.Store(false)
		return nil, p.headerBlockTooLarge(f.StreamID)
	}

	// The frames of a block are counted as well as its bytes: an empty
	// fragment adds none (maxContinuationFrames).
	if p.continuationState.continuations++; p.continuationState.continuations > maxContinuationFrames {
		p.continuationState = nil
		p.continuationActive.Store(false)
		return nil, p.GoAwayErr(p.manager.GetLastStreamID(), http2.ErrCodeEnhanceYourCalm, []byte("too many CONTINUATION frames"),
			fmt.Errorf("header block on stream %d has more than %d CONTINUATION frames", f.StreamID, maxContinuationFrames))
	}

	p.continuationState.headerBlock = append(
		p.continuationState.headerBlock,
		f.HeaderBlockFragment()...,
	)

	if !f.HeadersEnded() {
		return nil, nil
	}
	cs := p.continuationState
	p.continuationState = nil
	p.continuationActive.Store(false)
	return cs, nil
}

// headerBlockTooLarge is the connection error for a header block, assembled
// from its HEADERS and CONTINUATION fragments, that is longer than any
// encoding of a list within the bound (headerBlockExpansion). It names the
// last stream the server opened, not 0: 0 tells the peer that no stream was
// processed, and a client retries the ones it has in flight, a POST with a
// replayable body among them (RFC 9113 section 6.8).
func (p *Processor) headerBlockTooLarge(streamID uint32) error {
	return p.GoAwayErr(p.manager.GetLastStreamID(), http2.ErrCodeEnhanceYourCalm, []byte("header block too large"),
		fmt.Errorf("header block on stream %d exceeds %d bytes", streamID, p.headerListMax*headerBlockExpansion))
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
// lastStreamID is served from then on (runHandler). A GOAWAY never names a
// higher last stream than one sent before (RFC 9113 §6.8: "endpoints MUST
// NOT increase the value they send in the last stream identifier"): a caller
// that names the highest stream the manager opened after a graceful GOAWAY
// named a lower one gets the lower one.
func (p *Processor) SendGoAway(lastStreamID uint32, code http2.ErrCode, debugData []byte) error {
	if p.goAwaySent && lastStreamID > p.goAwayLastID {
		lastStreamID = p.goAwayLastID
	}
	p.goAwaySent, p.goAwayLastID = true, lastStreamID
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
	p.resetIDs.mark(streamID) // the DATA already on the wire for it is ignored (handleData)
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
			return p.GoAwayErr(p.manager.GetLastStreamID(), http2.ErrCodeFrameSize, []byte("WINDOW_UPDATE wrong size"),
				fmt.Errorf("WINDOW_UPDATE frame size %d, want 4", len(payload)))
		}
		_ = p.sendRSTStreamAndMarkClosed(streamID, http2.ErrCodeFrameSize)
		return fmt.Errorf("WINDOW_UPDATE frame size %d, want 4", len(payload))
	}

	increment := (uint32(payload[0])<<24 | uint32(payload[1])<<16 | uint32(payload[2])<<8 | uint32(payload[3])) & 0x7fffffff

	if increment == 0 {
		if streamID == 0 {
			return p.GoAwayErr(p.manager.GetLastStreamID(), http2.ErrCodeProtocol, []byte("WINDOW_UPDATE increment is 0"),
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
				return p.GoAwayErr(p.manager.GetLastStreamID(), http2.ErrCodeFlowControl, []byte("connection window overflow"),
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
		return p.GoAwayErr(p.manager.GetLastStreamID(), http2.ErrCodeFrameSize, []byte("PING wrong size"),
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
