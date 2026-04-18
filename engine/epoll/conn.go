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
)

// writeCap returns the effective back-pressure limit for cs, accounting
// for whether the connection is detached.
func (cs *connState) writeCap() int {
	if cs.detachMu != nil {
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
}

var connStatePool = sync.Pool{
	New: func() any {
		return &connState{
			writeBuf: make([]byte, 0, 4096),
		}
	},
}

func acquireConnState(ctx context.Context, fd int, bufSize int) *connState {
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
	// Async handler dispatch experiment: always allocate detachMu so
	// handler goroutine and worker can serialize writeBuf access.
	// Gated by CELERIS_ASYNC_HANDLERS=1 — detachMu is harmless when unused.
	if asyncHandlers {
		cs.detachMu = &sync.Mutex{}
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
	cs.fd = 0
	connStatePool.Put(cs)
}
