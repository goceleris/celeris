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
	dirtyNext    *connState // 8 bytes: intrusive doubly-linked dirty list
	dirtyPrev    *connState // 8 bytes

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
