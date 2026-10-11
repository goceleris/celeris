//go:build linux

package eventloop

// celeris#931: the worker must never wait on a conn's recvMu. A
// WriteAndPoll* call holds the lock for its whole poll loop (about 50 ms for
// WriteAndPollMulti), and every other conn on the worker waits for a worker
// that waits for the lock.

import (
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestAWorkerDoesNotWaitOnTheRecvMuOfAPollingCaller931: the worker is parked
// in P's onRecv with P's and then X's events collected. X's owner calls
// WriteAndPollMulti(X): its write reaches X's peer, it reads X's waiting byte,
// and it polls with isDone false, holding X's recvMu. P is let go, so the
// worker dispatches X's event. B, on the same worker, is sent one byte. B must
// be served while the call still holds X's recvMu (10 ms or more before the call returns).
func TestAWorkerDoesNotWaitOnTheRecvMuOfAPollingCaller931(t *testing.T) {
	l, err := New(1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	w := l.WorkerLoop(0).(*worker)

	type parker struct {
		fd, peer int
		entered  chan struct{}
		release  chan struct{}
		once     sync.Once
	}
	newParker := func() *parker {
		p := &parker{entered: make(chan struct{}, 1), release: make(chan struct{})}
		p.fd, p.peer = socketPair(t)
		t.Cleanup(func() {
			p.once.Do(func() { close(p.release) })
			_ = w.UnregisterConn(p.fd)
			_ = unix.Close(p.fd)
			_ = unix.Close(p.peer)
		})
		first := true
		if err := w.RegisterConn(p.fd, func([]byte) {
			if first {
				first = false
				p.entered <- struct{}{}
				<-p.release
			}
		}, func(error) {}); err != nil {
			t.Fatalf("RegisterConn: %v", err)
		}
		return p
	}
	wait := func(c chan struct{}, what string) {
		t.Helper()
		select {
		case <-c:
		case <-time.After(5 * time.Second):
			t.Fatalf("timeout: %s", what)
		}
	}
	q := newParker()
	p := newParker()
	x, xPeer := socketPair(t)
	b, bPeer := socketPair(t)
	t.Cleanup(func() {
		_ = w.UnregisterConn(x)
		_ = w.UnregisterConn(b)
		for _, fd := range []int{x, xPeer, b, bPeer} {
			_ = unix.Close(fd)
		}
	})
	if err := w.RegisterConn(x, func([]byte) {}, func(error) {}); err != nil {
		t.Fatal(err)
	}
	bGot := make(chan time.Time, 1)
	if err := w.RegisterConn(b, func([]byte) {
		select {
		case bGot <- time.Now():
		default:
		}
	}, func(error) {}); err != nil {
		t.Fatal(err)
	}

	c784Queue(t, q.peer, []byte("q"))
	wait(q.entered, "Q parked")
	c784Queue(t, p.peer, []byte("p"))
	c784Queue(t, xPeer, []byte("x")) // X's event, collected with P's
	q.once.Do(func() { close(q.release) })
	wait(p.entered, "P parked")

	callDone := make(chan time.Time, 1)
	callStart := time.Now()
	go func() {
		rbuf := make([]byte, 1024)
		_, _ = w.WriteAndPollMulti(x, []byte("Q"), rbuf, func([]byte) {}, func() bool { return false }, nil)
		callDone <- time.Now()
	}()
	if got := c784Drain(xPeer, 1, 5*time.Second); string(got) != "Q" {
		t.Fatalf("X's peer got %q: the call's write never happened", got)
	}
	time.Sleep(2 * time.Millisecond) // the call holds X's recvMu and polls
	p.once.Do(func() { close(p.release) })
	time.Sleep(1 * time.Millisecond) // the worker reaches X's event
	sent := time.Now()
	c784Queue(t, bPeer, []byte("b"))
	var at time.Time
	select {
	case at = <-bGot:
	case <-time.After(5 * time.Second):
		t.Fatal("B was not served within 5 s")
	}
	var end time.Time
	select {
	case end = <-callDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the WriteAndPollMulti call did not return within 10 s")
	}
	call := end.Sub(callStart)
	t.Logf("C931 dispatched event vs caller: WriteAndPollMulti on X took %v; B, on the same worker, was served %v after its byte, %v before the call returned",
		call, at.Sub(sent), end.Sub(at))
	if call < 30*time.Millisecond {
		t.Fatalf("the WriteAndPollMulti call took %v: it did not poll, so the test did not drive the window", call)
	}
	// The worker serves B within microseconds when it does not wait. Waiting
	// ends when the call lets go of recvMu, a moment before the call returns,
	// so a B served less than 10 ms before the end was held for the call.
	if end.Sub(at) < 10*time.Millisecond {
		t.Errorf("B was served %v after its byte and only %v before the WriteAndPollMulti call returned: the worker waited on X's recvMu for the caller", at.Sub(sent), end.Sub(at))
	}
}
