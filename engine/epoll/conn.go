//go:build linux

// Package epoll implements the epoll-based I/O engine for Linux.
package epoll

import (
	"context"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/internal/conn"
)

// maxPendingBytes is the per-connection back-pressure limit for pending writes.
const maxPendingBytes = 4 << 20 // 4 MiB

type connState struct {
	fd           int
	protocol     engine.Protocol
	detected     bool
	buf          []byte
	h1State      *conn.H1State
	h2State      *conn.H2State
	ctx          context.Context
	cancel       context.CancelFunc
	pending      [][]byte
	pendingBytes int
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
