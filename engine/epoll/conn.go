//go:build linux

// Package epoll implements the epoll-based I/O engine for Linux.
package epoll

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/internal/conn"
)

// maxPendingBytes is the per-connection back-pressure limit for pending
// writes on H1/H2 connections. Intentionally small (4 MiB) so a stalled
// peer cannot fill server memory with un-ACKed responses.
//
// maxPendingBytesDetached is the per-connection limit once the
// connection is detached (WebSocket / SSE). Detached middleware owns
// its own flow control — ReadLimit + backpressure — and may legitimately
// echo payloads larger than 4 MiB (RFC 6455 allows frames up to 2^63,
// Autobahn 9.1.6 sends 16 MiB). 64 MiB matches the WS default ReadLimit.
const (
	maxPendingBytes         = 4 << 20  // 4 MiB (H1/H2)
	maxPendingBytesDetached = 64 << 20 // 64 MiB (WS/SSE)
	// maxPendingInputBytes caps the async dispatch input buffer
	// (cs.asyncInBuf) so a client pipelining requests faster than
	// the dispatch goroutine drains them cannot balloon per-conn
	// memory. Same ceiling as the output side (4 MiB) — a full
	// saturated buffer pair is 8 MiB per conn.
	maxPendingInputBytes = 4 << 20
)

// writeCap returns the effective back-pressure limit for cs, accounting
// for whether the connection is detached. Async-mode HTTP1 conns set
// detachMu up front without being truly detached; they keep the H1/H2
// limit so a stalled peer cannot balloon per-conn memory to 64 MiB.
func (cs *connState) writeCap() int {
	if cs.detachMu != nil && cs.h1State != nil && cs.h1State.Detached {
		return maxPendingBytesDetached
	}
	return maxPendingBytes
}

// connState holds per-connection state for the epoll engine.
// Fields are ordered for cache line optimization (P4): hot fields first.
type connState struct {
	// Hot path — first cache line:
	fd       int             // 8 bytes
	protocol engine.Protocol // 1 byte
	detected bool            // 1 byte
	dirty    bool            // 1 byte: true when writeBuf has data to flush
	_        [5]byte         // padding to 8-byte alignment
	buf      []byte          // 24 bytes
	writeBuf []byte          // 24 bytes: single append buffer for pending writes
	bodyBuf  []byte          // 24 bytes: zero-copy body slice for writev scatter-gather

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
	asyncClosed atomic.Bool // set by worker's close path; goroutine exits next iter
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
	cs.dirty = false
	cs.pendingBytes = 0
	cs.writePos = 0
	cs.lastActivity = 0
	cs.detachMu = nil
	cs.detachClosed = false
	cs.recvPaused = false
	cs.recvPauseDesired.Store(false)
	cs.bodyBuf = nil
	cs.asyncInBuf = cs.asyncInBuf[:0]
	cs.asyncOutBuf = cs.asyncOutBuf[:0]
	cs.asyncRun = false
	cs.asyncClosed.Store(false)
	cs.fd = 0
	connStatePool.Put(cs)
}
