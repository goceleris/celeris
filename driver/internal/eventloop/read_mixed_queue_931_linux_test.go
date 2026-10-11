//go:build linux

package eventloop

// celeris#931, review round 3: the queue that holds both kinds of turn.

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestAQueueWithABudgetStoppedConnAndABusyTurnIsNotWaitedOn931: A's peer keeps
// A's socket full and A's onRecv is slow, so A is queued after every turn with
// bytes left. B's owner holds B's recvMu (the test, standing in for a
// WriteAndPoll* call) while B's peer sends a byte, so B's turn finds the lock
// held and is queued busy. Each round the queue holds one turn of each kind.
// The worker must not wait in epoll_wait then: A's bytes are owed now, and
// edge-triggered epoll will not report them again. Only a queue of busy turns
// alone waits busyRetryMs. A change that waits whenever any turn is busy would
// put up to a millisecond between A's turns. When the test lets go of B's
// recvMu, B's byte must reach its onRecv.
func TestAQueueWithABudgetStoppedConnAndABusyTurnIsNotWaitedOn931(t *testing.T) {
	var wp atomic.Pointer[worker]
	var mixedWaits, wrongTimeouts atomic.Int64
	testHookEpollWait = func(queued, timeoutMs int) {
		w := wp.Load()
		if w == nil || queued < 2 {
			return
		}
		busy, owed := 0, 0
		for _, c := range w.readQ { // on the worker's goroutine, the one that owns readQ
			if c.readBusy {
				busy++
			} else {
				owed++
			}
		}
		if busy > 0 && owed > 0 {
			mixedWaits.Add(1)
			if timeoutMs != 0 {
				wrongTimeouts.Add(1)
			}
		}
	}
	t.Cleanup(func() { testHookEpollWait = nil }) // runs last, after Close
	l, err := New(1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	w := l.WorkerLoop(0).(*worker)
	wp.Store(w)

	a, aPeer := socketPair(t)
	b, bPeer := socketPair(t)
	t.Cleanup(func() {
		_ = w.UnregisterConn(a)
		_ = w.UnregisterConn(b)
		for _, fd := range []int{a, aPeer, b, bPeer} {
			_ = unix.Close(fd)
		}
	})
	aFirst := make(chan struct{}, 1)
	var aGot atomic.Int64
	if err := w.RegisterConn(a, func(p []byte) {
		if aGot.Add(int64(len(p))) == int64(len(p)) {
			aFirst <- struct{}{}
		}
		time.Sleep(100 * time.Microsecond)
	}, func(error) {}); err != nil {
		t.Fatalf("RegisterConn(A): %v", err)
	}
	bGot := make(chan struct{}, 1)
	if err := w.RegisterConn(b, func([]byte) {
		select {
		case bGot <- struct{}{}:
		default:
		}
	}, func(error) {}); err != nil {
		t.Fatalf("RegisterConn(B): %v", err)
	}
	w.mu.RLock()
	bc := w.conns[b]
	w.mu.RUnlock()

	stop := make(chan struct{})
	var feeder sync.WaitGroup
	feeder.Add(1)
	go func() {
		defer feeder.Done()
		chunk := make([]byte, 64<<10)
		for {
			select {
			case <-stop:
				return
			default:
			}
			_, err := unix.Write(aPeer, chunk)
			if err == unix.EAGAIN {
				time.Sleep(10 * time.Microsecond)
				continue
			}
			if err != nil {
				return
			}
		}
	}()
	stopFeeder := sync.OnceFunc(func() { close(stop); feeder.Wait() })
	t.Cleanup(stopFeeder)
	select {
	case <-aFirst:
	case <-time.After(5 * time.Second):
		t.Fatal("A's onRecv never ran: the test cannot load the worker")
	}
	bc.recvMu.Lock() // B's owner is in a call
	locked := true
	unlockB := func() {
		if locked {
			locked = false
			bc.recvMu.Unlock()
		}
	}
	t.Cleanup(unlockB)
	c784Queue(t, bPeer, []byte{'b'})
	const want = 50
	for dl := time.Now().Add(5 * time.Second); mixedWaits.Load() < want && time.Now().Before(dl); {
		time.Sleep(time.Millisecond)
	}
	stopFeeder()
	mixed, wrong := mixedWaits.Load(), wrongTimeouts.Load()
	unlockB()
	delivered := false
	select {
	case <-bGot:
		delivered = true
	case <-time.After(3 * time.Second):
	}
	t.Logf("C931 mixed queue: %d epoll_waits with one budget-stopped and one busy turn queued, of which with a timeout other than 0: %d; B's byte reached its onRecv after the call let go: %v",
		mixed, wrong, delivered)
	if mixed < want {
		t.Fatalf("only %d epoll_waits found both kinds of turn queued, want %d: the test did not drive the queue", mixed, want)
	}
	if wrong != 0 {
		t.Errorf("%d of the worker's %d epoll_waits with a conn owed bytes and another conn's busy turn queued waited: the bytes of the first are owed now", wrong, mixed)
	}
	if !delivered {
		t.Errorf("B's byte never reached its onRecv after the call let go of recvMu")
	}
}
