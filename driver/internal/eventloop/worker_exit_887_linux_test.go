//go:build linux

package eventloop

// celeris#887 item 18: a worker goroutine that ends without returning (an
// onRecv that calls runtime.Goexit) must be reported, as celeris#848 does for
// the HTTP engines' loops. The standalone driver loop runs the drivers' own
// onRecv callbacks on that goroutine, and postgres's Pool.CopyTo runs the
// caller's dest there; a dest that calls t.FailNow ends the worker, and every
// conn on it is never read again.
//
// The report goes to slog.Default (the driver packages have no logger of
// their own), so the tests swap the default logger for one that records.

import (
	"log/slog"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestAWorkerWhoseOnRecvGoexitsIsReportedAndCloseReturns887(t *testing.T) {
	h := c887Capture(t)
	l, err := New(2)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = l.Close()
		}
	})
	w := l.WorkerLoop(0).(*worker)
	x, xPeer := socketPair(t)
	t.Cleanup(func() { _ = w.UnregisterConn(x); _ = unix.Close(x); _ = unix.Close(xPeer) })
	if err := w.RegisterConn(x, func([]byte) { runtime.Goexit() }, func(error) {}); err != nil {
		t.Fatalf("RegisterConn: %v", err)
	}
	c784Queue(t, xPeer, []byte("x")) // the worker reads it and its onRecv Goexits

	select {
	case <-h.got:
	case <-time.After(5 * time.Second):
		t.Fatal("the worker goroutine ended in a runtime.Goexit and nothing was logged")
	}
	recs := h.snapshot()
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1: %v", len(recs), recs)
	}
	r := recs[0]
	var stack string
	var worker int64 = -1
	r.Attrs(func(a slog.Attr) bool {
		switch a.Key {
		case "stack":
			stack = a.Value.String()
		case "worker":
			worker = a.Value.Int64()
		}
		return true
	})
	if r.Level != slog.LevelError {
		t.Errorf("level %v, want Error", r.Level)
	}
	if worker != 0 {
		t.Errorf("worker attr = %d, want 0", worker)
	}
	if !strings.Contains(stack, "Goexit") {
		t.Errorf("the stack does not show the Goexit call:\n%s", stack)
	}

	// Close must not hang on the worker that is gone.
	done := make(chan error, 1)
	go func() { done <- l.Close() }()
	select {
	case err := <-done:
		closed = true
		if err != nil {
			t.Errorf("Close: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return within 10 s after a worker goroutine ended in a Goexit")
	}
	if n := len(h.snapshot()); n != 1 {
		t.Errorf("%d records after Close, want only the Goexit's", n)
	}
}

func TestACloseOfAHealthyLoopReportsNothing887(t *testing.T) {
	h := c887Capture(t)
	l, err := New(2)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w := l.WorkerLoop(0).(*worker)
	x, xPeer := socketPair(t)
	t.Cleanup(func() { _ = unix.Close(x); _ = unix.Close(xPeer) })
	got := make(chan struct{}, 1)
	if err := w.RegisterConn(x, func([]byte) {
		select {
		case got <- struct{}{}:
		default:
		}
	}, func(error) {}); err != nil {
		t.Fatalf("RegisterConn: %v", err)
	}
	c784Queue(t, xPeer, []byte("x"))
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("the worker never read the byte: the loop did not run")
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Close joined the workers, and a report runs before a worker goroutine
	// ends, so a record would be here by now.
	if recs := h.snapshot(); len(recs) != 0 {
		t.Errorf("a normal Close logged %d record(s): %v", len(recs), recs)
	}
}
