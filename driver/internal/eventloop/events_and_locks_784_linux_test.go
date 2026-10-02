//go:build linux

package eventloop

// celeris#784, review of #843: three more properties of the conn teardown.
//
//   - A WriteAndPoll* call's EPOLL_CTL_MODs (the step-2 EPOLLIN mask and the
//     step-5 re-arm) are issued by number, so they too must stop once the
//     conn is torn down: by then the number can be another conn's.
//   - A read of a conn must not wait for a flush of that conn. The worker
//     goroutine reads for every conn on it.
//   - A teardown marks the conn closed before the conn leaves the worker's
//     map. An UnregisterConn that misses returns ErrUnknownFD, and its caller
//     closes the fd at once.

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine"
)

// c784Lookup returns the worker's driverConn for fd, or nil.
func c784Lookup(w *worker, fd int) *driverConn {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.conns[fd]
}

// c784Drain reads fd (non-blocking) until it has want bytes or d has passed,
// and returns what it read.
func c784Drain(fd, want int, d time.Duration) []byte {
	got := make([]byte, 0, want)
	buf := make([]byte, 64<<10)
	deadline := time.Now().Add(d)
	for len(got) < want && time.Now().Before(deadline) {
		n, err := unix.Read(fd, buf)
		if n > 0 {
			got = append(got, buf[:n]...)
			continue
		}
		if err != nil && err != unix.EAGAIN {
			break
		}
		time.Sleep(time.Millisecond)
	}
	return got
}

// TestWriteAndPollLeavesTheConnThatTakesTheNumberAlone784: a WriteAndPoll*
// call masks EPOLLIN on its conn before it reads (step 2) and re-arms it after
// (step 5), both with EPOLL_CTL_MOD on the conn's number. Here the conn A is
// torn down while the call waits for recvMu, and a new conn B registers A's
// number on the same worker with a flush pending, so B has EPOLLOUT armed.
// Neither MOD may land on B: the mask would leave B without EPOLLIN, and
// either MOD would drop B's EPOLLOUT, so the rest of B's flush would never be
// sent.
//
// The worker is parked in A's onRecv and holds A's recvMu. The call's write
// has reached A's peer, so the call is past its own closed check and cannot
// reach its mask until the worker lets go of recvMu. The worker is let go only
// after A is unregistered and closed and B is registered on A's number with
// its flush pending.
func TestWriteAndPollLeavesTheConnThatTakesTheNumberAlone784(t *testing.T) {
	type call func(w *worker, fd int, rbuf []byte) (bool, error)
	nop := func([]byte) {}
	calls := []struct {
		name string
		call call
	}{
		{"WriteAndPoll", func(w *worker, fd int, rbuf []byte) (bool, error) {
			return w.WriteAndPoll(fd, []byte("q"), rbuf, nop)
		}},
		{"WriteAndPollBusy", func(w *worker, fd int, rbuf []byte) (bool, error) {
			return w.WriteAndPollBusy(fd, []byte("q"), rbuf, nop)
		}},
		{"WriteAndPollMulti", func(w *worker, fd int, rbuf []byte) (bool, error) {
			return w.WriteAndPollMulti(fd, []byte("q"), rbuf, nop, func() bool { return false }, nil)
		}},
	}
	for _, tc := range calls {
		t.Run(tc.name, func(t *testing.T) {
			l, err := New(1)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() { _ = l.Close() })
			w := l.WorkerLoop(0).(*worker)

			a, aPeer := socketPair(t)
			t.Cleanup(func() { _ = unix.Close(aPeer) })
			entered := make(chan struct{}, 1)
			release := make(chan struct{})
			var once sync.Once
			letGo := func() { once.Do(func() { close(release) }) }
			t.Cleanup(letGo)
			first := true
			if err := w.RegisterConn(a, func([]byte) {
				if first {
					first = false
					entered <- struct{}{}
					<-release
				}
			}, func(error) {}); err != nil {
				t.Fatalf("RegisterConn(A): %v", err)
			}
			c784Queue(t, aPeer, []byte("Z"))
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("the worker never entered A's onRecv")
			}

			done := make(chan error, 1)
			go func() {
				_, err := tc.call(w, a, make([]byte, 1024))
				done <- err
			}()
			if got := string(c784Drain(aPeer, 1, 5*time.Second)); got != "q" {
				t.Fatalf("A's peer read %q, want \"q\": the call's write never happened", got)
			}

			if err := w.UnregisterConn(a); err != nil {
				t.Fatalf("UnregisterConn(A): %v", err)
			}
			if err := unix.Close(a); err != nil {
				t.Fatalf("close A: %v", err)
			}
			b, bPeer := c784TakeNumber(t, a, true)
			bRecv := make(chan []byte, 8)
			if err := w.RegisterConn(b, func(p []byte) {
				bRecv <- append([]byte(nil), p...)
			}, func(error) {}); err != nil {
				t.Fatalf("RegisterConn(B) on A's number: %v", err)
			}
			t.Cleanup(func() { _ = w.UnregisterConn(b) })

			// More than the socket takes at once: the rest waits for EPOLLOUT.
			flush := bytes.Repeat([]byte{'B'}, 1<<20)
			if err := w.Write(b, flush); err != nil {
				t.Fatalf("Write(B): %v", err)
			}
			cb := c784Lookup(w, b)
			if cb == nil {
				t.Fatal("B is not registered")
			}
			cb.mu.Lock()
			armed, queued := cb.epollOut, len(cb.writeBuf)-cb.writePos
			cb.mu.Unlock()
			if !armed || queued == 0 {
				t.Fatalf("B's flush was taken whole (EPOLLOUT armed %v, %d bytes queued): the test cannot see a MOD drop B's EPOLLOUT", armed, queued)
			}

			letGo()
			var cerr error
			select {
			case cerr = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the call did not return within 5 s")
			}

			// B's peer drains, and the worker must send the rest on EPOLLOUT.
			got := c784Drain(bPeer, len(flush), 5*time.Second)
			c784Queue(t, bPeer, []byte("yo"))
			var gotIn []byte
			select {
			case gotIn = <-bRecv:
			case <-time.After(2 * time.Second):
			}
			t.Logf("C784 %s mask: returned %v; B's peer got %d of %d bytes (%d queued behind EPOLLOUT); B's onRecv got %q",
				tc.name, cerr, len(got), len(flush), queued, gotIn)
			if !errors.Is(cerr, engine.ErrUnknownFD) {
				t.Errorf("%s returned %v for a conn torn down under it, want engine.ErrUnknownFD", tc.name, cerr)
			}
			if !bytes.Equal(got, flush) {
				t.Errorf("B's peer got %d of B's %d bytes within 5 s: B's EPOLLOUT was dropped by a MOD on its number from A's %s", len(got), len(flush), tc.name)
			}
			if string(gotIn) != "yo" {
				t.Errorf("B's onRecv got %q within 2 s, want \"yo\": B's EPOLLIN was masked by A's %s", gotIn, tc.name)
			}
		})
	}
}

// TestWorkerReadsAConnWhileItIsBeingFlushed784: a read of a conn must not
// wait for a flush of that conn. flushLocked holds c.mu across its whole
// write(2) loop (Write, WriteAndPoll* step 1, the worker's drainOne), and the
// worker goroutine reads for every conn on it, so a read that took c.mu would
// hold up the whole worker for as long as one conn's flush runs. The test
// holds A's c.mu, as a flush in progress does, and makes A and then B
// readable. The worker must deliver both while the lock is held.
func TestWorkerReadsAConnWhileItIsBeingFlushed784(t *testing.T) {
	l, err := New(1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	w := l.WorkerLoop(0).(*worker)
	a, aPeer := socketPair(t)
	b, bPeer := socketPair(t)
	aGot := make(chan struct{}, 4)
	bGot := make(chan struct{}, 4)
	if err := w.RegisterConn(a, func([]byte) { aGot <- struct{}{} }, func(error) {}); err != nil {
		t.Fatalf("RegisterConn(A): %v", err)
	}
	if err := w.RegisterConn(b, func([]byte) { bGot <- struct{}{} }, func(error) {}); err != nil {
		t.Fatalf("RegisterConn(B): %v", err)
	}
	t.Cleanup(func() {
		_ = w.UnregisterConn(a)
		_ = w.UnregisterConn(b)
		for _, fd := range []int{a, aPeer, b, bPeer} {
			_ = unix.Close(fd)
		}
	})
	ca := c784Lookup(w, a)
	if ca == nil {
		t.Fatal("A is not registered")
	}

	ca.mu.Lock()
	held := true
	unlock := func() {
		if held {
			held = false
			ca.mu.Unlock()
		}
	}
	t.Cleanup(unlock) // runs first: before the unregisters and l.Close
	c784Queue(t, aPeer, []byte{'a'})
	c784Queue(t, bPeer, []byte{'b'})
	servedA, servedB := false, false
	deadline := time.After(5 * time.Second)
	for !servedA || !servedB {
		select {
		case <-aGot:
			servedA = true
			continue
		case <-bGot:
			servedB = true
			continue
		case <-deadline:
		}
		break
	}
	unlock()
	t.Logf("C784 flush-lock: while A's c.mu was held, the worker delivered A: %v, B: %v", servedA, servedB)
	if !servedA {
		t.Errorf("the worker did not deliver A's byte within 5 s while A's flush lock was held: a read waits for the conn's flush")
	}
	if !servedB {
		t.Errorf("the worker did not deliver B's byte within 5 s while A's flush lock was held: every conn on the worker waits for one conn's flush")
	}
}

// TestTeardownMarksTheConnClosedBeforeItLeavesTheWorker784: a teardown must
// set the conn's closed flag before it removes the conn from the worker's map.
// Once the conn is out of the map, an UnregisterConn of its number returns
// ErrUnknownFD and the caller closes the fd; a reader that has not seen closed
// yet would then read the number. The test holds the conn's c.mu, which the
// teardown needs to set closed, starts the teardown, and checks for 500 ms
// that the conn stays in the map.
func TestTeardownMarksTheConnClosedBeforeItLeavesTheWorker784(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start func(w *worker, fd, peer int, unregErr chan<- error)
	}{
		// The owner's UnregisterConn.
		{"UnregisterConn", func(w *worker, fd, _ int, unregErr chan<- error) {
			go func() { unregErr <- w.UnregisterConn(fd) }()
		}},
		// The worker reads the peer's EOF and tears the conn down (errorClose).
		{"PeerEOF", func(_ *worker, _, peer int, _ chan<- error) {
			_ = unix.Close(peer)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, err := New(1)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() { _ = l.Close() })
			w := l.WorkerLoop(0).(*worker)
			a, aPeer := socketPair(t)
			t.Cleanup(func() { _ = unix.Close(a) })
			if tc.name != "PeerEOF" {
				t.Cleanup(func() { _ = unix.Close(aPeer) })
			}
			fired := make(chan error, 2)
			if err := w.RegisterConn(a, func([]byte) {}, func(err error) { fired <- err }); err != nil {
				t.Fatalf("RegisterConn(A): %v", err)
			}
			c := c784Lookup(w, a)
			if c == nil {
				t.Fatal("A is not registered")
			}

			unregErr := make(chan error, 1)
			c.mu.Lock()
			tc.start(w, a, aPeer, unregErr)
			left := false
			for deadline := time.Now().Add(500 * time.Millisecond); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
				if c784Lookup(w, a) != c {
					left = true
					break
				}
			}
			markedWhileOut := c.closed // c.mu is held: the flag cannot change under us
			c.mu.Unlock()

			select {
			case <-fired:
			case <-time.After(5 * time.Second):
				t.Fatal("onClose did not fire within 5 s of the release: the teardown never ran")
			}
			if tc.name == "UnregisterConn" {
				select {
				case err := <-unregErr:
					if err != nil {
						t.Errorf("UnregisterConn(A): %v", err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("UnregisterConn(A) did not return within 5 s of the release")
				}
			}
			gone := c784Lookup(w, a) == nil
			t.Logf("C784 teardown order %s: the conn left the map while its closed flag could not be set: %v (closed then: %v); out of the map after the release: %v",
				tc.name, left, markedWhileOut, gone)
			if left && !markedWhileOut {
				t.Errorf("%s removed the conn from the worker's map before it marked the conn closed: an UnregisterConn that misses lets its caller close the fd while a reader can still read the number", tc.name)
			}
			if !gone {
				t.Errorf("the conn is still in the worker's map after %s finished", tc.name)
			}
		})
	}
}
