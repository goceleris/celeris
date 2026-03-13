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
// Fields are ordered for cache line optimization (P4): hot fields in the first
// 64-byte cache line, warm in the second, cold in the third.
type connState struct {
	// Hot path — first cache line (~62 bytes on x86/arm64):
	fd       int             // 8 bytes: real FD, or fixed file index when fixedFile is true
	protocol engine.Protocol // 1 byte
	detected bool            // 1 byte
	sending  bool            // 1 byte: true when a SEND SQE is in-flight
	closing  bool            // 1 byte: defers close until all in-flight sends complete
	dirty    bool            // 1 byte: true when data needs to be flushed
	fixedFile bool           // 1 byte: true when fd is a fixed file index
	_        [2]byte         // padding to 8-byte alignment
	buf      []byte          // 24 bytes: per-connection recv buffer (nil when using multishot recv)
	writeBuf []byte          // 24 bytes: append buffer: handler writes accumulate here

	// Warm — second cache line:
	sendBuf   []byte     // 24 bytes: in-flight buffer: kernel holds this until CQE
	dirtyNext *connState // 8 bytes: intrusive doubly-linked dirty list
	dirtyPrev *connState // 8 bytes

	// Cold — third cache line:
	h1State    *conn.H1State
	h2State    *conn.H2State
	ctx        context.Context
	cancel     context.CancelFunc
	remoteAddr string
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
