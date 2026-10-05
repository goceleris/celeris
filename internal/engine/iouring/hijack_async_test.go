//go:build linux

package iouring

import (
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// TestHijackRefusedOnAsyncDispatchPath pins celeris#539.
//
// hijackConn is worker-owned work: it mutates w.conns, w.connCount,
// w.liveConns and the dirty list, and submits an ASYNC_CANCEL SQE, which the
// engine treats as single-issuer. In async mode the handler runs on the
// per-connection dispatch goroutine, and Hijack() reaches this through
// h1State.HijackFn — so performing it there races the worker's own loop.
//
// Refusing is the safe behaviour until the synchronous detach-queue round
// trip lands (v1.7.0); the alternative is silently corrupting worker state.
func TestHijackRefusedOnAsyncDispatchPath(t *testing.T) {
	fd, other := socketPairFDs(t)
	defer func() { _ = unix.Close(other) }()
	defer func() { _ = unix.Close(fd) }()

	w := newDirtyTestWorker(fd)
	w.async = true
	cs := &connState{fd: fd, liveIdx: -1}
	w.conns[fd] = cs
	w.connCount = 1

	c, err := w.hijackConn(fd)
	if err == nil {
		_ = c.Close()
		t.Fatal("hijackConn succeeded on the async dispatch path: it mutates worker-owned " +
			"state and submits an SQE off the single-issuer thread")
	}
	if !strings.Contains(err.Error(), "AsyncHandlers") {
		t.Errorf("error should say why and what to do instead, got: %v", err)
	}

	// The refusal must be clean: nothing unregistered, nothing unlinked.
	if w.conns[fd] != cs {
		t.Error("connection was unregistered despite the refusal")
	}
	if w.connCount != 1 {
		t.Errorf("connCount = %d, want 1 — the refusal must not alter accounting", w.connCount)
	}
}
