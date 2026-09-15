//go:build linux

package wakefd

import (
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

// pipe655 returns a non-blocking pipe. The write end stands in for a loop's
// eventfd — what matters here is only whether Signal wrote to it.
func pipe655(t *testing.T) (rd, wr int) {
	t.Helper()
	var p [2]int
	if err := unix.Pipe2(p[:], unix.O_NONBLOCK|unix.O_CLOEXEC); err != nil {
		t.Fatalf("pipe2: %v", err)
	}
	t.Cleanup(func() { _ = unix.Close(p[0]); _ = unix.Close(p[1]) })
	return p[0], p[1]
}

// wrote655 reports how many bytes reached the read end since the last call.
func wrote655(t *testing.T, rd int) int {
	t.Helper()
	var b [64]byte
	n, err := unix.Read(rd, b[:])
	if err == unix.EAGAIN {
		return 0
	}
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return n
}

// TestSignalStopsAtClose is the whole point of the type (celeris#655): the
// descriptor is owned, not published, so a producer that runs after the loop
// shut down writes nothing at all.
func TestSignalStopsAtClose(t *testing.T) {
	rd, wr := pipe655(t)
	w := New(wr)

	w.Signal()
	if n := wrote655(t, rd); n != 8 {
		t.Fatalf("Signal on a live handle wrote %d bytes, want 8", n)
	}

	w.Close()
	w.Signal()
	if n := wrote655(t, rd); n != 0 {
		t.Errorf("Signal after Close wrote %d bytes; a producer that outlives the "+
			"loop must write nothing (celeris#655)", n)
	}
	if got := w.FD(); got != -1 {
		t.Errorf("FD() = %d after Close, want -1: the number must not survive the close", got)
	}
	w.Close() // idempotent
}

// TestSetRefusesAfterClose covers the lazy-creation path: a loop that creates
// its eventfd late must learn that the handle is gone, because then the
// descriptor it just created is its own to close and nothing else would.
func TestSetRefusesAfterClose(t *testing.T) {
	_, wr := pipe655(t)
	w := New(-1)
	if !w.Set(wr) {
		t.Fatal("Set on a live handle returned false")
	}
	if got := w.FD(); got != wr {
		t.Fatalf("FD() = %d after Set(%d)", got, wr)
	}
	w.Close()
	if w.Set(wr) {
		t.Error("Set after Close returned true: the caller would leak the descriptor " +
			"it just created, because the loop is gone and will never close it")
	}
}

// TestNilHandleIsDisabled: a nil *WakeFD is how a loop with no eventfd — and
// every engine test fixture that does not want one — expresses "disabled".
func TestNilHandleIsDisabled(t *testing.T) {
	var w *WakeFD
	w.Signal()
	w.Close()
	if got := w.FD(); got != -1 {
		t.Errorf("nil handle FD() = %d, want -1", got)
	}
	if w.Set(3) {
		t.Error("nil handle Set returned true")
	}
}

// TestNoFDWritesNothing: New(-1) is the loop whose eventfd creation failed.
func TestNoFDWritesNothing(t *testing.T) {
	w := New(-1)
	w.Signal() // must not write to fd -1, or to anything else
	w.Close()
}

// TestConcurrentSignalAndClose is the -race guard: producers on many
// goroutines signalling while the loop shuts down. Before celeris#655 the
// engines read and wrote the descriptor number across goroutines with no
// lock at all (epoll's lazy re-creation vs AdoptConn).
func TestConcurrentSignalAndClose(t *testing.T) {
	rd, wr := pipe655(t)
	w := New(wr)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range 50 {
				w.Signal()
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		w.Close()
	}()
	close(start)
	wg.Wait()

	// Drain whatever landed before the close; the point is that the race
	// detector stays quiet and no Signal touched a closed descriptor.
	for wrote655(t, rd) > 0 {
	}
	if got := w.FD(); got != -1 {
		t.Errorf("FD() = %d after the concurrent Close, want -1", got)
	}
}
