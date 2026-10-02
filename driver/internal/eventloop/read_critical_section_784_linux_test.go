//go:build linux

package eventloop

// celeris#784 (item 3's question, asked of the standalone loop): the closed
// check and the read must be ONE critical section with the flag UnregisterConn
// sets before it returns. A reader that checks and then reads in a separate
// step passes the tests in read_after_unregister_784_linux_test.go, which
// unregister while the reader is parked in onRecv, before its next check. It
// fails here: testHookBeforeRead runs between the check and the read, and from
// inside it the test tries to unregister the conn, close it and put another
// socket on its number.

import (
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestReadCheckAndReadAreOneCriticalSection784 calls handleReadable on the
// test goroutine (the worker has no goroutine of its own). Inside the hook it
// starts UnregisterConn(A) on another goroutine and gives it 300 ms. If it
// returns in that time, the reader has been overtaken between its check and
// its read: the test closes A, moves X onto A's number with bytes waiting, and
// lets the read go, which then reads X. If it does not return, UnregisterConn
// is waiting for the read, as it must; the test does the close and the reuse
// once it has returned.
func TestReadCheckAndReadAreOneCriticalSection784(t *testing.T) {
	a, aPeer := socketPair(t)
	t.Cleanup(func() { _ = unix.Close(aPeer) })
	c784Queue(t, aPeer, []byte("AAAA"))

	var (
		w         *worker
		armed     = true
		overtaken bool
		hookRan   bool
		unregDone = make(chan error, 1)
		x, xPeer  = -1, -1
	)
	reuse := func() {
		if err := unix.Close(a); err != nil {
			t.Fatalf("close A: %v", err)
		}
		x, xPeer = c784TakeNumber(t, a, true)
		c784Queue(t, xPeer, []byte(c784Stolen))
	}
	// Registered first, so it runs last: after the worker is shut down.
	t.Cleanup(func() { testHookBeforeRead = nil })
	testHookBeforeRead = func(fd int) {
		if fd != a || !armed {
			return
		}
		armed = false
		hookRan = true
		go func() { unregDone <- w.UnregisterConn(a) }()
		select {
		case err := <-unregDone:
			if err != nil {
				t.Errorf("UnregisterConn(A): %v", err)
			}
			overtaken = true
			reuse()
		case <-time.After(300 * time.Millisecond):
		}
	}

	var err error
	w, err = newWorker(0)
	if err != nil {
		t.Fatalf("newWorker: %v", err)
	}
	t.Cleanup(func() { _ = w.shutdown() })

	var chunks [][]byte
	if err := w.RegisterConn(a, func(b []byte) {
		chunks = append(chunks, append([]byte(nil), b...))
	}, func(error) {}); err != nil {
		t.Fatalf("RegisterConn(A): %v", err)
	}

	w.handleReadable(a, unix.EPOLLIN)
	if !hookRan {
		t.Fatal("testHookBeforeRead never ran for A: the reader did not go through the hooked read")
	}
	if !overtaken {
		select {
		case err := <-unregDone:
			if err != nil {
				t.Fatalf("UnregisterConn(A): %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("UnregisterConn(A) did not return within 5 s of the read")
		}
		reuse()
		// An event for the old number that the worker collected before the
		// unregister: the conn is gone, so it must not be read.
		w.handleReadable(a, unix.EPOLLIN)
	}

	got, rerr := c784ReadAll(x)
	stole := c784Contains(chunks, c784Stolen)
	t.Logf("C784 critical-section: UnregisterConn returned while the reader was between its check and its read: %v; onRecv got %d chunk(s), X's bytes among them: %v; X (fd %d) reads %q (err %v)",
		overtaken, len(chunks), stole, x, got, rerr)
	if overtaken {
		t.Errorf("UnregisterConn(A) returned while the reader was between its closed check and its read: the two are not one critical section")
	}
	if stole {
		t.Errorf("A's onRecv got X's bytes: the reader read A's number after UnregisterConn(A) returned and X took it")
	}
	if got != c784Stolen {
		t.Errorf("X, which took A's number, reads %q (err %v), want %q: the reader consumed X's bytes", got, rerr, c784Stolen)
	}
	if len(chunks) != 1 || string(chunks[0]) != "AAAA" {
		t.Errorf("A's onRecv got %q, want exactly A's own \"AAAA\" (the read UnregisterConn waited for is still delivered)", chunks)
	}
}
