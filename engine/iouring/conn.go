//go:build linux

package iouring

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/goceleris/celeris/internal/conn"
)

// maxSendQueueBytes is the per-connection back-pressure limit for
// H1/H2 connections. When pending send data exceeds this, the
// connection is closed to prevent unbounded memory growth while a
// slow peer stalls with un-ACKed responses.
//
// maxSendQueueBytesDetached is the corresponding limit once a
// connection is detached (WebSocket / SSE). Detached middleware owns
// its own flow control — ReadLimit + backpressure — and may legitimately
// echo payloads larger than 4 MiB (Autobahn 9.1.6 sends 16 MiB).
// 64 MiB matches the WS default ReadLimit.
const (
	maxSendQueueBytes         = 4 << 20  // 4 MiB (H1/H2)
	maxSendQueueBytesDetached = 64 << 20 // 64 MiB (WS/SSE)
	// maxPendingInputBytes caps the async dispatch input buffer
	// (cs.asyncInBuf) so a client pipelining requests faster than
	// the dispatch goroutine drains them cannot balloon per-conn
	// memory. Matches the send-side cap.
	maxPendingInputBytes = 4 << 20
)

// sendCap returns the effective back-pressure limit for cs, accounting
// for whether the connection is detached. Async-mode HTTP1 conns set
// detachMu up front without being truly detached; they keep the H1/H2
// limit so a stalled peer cannot balloon per-conn memory to 64 MiB.
func (cs *connState) sendCap() int {
	if cs.detachMu != nil && cs.h1State != nil && cs.h1State.Detached {
		return maxSendQueueBytesDetached
	}
	return maxSendQueueBytes
}

// iovec mirrors Linux struct iovec (16 bytes on 64-bit platforms).
// Used for IORING_OP_WRITEV scatter-gather sends. Celeris only builds
// on amd64 and arm64 (both 64-bit), so size is stable at 16.
type iovec struct {
	Base uintptr
	Len  uint64
}

type connState struct {
	fd int // 8: real FD, or fixed file index
	// protocol is accessed concurrently: the worker reads it on every
	// recv completion, and the async dispatch goroutine writes it once
	// via switchToH2Local during an H1→H2 upgrade. atomic.Int32 keeps
	// the common-case Load a single mov instruction while making the
	// write race-free. Use cs.getProtocol / cs.setProtocol helpers.
	protocol       atomic.Int32 // engine.Protocol values cast to int32
	detected       bool         // 1
	sending        bool         // 1: true when a SEND SQE is in-flight
	closing        bool         // 1: defers close until sends complete
	dirty          bool         // 1: true when data needs flushing
	fixedFile      bool         // 1: true when fd is fixed file index
	recvLinked     bool         // 1: RECV was linked to SEND (skip standalone prepareRecv)
	needsRecv      bool         // 1: recv arm was dropped (SQ ring full); retry on next opportunity
	recvIntoBody   bool         // 1: next recv CQE fills h1State.bodyBuf directly (skips ProcessH1 + cs.buf memcpy)
	zcNotifPending bool         // 1: waiting for SEND_ZC notification CQE
	zcSentBytes    int32        // bytes sent from first SEND_ZC CQE (processed on NOTIF)
	sendBuf        []byte       // 24: in-flight buffer (accessed with sending flag)

	writeBuf  []byte     // 24: append buffer for handler writes
	bodyBuf   []byte     // 24: zero-copy body slice; sent as iovec[1] alongside sendBuf
	sendBody  []byte     // 24: in-flight body slice during WRITEV (cleared by completeSend)
	buf       []byte     // 24: per-connection recv buffer
	iov       [2]iovec   // 32: iovec storage for WRITEV SQEs (sendBuf + sendBody)
	dirtyNext *connState // 8
	dirtyPrev *connState // 8

	lastActivity int64 // nanosecond timestamp of last I/O activity (for timeout checks)

	h1State      *conn.H1State
	h2State      *conn.H2State
	ctx          context.Context
	remoteAddr   string
	writeFn      func([]byte) // cached write function (avoids closure allocation per recv)
	detachMu     *sync.Mutex  // non-nil after Detach(); guards writeBuf from event loop + goroutine
	detachClosed bool         // true after closeConn on a detached conn; writeFn becomes no-op

	// WebSocket recv backpressure (detached conns only):
	recvPaused       bool        // engine-side current state (worker-thread only)
	recvPauseDesired atomic.Bool // requested state from middleware goroutine

	// Async handler dispatch (Worker.async=true, HTTP1 only):
	// Incoming recv bytes are appended under asyncInMu by the worker.
	// A single dispatch goroutine per conn drains asyncInBuf via a
	// double-buffer swap with asyncOutBuf, runs ProcessH1 under
	// detachMu, then enqueues on detachQueue so the worker submits
	// SEND SQEs on its own goroutine (SINGLE_ISSUER). The goroutine
	// parks on asyncCond between requests rather than exiting — saves
	// the ~1.5µs spawn cost on keep-alive conns. Shape matches
	// engine/epoll's runAsyncHandler and preserves HTTP/1.1 pipelining.
	asyncInBuf  []byte
	asyncOutBuf []byte
	asyncInMu   sync.Mutex
	asyncCond   sync.Cond
	asyncRun    bool
	asyncClosed atomic.Bool
	// asyncH2Promoted signals the worker that runAsyncHandler observed
	// ErrUpgradeH2C and completed the cs-local H1→H2 state swap under
	// detachMu. drainDetachQueue finishes the promotion by appending
	// cs.fd to w.h2Conns (the worker-owned write-queue poll list) and
	// keeps the conn alive, rather than routing it through the
	// asyncClosed teardown.
	asyncH2Promoted atomic.Bool
}

var connStatePool = sync.Pool{
	New: func() any {
		return &connState{
			writeBuf: make([]byte, 0, 4096),
			sendBuf:  make([]byte, 0, 4096),
		}
	},
}

func acquireConnState(ctx context.Context, fd int, bufSize int, async bool) *connState {
	cs := connStatePool.Get().(*connState)
	cs.fd = fd
	cs.ctx = ctx
	cs.writeBuf = cs.writeBuf[:0]
	cs.sendBuf = cs.sendBuf[:0]
	if bufSize > 0 {
		if cap(cs.buf) >= bufSize {
			cs.buf = cs.buf[:bufSize]
		} else {
			cs.buf = make([]byte, bufSize)
		}
	}
	// Async handler dispatch: pre-allocate detachMu so the dispatch
	// goroutine and the worker can serialize writeBuf access without a
	// later install step. Harmless (nil-free) when unused.
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
	cs.protocol.Store(0)
	cs.detected = false
	cs.sending = false
	cs.closing = false
	cs.dirty = false
	cs.fixedFile = false
	cs.recvLinked = false
	cs.needsRecv = false
	cs.recvIntoBody = false
	cs.zcNotifPending = false
	cs.zcSentBytes = 0
	cs.lastActivity = 0
	cs.detachMu = nil
	cs.detachClosed = false
	cs.recvPaused = false
	cs.recvPauseDesired.Store(false)
	cs.asyncInBuf = cs.asyncInBuf[:0]
	cs.asyncOutBuf = cs.asyncOutBuf[:0]
	cs.asyncRun = false
	cs.asyncClosed.Store(false)
	cs.bodyBuf = nil
	cs.sendBody = nil
	cs.fd = 0
	connStatePool.Put(cs)
}
