//go:build linux

// Package epoll implements the epoll-based I/O engine for Linux.
package epoll

import (
	"context"
	"math"
	"sync"
	"sync/atomic"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/engine"
)

// maxPendingBytes is the per-connection back-pressure limit for pending
// writes on H1 connections. Intentionally small (4 MiB) so a stalled
// peer cannot fill server memory with un-ACKed responses. It is held per
// request, not per write (celeris#761): a request that finds more than this
// unsent is not served and the conn is closed (conn.H1State.WriteBacklogged),
// while a response, however large, is staged whole.
//
// maxPendingBytesDetached is the per-connection limit once the
// connection is detached (WebSocket / SSE). Detached middleware owns
// its own flow control — ReadLimit + backpressure — and may legitimately
// echo payloads larger than 4 MiB (RFC 6455 allows frames up to 2^63,
// Autobahn 9.1.6 sends 16 MiB). 64 MiB matches the WS default ReadLimit.
const (
	maxPendingBytes         = 4 << 20  // 4 MiB (H1)
	maxPendingBytesDetached = 64 << 20 // 64 MiB (WS/SSE)
	// maxPendingBytesH2 is the limit for an HTTP/2 connection. Its DATA is
	// already bounded by the flow-control windows the peer grants (what the
	// windows refuse waits in the streams' buffers, bounded per connection by
	// stream.OutboundBudget, celeris#893; a StreamWriter on the worker pool
	// waits for the window instead of queueing past it, and one on the event
	// loop buffers what the window refuses on its stream, charged to that
	// budget but not refused by it, celeris#904), and a
	// peer that reads keeps up to a window of frames queued behind the
	// socket as a matter of course (net/http's client grants 4 MiB per
	// stream, browsers more per connection), so the H1 limit refused, and
	// closed, healthy HTTP/2 connections (celeris#761). 64 MiB, as for a
	// detached connection, still bounds a peer that grants large windows
	// and stops reading.
	maxPendingBytesH2 = 64 << 20
	// maxPendingInputBytes caps the async dispatch input buffer
	// (cs.asyncInBuf) so a client pipelining requests faster than
	// the dispatch goroutine drains them cannot balloon per-conn
	// memory. Same ceiling as the output side (4 MiB) — a full
	// saturated buffer pair is 8 MiB per conn.
	maxPendingInputBytes = 4 << 20
	// pooledBufCap is the largest backing-array capacity a per-conn
	// buffer may retain when its connState returns to connStatePool.
	// asyncInBuf / asyncOutBuf / writeBuf all grow via append to hold a
	// burst (up to writeCap = 4–64 MiB); a single multi-MB request would
	// otherwise pin that oversized array in the pool forever, inflating
	// pooled connState memory (the epoll-h1-async peak-RSS outlier). On
	// release a buffer whose cap exceeds this bound is dropped (set nil)
	// so the GC reclaims it; acquireConnState / append lazily re-grow a
	// fresh small backing array. Buffers at or below the bound keep their
	// capacity, so the zero-alloc steady state (small responses, sub-64 K
	// pipelines) is unchanged. cs.buf is intentionally excluded: it is
	// fixed at resource.BufferSize and never grows past it.
	pooledBufCap = 64 << 10
)

// trimPooledBuf returns b reset to zero length if its capacity is within the
// pooled bound, or nil if it grew past pooledBufCap. Dropping the oversized
// backing array on release keeps a connState that handled one large burst
// from permanently inflating connStatePool; the common small-buffer path
// keeps its capacity and stays allocation-free.
func trimPooledBuf(b []byte) []byte {
	if cap(b) > pooledBufCap {
		return nil
	}
	return b[:0]
}

// writeCap returns the per-write back-pressure limit for cs: a write is
// refused when the bytes still queued before it (cs.pendingBytes) are over
// it. An HTTP/2 conn has maxPendingBytesH2 and a truly-detached one (WS/SSE)
// maxPendingBytesDetached. An HTTP/1 conn has none: its writes are its
// handlers' responses, and a limit per write cut a response off mid-body, a
// large body or the chunks of a StreamWriter (celeris#761). Its limit,
// maxPendingBytes, is held per request instead (overBacklogH1), before the
// handler runs. Async-mode HTTP1 conns set detachMu up front without being
// truly detached; they are HTTP/1 conns here.
func (cs *connState) writeCap() int {
	if cs.h2State != nil {
		return maxPendingBytesH2
	}
	if cs.detachMu != nil && cs.h1State != nil && cs.h1State.Detached.Load() {
		return maxPendingBytesDetached
	}
	return math.MaxInt
}

// overBacklogH1 is the HTTP/1 back-pressure limit (conn.H1State.WriteBacklogged,
// celeris#761): whether the responses cs still has unsent, which the write
// hooks count in pendingBytes, are over maxPendingBytes, i.e. its client
// stopped reading while it kept sending requests. The next request is then
// not served, and the conn is closed once what is queued has gone out.
func (cs *connState) overBacklogH1() bool {
	return cs.pendingBytes > maxPendingBytes
}

// connState holds per-connection state for the epoll engine.
// Fields are ordered for cache line optimization (P4): hot fields first.
type connState struct {
	// Hot path — first cache line:
	fd       int             // 8 bytes
	protocol engine.Protocol // 1 byte
	detected bool            // 1 byte
	dirty    bool            // 1 byte: true when writeBuf has data to flush
	epollOut bool            // 1 byte: true while EPOLLOUT is armed (write backpressure; edge-triggered, like EPOLLIN)
	detectN  uint8           // 1 byte: bytes at the head of buf received but too few to detect on, under detect.PrefaceLen (celeris#870); 0 once detected
	_        [3]byte         // padding to 8-byte alignment
	buf      []byte          // 24 bytes
	writeBuf []byte          // 24 bytes: single append buffer for pending writes
	bodyBuf  []byte          // 24 bytes: zero-copy body slice for writev scatter-gather

	// sendfile holds an in-progress zero-copy sendfile(2) response. Set by
	// the H1 response adapter's SetSendFileFn hook (non-async mode only).
	// flushWrites drives it AFTER writeBuf/bodyBuf drain (preserving
	// ordering with any prior pipelined bytes); on completion or conn close
	// the engine closes the dup'd file it owns. Mutually exclusive with
	// bodyBuf for a given response (the body comes from a file, not a
	// slice). nil when no sendfile is pending.
	sendfile *sendfileState

	// Warm — second cache line:
	pendingBytes int        // 8 bytes
	writePos     int        // 8 bytes: offset into writeBuf for next write(2) call
	dirtyNext    *connState // 8 bytes: intrusive doubly-linked dirty list
	dirtyPrev    *connState // 8 bytes

	// Warm — timeout tracking:
	lastActivity int64 // nanosecond timestamp of last I/O activity (for timeout checks)

	// Cold — third cache line:
	h1State      *conn.H1State
	h2State      *conn.H2State
	ctx          context.Context
	remoteAddr   string
	writeFn      func([]byte) // cached write function
	detachMu     *sync.Mutex  // non-nil after Detach(); guards writeBuf from event loop + goroutine
	detachClosed bool         // true after closeConn on a detached conn; writeFn becomes no-op

	// peerClosed is set when EPOLLRDHUP reports the peer half-closed (FIN) while
	// a response was still flushing (write backpressure). The conn is closed once
	// its pending write drains, so the response is not truncated. Reset on release.
	// closeWhenFlushed sets it too, when the engine itself ends a conn whose
	// response the kernel has not all taken (Connection: close, a request
	// error, a refused write; celeris#761): the close it defers is the same.
	peerClosed bool

	// closeSince, closePending and closeAcked are the clock of that deferred
	// close (celeris#876): closeSince is when the engine last saw the close
	// move forward, in nanoseconds, from the request that deferred it
	// (markClosing), then from each flush that took some of the response
	// (noteClosingProgress), then from each sample of the socket that finds
	// the peer has acknowledged more bytes (noteClosingAcked); closePending is
	// how many bytes were queued at the last flush, closeAcked the peer's
	// acknowledged byte count at the last sample (closeAckedKnown: whether
	// there is one) and closeSampled when that was. checkTimeouts reaps a conn
	// whose closeSince is older than closingDrainBound, and measures nothing
	// else on it: lastActivity is the last READ, and a conn that is closing is
	// no longer read. 0 when no deferred close is under way. Worker-thread-
	// only; reset on release.
	closeSince      int64
	closePending    int
	closeAcked      uint64
	closeSampled    int64
	closeAckedKnown bool

	// writeRefused records that response bytes were lost (celeris#761): a
	// write hook refused them because the conn's backlog was already over
	// writeCap, or the write the zero-copy body hook made failed, or a
	// staged file could not be read. It is never silent: the site that ran
	// the handler closes the conn (closeWhenFlushed) instead of leaving the
	// client waiting for bytes that will not come. Sticky until release.
	// Written under detachMu when the conn has one, like the buffers it
	// guards.
	writeRefused bool

	// drainDeadline bounds how long checkTimeouts defers the idle-deadline
	// reap of a truly-detached conn whose terminal bytes (SSE last event, WS
	// close echo) are still queued: stamped now+detachDrainGrace on the first
	// deferred sweep, cleared whenever the middleware pushes the idle deadline
	// back out. 0 when no drain is being waited on. Worker-thread-only;
	// reset on release.
	drainDeadline int64

	// WebSocket recv backpressure (detached conns only):
	recvPaused       bool        // engine-side current state (single-threaded write)
	recvPauseDesired atomic.Bool // requested state from middleware goroutine

	// Async handler dispatch (Config.AsyncHandlers=true, HTTP1 only):
	// Incoming bytes are appended to asyncInBuf under asyncInMu by the
	// worker. A single dispatch goroutine per conn drains asyncInBuf
	// via a double-buffer swap with asyncOutBuf (zero-alloc on the hot
	// path) and runs ProcessH1 over the pulled data. The goroutine
	// stays alive across requests — after draining, it blocks on
	// asyncCond.Wait rather than exiting, so a subsequent read from
	// the same keep-alive conn reuses the goroutine instead of paying
	// the spawn cost each request. Matches net/http's goroutine-per-
	// conn model while preserving HTTP/1.1 pipelining order (ProcessH1
	// handles pipelined requests in one shot via its internal offset
	// loop).
	asyncInBuf  []byte
	asyncOutBuf []byte
	asyncInMu   sync.Mutex
	asyncCond   sync.Cond   // L = &asyncInMu; signaled by worker on new data or close
	asyncRun    bool        // true while the dispatch goroutine is alive
	asyncParked bool        // true while the dispatch goroutine is parked in asyncCond.Wait (idle between requests); guarded by asyncInMu. #383 transplant only moves a promoted conn caught parked (not mid-ProcessH1, since the double-buffer swap empties asyncInBuf during processing).
	asyncClosed atomic.Bool // set by worker's close path; goroutine exits next iter
	// asyncQuiesce (#383) asks a promoted conn's dispatch goroutine to exit
	// cleanly (NOT close the conn) so the fd can be transplanted to io_uring.
	// Set by tryTransplant on the loop thread after detaching the fd from epoll;
	// the goroutine, on its next park, sees it, exits, and enqueues cs on
	// detachQueue for the loop to finish the handoff.
	asyncQuiesce atomic.Bool
	// xferAsked (celeris#657 P8) is set by this conn's dispatch goroutine,
	// by CAS, when it queues a park-boundary examination request on its
	// loop, and cleared by the loop when it drains or withdraws the ask. It
	// deduplicates the queue to one entry per park.
	xferAsked atomic.Bool
	// transplantPending (#383, loop-thread-only) marks a conn detached for
	// transplant whose dispatch goroutine must drain+exit first; drainDetachQueue
	// completes the handoff at the first entry it drains after that exit.
	transplantPending bool
	// transplanted (loop-thread-only) is set by finishTransplantHandoff once
	// the fd is handed over. Such a connState is never returned to the pool —
	// the goroutine's exit entry may still be queued behind the entry the
	// hand-off finished on — so every later entry finds this set and does
	// nothing (celeris#669).
	transplanted bool
	// asyncPromoted: once an async-marked route is observed on this conn
	// while it ran inline on the event loop (per-handler async, celeris
	// #300), the conn is promoted (sticky) — every subsequent recv goes
	// straight to the dispatch goroutine instead of retrying the inline
	// fast path. Reset on release.
	asyncPromoted bool
	// asyncH2Promoted signals that runAsyncHandler observed ErrUpgradeH2C
	// and completed the cs-local H1→H2 swap under detachMu. The worker
	// must finish the promotion in drainDetachQueue (append fd to
	// l.h2Conns — the only worker-thread-owned piece) and keep the conn
	// alive instead of closing it.
	asyncH2Promoted atomic.Bool

	// asyncDetachUnlocked is set by OnDetach when it releases detachMu
	// on behalf of the dispatch goroutine. runAsyncHandler observes it
	// after ProcessH1 returns and skips the symmetric final Unlock
	// (otherwise it would unlock an already-released mutex). Cleared by
	// releaseConnState. See internal/engine/iouring/conn.go for the full rationale
	// (celeris#273) — pre-fix, /ws and /events would TIMEOUT on
	// iouring+async or epoll+async because the dispatch goroutine
	// deadlocked on its own re-entrant Lock attempt when the middleware
	// emitted the 101 / SSE headers right after Detach.
	asyncDetachUnlocked bool

	// asyncDetachPending defers worker-private OnDetach mutations
	// (detachedCount++ and eventfd allocation + EPOLL_CTL_ADD) to
	// drainDetachQueue, which runs on the event-loop thread. In
	// async mode the dispatch goroutine fires OnDetach and cannot
	// safely mutate l.detachedCount (race with the event-loop's
	// adaptiveTimeout read) or the eventFD slot (race with the event
	// loop's epoll_wait set). The flag is set by OnDetach and
	// cleared by drainDetachQueue once the bookkeeping runs.
	asyncDetachPending bool

	// liveIdx is this conn's index into l.liveConns (the dense slice of
	// live connStates iterated by checkTimeouts, the sweep and shutdown).
	// -1 when not present. Maintained by addLiveConn/removeLiveConn for O(1)
	// removal (#318). Loop-thread-only, like liveConns itself (celeris#668).
	liveIdx int

	// hijacked is set by hijackConn (Context.Hijack) once the fd has been
	// detached from the engine and handed to the caller as a net.Conn. In
	// async mode hijackConn runs ON the dispatch goroutine (inside
	// ProcessH1 → ErrHijacked), which still touches cs after ProcessH1
	// returns — so the connState MUST NOT be released by hijackConn (#3.1),
	// and since celeris#668 it is never returned to the pool at all.
	//
	// Off-thread, hijackConn also enqueues cs at once, and the loop's
	// drainDetachQueue takes the conn out of l.liveConns and l.connCount,
	// which are the loop's and which the dispatch goroutine must not touch
	// (celeris#668). Until that runs, the conn's entry stays in the live set
	// with its descriptor already closed, so the live-set walkers and the
	// dirty pass skip a conn with this flag set. They read it without any
	// lock the goroutine holds, hence atomic; hijackConn stores it before it
	// releases the descriptor number.
	hijacked atomic.Bool

	// hijackSettled (loop-thread-only) records that drainDetachQueue has
	// taken an off-thread-hijacked conn out of the loop's state. Such a
	// connState is never returned to the pool — queue entries made before
	// or after the hijack may still name it — so every later entry finds
	// this set and does nothing (celeris#668).
	hijackSettled bool

	// h2GoAwaySent records that a graceful shutdown has sent this HTTP/2
	// conn its GOAWAY (celeris#759; Loop.h2PoolSettled). Loop thread; reset
	// on release.
	h2GoAwaySent bool

	// relinkOwed (guarded by asyncInMu) is set by the dirty pass or the
	// EPOLLOUT resume when they give the conn up because its dispatch
	// goroutine holds detachMu across a handler (celeris#669). The goroutine
	// hands cs back through the detach queue at its next park, and
	// drainDetachQueue puts it on the dirty list again. relinkPending
	// (loop-thread-only) is the loop's side of the same debt: while it is
	// set the conn is not offered to a transplant, because the loop has not
	// yet seen it as the handler left it. Any entry that puts cs back on the
	// dirty list clears it, the goroutine's own partial-flush entry included.
	// A deferred hand-off does not rely on it for memory safety: that
	// finishes only after the goroutine's exit and never pools cs.
	relinkOwed    bool
	relinkPending bool

	// closeOwed (guarded by asyncInMu) is set by closeConn when it finds
	// detachMu held by this conn's RUNNING dispatch goroutine — i.e. held
	// across a user handler — and leaves the close to that goroutine
	// instead of parking the loop on the lock for the rest of the handler
	// (celeris#669). asyncClosed is already set, so the goroutine exits at
	// its next check; the exit path that finds closeOwed hands cs back
	// through the detach queue, whose asyncClosed branch runs closeConn
	// again, on the loop thread, with the lock free.
	closeOwed bool

	// closeErr (loop-thread-only) is the I/O error a drainRead branch met
	// while a running handler held detachMu. That branch used to flush and
	// deliver it to OnError under the lock before closing; it now leaves
	// both to closeConn, which does them under the lock when the close
	// actually runs (celeris#669).
	closeErr error
}

var connStatePool = sync.Pool{
	New: func() any {
		return &connState{
			writeBuf: make([]byte, 0, 4096),
		}
	},
}

func acquireConnState(ctx context.Context, fd int, bufSize int, async bool) *connState {
	cs := connStatePool.Get().(*connState)
	cs.fd = fd
	cs.ctx = ctx
	cs.liveIdx = -1
	cs.writeBuf = cs.writeBuf[:0]
	cs.writePos = 0
	if cap(cs.buf) >= bufSize {
		cs.buf = cs.buf[:bufSize]
	} else {
		cs.buf = make([]byte, bufSize)
	}
	// Async handler dispatch: allocate detachMu so handler goroutine and
	// worker serialize writeBuf access. Harmless when unused.
	if async {
		cs.detachMu = &sync.Mutex{}
		cs.asyncCond.L = &cs.asyncInMu
	}
	return cs
}

func releaseConnState(cs *connState) {
	cs.h1State = nil
	cs.h2State = nil
	cs.ctx = nil
	cs.writeFn = nil
	cs.remoteAddr = ""
	cs.dirtyNext = nil
	cs.dirtyPrev = nil
	cs.protocol = 0
	cs.detected = false
	cs.detectN = 0
	cs.dirty = false
	cs.epollOut = false
	cs.pendingBytes = 0
	cs.writePos = 0
	cs.lastActivity = 0
	cs.detachMu = nil
	cs.detachClosed = false
	cs.recvPaused = false
	cs.recvPauseDesired.Store(false)
	cs.bodyBuf = nil
	if cs.sendfile != nil {
		cs.sendfile.close()
		cs.sendfile = nil
	}
	cs.asyncInBuf = trimPooledBuf(cs.asyncInBuf)
	cs.asyncOutBuf = trimPooledBuf(cs.asyncOutBuf)
	cs.writeBuf = trimPooledBuf(cs.writeBuf)
	cs.peerClosed = false
	cs.closeSince = 0
	cs.closePending = 0
	cs.closeAcked = 0
	cs.closeAckedKnown = false
	cs.closeSampled = 0
	cs.writeRefused = false
	cs.drainDeadline = 0
	cs.asyncRun = false
	cs.asyncParked = false
	cs.asyncClosed.Store(false)
	cs.asyncQuiesce.Store(false)
	cs.xferAsked.Store(false)
	cs.transplantPending = false
	cs.transplanted = false
	cs.asyncPromoted = false
	cs.asyncDetachUnlocked = false
	cs.asyncDetachPending = false
	cs.liveIdx = -1
	cs.hijacked.Store(false)
	cs.hijackSettled = false
	cs.h2GoAwaySent = false
	cs.closeOwed = false
	cs.closeErr = nil
	cs.relinkOwed = false
	cs.relinkPending = false
	cs.fd = 0
	connStatePool.Put(cs)
}
