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

// connState holds per-connection state for the io_uring engine.
type connState struct {
	fd         int // real FD, or fixed file index when fixedFile is true
	protocol   engine.Protocol
	buf        []byte // per-connection recv buffer (nil when using multishot recv)
	h1State    *conn.H1State
	h2State    *conn.H2State
	ctx        context.Context
	cancel     context.CancelFunc
	detected   bool
	fixedFile  bool   // true when fd is a fixed file index
	writeBuf   []byte // append buffer: handler writes accumulate here
	sendBuf    []byte // in-flight buffer: kernel holds this until CQE
	sending    bool   // true when a SEND SQE is in-flight for this connection
	closing    bool   // defers close until all in-flight sends complete
	remoteAddr string
	dirty      bool       // true when data needs to be flushed
	dirtyNext  *connState // intrusive doubly-linked dirty list
	dirtyPrev  *connState
}

func newConnState(ctx context.Context, fd int, bufSize int) *connState {
	childCtx, cancel := context.WithCancel(ctx)
	return &connState{
		fd:     fd,
		buf:    make([]byte, bufSize),
		ctx:    childCtx,
		cancel: cancel,
	}
}
