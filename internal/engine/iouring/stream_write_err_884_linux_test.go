//go:build linux && (amd64 || arm64)

package iouring

import (
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/conn"
)

// The write-error query of an HTTP/1 StreamWriter (celeris#884 item 45, #885
// item 30), asked directly: the cases the engine tests reach only through a
// socket.

// A conn before Detach: the chunk about to be staged is refused when it would
// take the response held unsent past the limit, and exactly at the limit it is
// taken; the refusal is remembered, and a torn-down conn reports a closed one.
func TestStreamWriteErrCap884(t *testing.T) {
	cs := &connState{}
	cs.writeBuf = make([]byte, maxSendQueueBytesDetached-10)
	if err := cs.StreamWriteErr(10); err != nil {
		t.Fatalf("a chunk that brings the response to the limit exactly was refused: %v", err)
	}
	if cs.writeRefused {
		t.Fatal("writeRefused set by a chunk that was taken")
	}
	if err := cs.StreamWriteErr(11); !errors.Is(err, conn.ErrWriteRefused) {
		t.Fatalf("a chunk one byte over the limit: %v, want ErrWriteRefused", err)
	}
	if !cs.writeRefused {
		t.Fatal("a refused chunk did not mark the conn writeRefused, so it would not be closed")
	}
	if err := cs.StreamWriteErr(0); !errors.Is(err, conn.ErrWriteRefused) {
		t.Fatalf("after a refusal: %v, want ErrWriteRefused for everything that follows", err)
	}

	// The state alone (n = 0), as the adapter asks it after the write hook:
	// a backlog over the limit is refused, at it is not.
	cs = &connState{}
	cs.sendBuf = make([]byte, maxSendQueueBytesDetached)
	if err := cs.StreamWriteErr(0); err != nil {
		t.Fatalf("a response at the limit, asked with n=0: %v", err)
	}
	cs.writeBuf = make([]byte, 1)
	if err := cs.StreamWriteErr(0); !errors.Is(err, conn.ErrWriteRefused) {
		t.Fatalf("a response over the limit, asked with n=0: %v, want ErrWriteRefused", err)
	}

	cs = &connState{}
	cs.asyncClosed.Store(true)
	err := cs.StreamWriteErr(0)
	if !errors.Is(err, conn.ErrConnClosed) || !errors.Is(err, net.ErrClosed) {
		t.Fatalf("a torn-down conn: %v, want ErrConnClosed wrapping net.ErrClosed", err)
	}
}

// A detached conn: nil while it is healthy, and without the detach lock (a
// goroutine parked in a write holds it; the query must not wait for it, so a
// Write and its Flush do not each take it again); ErrWriteRefused once the
// guarded hook has refused a write, ErrConnClosed once the conn is torn down.
func TestDetachedStreamErr884(t *testing.T) {
	mu := &sync.Mutex{}
	cs := &connState{detachMu: mu}
	q := &detachedStreamErr{cs: cs, mu: mu}

	mu.Lock()
	done := make(chan error, 1)
	go func() { done <- q.StreamWriteErr(1 << 30) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a healthy detached conn: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the query of a healthy detached conn waited for the detach lock")
	}
	mu.Unlock()

	// As the guarded hook leaves a refusal: writeRefused under the lock, then
	// asyncClosed.
	mu.Lock()
	cs.writeRefused = true
	cs.asyncClosed.Store(true)
	mu.Unlock()
	if err := q.StreamWriteErr(0); !errors.Is(err, conn.ErrWriteRefused) {
		t.Fatalf("a detached conn whose write was refused: %v, want ErrWriteRefused", err)
	}

	cs = &connState{detachMu: mu}
	q = &detachedStreamErr{cs: cs, mu: mu}
	cs.asyncClosed.Store(true)
	if err := q.StreamWriteErr(0); !errors.Is(err, conn.ErrConnClosed) {
		t.Fatalf("a detached conn being closed: %v, want ErrConnClosed", err)
	}

	// stopDetachedProducers ends a detached conn's writes at the early drain
	// (endDetachedWrites) without asyncClosed: the query still reports it.
	cs = &connState{detachMu: mu}
	q = &detachedStreamErr{cs: cs, mu: mu}
	mu.Lock()
	endDetachedWrites(cs)
	mu.Unlock()
	if err := q.StreamWriteErr(0); !errors.Is(err, conn.ErrConnClosed) {
		t.Fatalf("a detached conn whose writes were ended by the early drain: %v, want ErrConnClosed", err)
	}
}

// The flag the slow path reads is the guarded hook's, written under the lock
// while the handler's goroutine asks: read without it, -race reports it.
func TestDetachedStreamErrReadsFlagsUnderLock884(t *testing.T) {
	mu := &sync.Mutex{}
	cs := &connState{detachMu: mu}
	cs.asyncClosed.Store(true)
	q := &detachedStreamErr{cs: cs, mu: mu}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			mu.Lock()
			cs.writeRefused = !cs.writeRefused
			mu.Unlock()
		}
	}()
	for i := 0; i < 2000; i++ {
		_ = q.StreamWriteErr(0)
	}
	wg.Wait()
}
