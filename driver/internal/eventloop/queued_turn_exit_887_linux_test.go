//go:build linux

package eventloop

// celeris#887 item 18, review r1 of #1001: a worker whose onRecv ends the
// goroutine with runtime.Goexit during a QUEUED read turn (the conn used up
// readBudget in its first turn, celeris#881) must let go of the conn's
// recvMu. readTurn defers the unlock; serveReadQ released it with a plain
// Unlock after the turn, so the dead goroutine kept the lock and any later
// WriteAndPoll* on the conn blocked in recvMu.Lock for good, with no ctx,
// instead of reading the conn itself.

import (
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestAWorkerThatGoexitsInAQueuedTurnLeavesRecvMuFree887(t *testing.T) {
	h := c887Capture(t)
	l, err := New(1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	w := l.WorkerLoop(0).(*worker)
	x, xw := c881Pipe(t)
	t.Cleanup(func() { _ = w.UnregisterConn(x); _ = unix.Close(x); _ = unix.Close(xw) })

	// One turn's worth and a little more: the first turn uses up its budget
	// and queues X, the queued turn reads the rest and Goexits.
	var got atomic.Int64
	if err := w.RegisterConn(x, func(p []byte) {
		if got.Add(int64(len(p))) > c881Turn {
			runtime.Goexit()
		}
	}, func(error) {}); err != nil {
		t.Fatalf("RegisterConn: %v", err)
	}
	if n, err := unix.Write(xw, make([]byte, c881Turn+100)); n != c881Turn+100 || err != nil {
		t.Fatalf("fill X: wrote %d of %d (%v)", n, c881Turn+100, err)
	}
	select {
	case <-h.got:
	case <-time.After(5 * time.Second):
		t.Fatalf("the worker never ended in the queued turn's Goexit (X got %d bytes)", got.Load())
	}

	w.mu.RLock()
	c := w.conns[x]
	w.mu.RUnlock()
	if c == nil {
		t.Fatal("X is not registered")
	}
	if !c.recvMu.TryLock() {
		t.Fatal("X's recvMu is still held by the worker goroutine that Goexited in X's queued turn: a WriteAndPoll* on X blocks for good")
	}
	c.recvMu.Unlock()
}
