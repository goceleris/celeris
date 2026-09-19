//go:build linux

package wakefd

import (
	"sync"
	"testing"
	"time"

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

// TestConcurrentSetAndSignal is the epoll field race (E4), which this type
// is also the fix for and which TestConcurrentSignalAndClose does NOT cover:
// that one races Signal against Close, this one races Signal against Set.
//
// On main the shape was an unsynchronised field. epoll creates its wakeup
// eventfd at startup, and if that create failed (EMFILE) it retries later —
// on the loop thread, writing l.eventFD with no lock — while AdoptConn, the
// detached WS/SSE closures and switchToH2Local read the same field from
// other goroutines. Here Set takes the write lock Signal reads under, so the
// -race detector has to stay quiet and the installed descriptor has to be
// the one every later Signal uses.
func TestConcurrentSetAndSignal(t *testing.T) {
	rd, wr := pipe655(t)
	// The handle owns its own descriptor, so Close here cannot double-close
	// the one pipe655's cleanup closes.
	owned, err := unix.Dup(wr)
	if err != nil {
		t.Fatalf("dup: %v", err)
	}

	// A loop whose eventfd creation failed at startup: no descriptor yet.
	w := New(-1)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			// Signal at least 200 times, and keep going until this producer
			// has seen the Set land, then once more. Every producer therefore
			// signals after the Set whatever order the scheduler picks, which
			// the assertion below depends on. A fixed 200 let all 3,200
			// Signals finish before the Set goroutine ran, and the test then
			// failed with nothing wrong (seen once on a GitHub runner). The
			// cap only stops a producer spinning forever if Set never lands.
			seen := false
			for i := 0; i < 1<<20; i++ {
				w.Signal()
				if seen && i >= 200 {
					return
				}
				seen = seen || w.FD() == owned
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		if !w.Set(owned) {
			t.Error("Set on a live handle returned false")
		}
	}()
	close(start)
	wg.Wait()

	if got := w.FD(); got != owned {
		t.Errorf("FD() = %d after the concurrent Set(%d): the descriptor the loop "+
			"installed must be the one every producer signals", got, owned)
	}
	// Signals that ran after the Set must have reached the new descriptor.
	if n := wrote655(t, rd); n == 0 {
		t.Error("nothing reached the descriptor Set installed; the producers " +
			"would be signalling into a handle that never took it")
	}
	for wrote655(t, rd) > 0 {
	}
	w.Close()
}

// TestNewForcesNonBlocking pins the precondition Signal's bounded-time
// guarantee rests on. Signal holds the read lock across its write(2) and
// Close waits behind the signals in flight, so a descriptor whose write can
// block would stall a loop's shutdown for as long as the peer takes to
// drain. Every engine creates its eventfd with EFD_NONBLOCK, but New and Set
// accept any descriptor, so the type enforces it rather than documenting it
// and hoping.
//
// The pipe is filled first (through a temporarily non-blocking descriptor,
// then restored to blocking) so that a write(2) on it genuinely would park.
func TestNewForcesNonBlocking(t *testing.T) {
	var p [2]int
	if err := unix.Pipe2(p[:], unix.O_CLOEXEC); err != nil { // BLOCKING on purpose
		t.Fatalf("pipe2: %v", err)
	}
	t.Cleanup(func() { _ = unix.Close(p[0]); _ = unix.Close(p[1]) })

	flags, err := unix.FcntlInt(uintptr(p[1]), unix.F_GETFL, 0)
	if err != nil {
		t.Fatalf("F_GETFL: %v", err)
	}
	if flags&unix.O_NONBLOCK != 0 {
		t.Fatalf("precondition: the pipe is already non-blocking, so this test " +
			"could pass without the type enforcing anything")
	}
	if _, err := unix.FcntlInt(uintptr(p[1]), unix.F_SETFL, flags|unix.O_NONBLOCK); err != nil {
		t.Fatalf("F_SETFL O_NONBLOCK: %v", err)
	}
	buf := make([]byte, 4096)
	for {
		if _, werr := unix.Write(p[1], buf); werr != nil {
			break // EAGAIN: the pipe buffer is full
		}
	}
	if _, err := unix.FcntlInt(uintptr(p[1]), unix.F_SETFL, flags); err != nil {
		t.Fatalf("F_SETFL restore: %v", err)
	}

	owned, err := unix.Dup(p[1])
	if err != nil {
		t.Fatalf("dup: %v", err)
	}
	w := New(owned)

	if fl, ferr := unix.FcntlInt(uintptr(owned), unix.F_GETFL, 0); ferr != nil {
		t.Fatalf("F_GETFL after New: %v", ferr)
	} else if fl&unix.O_NONBLOCK == 0 {
		t.Fatal("New left the descriptor blocking: a Signal on a full pipe would " +
			"park holding the read lock, and Close — which runs on the loop " +
			"thread at shutdown — would wait behind it")
	}

	// Behavioural half: a signal into the full pipe must not stall Close.
	done := make(chan struct{})
	go func() {
		w.Signal()
		w.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Close did not return: a Signal is parked in write(2) with the " +
			"read lock held (celeris#655)")
	}
}
