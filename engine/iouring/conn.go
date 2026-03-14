//go:build linux

package iouring

import (
	"context"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/internal/conn"
)

// maxSendQueueBytes is the per-connection back-pressure limit.
// When pending send data exceeds this, the connection is closed to prevent
// unbounded memory growth when the SQ ring is full under sustained load.
const maxSendQueueBytes = 4 << 20 // 4 MiB

type connState struct {
	fd        int             // 8: real FD, or fixed file index
	protocol  engine.Protocol // 1
	detected  bool            // 1
	sending   bool            // 1: true when a SEND SQE is in-flight
	closing   bool            // 1: defers close until sends complete
	dirty     bool            // 1: true when data needs flushing
	fixedFile bool            // 1: true when fd is fixed file index
	_         [2]byte
	sendBuf   []byte // 24: in-flight buffer (accessed with sending flag)

	writeBuf  []byte     // 24: append buffer for handler writes
	buf       []byte     // 24: per-connection recv buffer
	dirtyNext *connState // 8
	dirtyPrev *connState // 8

	lastActivity int64 // nanosecond timestamp of last I/O activity (for timeout checks)

	h1State    *conn.H1State
	h2State    *conn.H2State
	ctx        context.Context
	cancel     context.CancelFunc
	remoteAddr string
	writeFn    func([]byte) // cached write function (avoids closure allocation per recv)
}

func newConnState(ctx context.Context, fd int, bufSize int) *connState {
	childCtx, cancel := context.WithCancel(ctx)
	cs := &connState{
		fd:     fd,
		ctx:    childCtx,
		cancel: cancel,
	}
	if bufSize > 0 {
		cs.buf = make([]byte, bufSize)
	}
	return cs
}
