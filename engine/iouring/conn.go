//go:build linux

package iouring

import (
	"context"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/internal/conn"
)

// connState holds per-connection state for the io_uring engine.
type connState struct {
	fd        int
	protocol  engine.Protocol
	buf       []byte
	h1State   *conn.H1State
	h2State   *conn.H2State
	ctx       context.Context
	cancel    context.CancelFunc
	detected  bool
	sendQueue [][]byte // FIFO queue of in-flight SEND buffers for partial send retry
	closing   bool     // defers close until all in-flight sends complete
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
