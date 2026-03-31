//go:build linux

package iouring

import (
	"context"
	"sync"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/internal/conn"
)

// maxSendQueueBytes is the per-connection back-pressure limit.
// When pending send data exceeds this, the connection is closed to prevent
// unbounded memory growth when the SQ ring is full under sustained load.
const maxSendQueueBytes = 4 << 20 // 4 MiB

type connState struct {
	fd             int             // 8: real FD, or fixed file index
	protocol       engine.Protocol // 1
	detected       bool            // 1
	sending        bool            // 1: true when a SEND SQE is in-flight
	closing        bool            // 1: defers close until sends complete
	dirty          bool            // 1: true when data needs flushing
	fixedFile      bool            // 1: true when fd is fixed file index
	recvLinked     bool            // 1: RECV was linked to SEND (skip standalone prepareRecv)
	needsRecv      bool            // 1: recv arm was dropped (SQ ring full); retry on next opportunity
	zcNotifPending bool            // 1: waiting for SEND_ZC notification CQE
	zcSentBytes    int32           // bytes sent from first SEND_ZC CQE (processed on NOTIF)
	sendBuf        []byte          // 24: in-flight buffer (accessed with sending flag)

	writeBuf  []byte     // 24: append buffer for handler writes
	buf       []byte     // 24: per-connection recv buffer
	dirtyNext *connState // 8
	dirtyPrev *connState // 8

	lastActivity int64 // nanosecond timestamp of last I/O activity (for timeout checks)

	h1State    *conn.H1State
	h2State    *conn.H2State
	ctx        context.Context
	remoteAddr string
	writeFn    func([]byte) // cached write function (avoids closure allocation per recv)
	detachMu   *sync.Mutex  // non-nil after Detach(); guards writeBuf from event loop + goroutine
}

var connStatePool = sync.Pool{
	New: func() any {
		return &connState{
			writeBuf: make([]byte, 0, 4096),
			sendBuf:  make([]byte, 0, 4096),
		}
	},
}

func acquireConnState(ctx context.Context, fd int, bufSize int) *connState {
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
	cs.sending = false
	cs.closing = false
	cs.dirty = false
	cs.fixedFile = false
	cs.recvLinked = false
	cs.needsRecv = false
	cs.zcNotifPending = false
	cs.zcSentBytes = 0
	cs.lastActivity = 0
	cs.detachMu = nil
	cs.fd = 0
	connStatePool.Put(cs)
}
