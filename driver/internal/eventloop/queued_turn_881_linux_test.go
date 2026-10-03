//go:build linux

package eventloop

// celeris#881, review round 1 of #934: the read queue's turns and the
// WriteAndPoll* calls that hold a conn's recvMu, and the worker's epoll_wait
// timeout while a conn is queued.

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// c881Turn is one full read turn of the worker's 16 KiB read buffer.
const c881Turn = readBudget * (16 << 10)

// c881Parker is a conn whose first onRecv parks the worker until the test
// releases it.
type c881Parker struct {
	fd, peer int
	entered  chan struct{}
	release  chan struct{}
	once     sync.Once
}

func c881NewParker(t *testing.T, w *worker) *c881Parker {
	t.Helper()
	p := &c881Parker{entered: make(chan struct{}, 1), release: make(chan struct{})}
	p.fd, p.peer = socketPair(t)
	first := true
	if err := w.RegisterConn(p.fd, func([]byte) {
		if first {
			first = false
			p.entered <- struct{}{}
			<-p.release
		}
	}, func(error) {}); err != nil {
		t.Fatalf("RegisterConn(parker): %v", err)
	}
	t.Cleanup(func() {
		p.let()
		_ = w.UnregisterConn(p.fd)
		_ = unix.Close(p.fd)
		_ = unix.Close(p.peer)
	})
	return p
}

func (p *c881Parker) let() { p.once.Do(func() { close(p.release) }) }

func c881Wait(t *testing.T, c <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(5 * time.Second):
		t.Fatalf("timeout: %s", what)
	}
}

// c881QueueX parks the worker in P's onRecv right after a read turn of X
// that used up its budget, so X is queued and nothing is left in it. X is a
// pipe; xw is its write end. The worker is parked in Q while X is filled
// with exactly one turn's worth and P is sent a byte, so the next batch is
// X's event, then P's. It returns P, still parked, once X's turn is done.
func c881QueueX(t *testing.T, w *worker, x, xw int, xBytes *atomic.Int64) *c881Parker {
	t.Helper()
	q := c881NewParker(t, w)
	p := c881NewParker(t, w)
	c784Queue(t, q.peer, []byte("q"))
	c881Wait(t, q.entered, "Q parked the worker")
	if n, err := unix.Write(xw, make([]byte, c881Turn)); n != c881Turn || err != nil {
		t.Fatalf("fill X: wrote %d of %d (%v)", n, c881Turn, err)
	}
	c784Queue(t, p.peer, []byte("p"))
	q.let()
	c881Wait(t, p.entered, "P parked the worker")
	if got := xBytes.Load(); got != c881Turn {
		t.Fatalf("X's onRecv got %d of %d bytes before P parked the worker: the batch was not X's event, then P's", got, c881Turn)
	}
	// The worker set readQueued before it parked in P, and the receive from
	// p.entered orders this read after that write.
	w.mu.RLock()
	c := w.conns[x]
	w.mu.RUnlock()
	if c == nil || !c.readQueued {
		t.Fatalf("X is not queued after a read turn that used up its budget (conn found: %v)", c != nil)
	}
	return p
}

// c881Pipe returns a non-blocking pipe (read end, write end) sized to 1 MiB.
func c881Pipe(t *testing.T) (int, int) {
	t.Helper()
	var p [2]int
	if err := unix.Pipe2(p[:], unix.O_NONBLOCK|unix.O_CLOEXEC); err != nil {
		t.Fatalf("pipe2: %v", err)
	}
	if _, err := unix.FcntlInt(uintptr(p[1]), unix.F_SETPIPE_SZ, 1<<20); err != nil {
		t.Fatalf("F_SETPIPE_SZ: %v", err)
	}
	return p[0], p[1]
}

// TestAQueuedTurnDoesNotWaitForAPollingCaller881: a conn's queued read turn
// finds the conn's recvMu held by a WriteAndPoll* call. The worker must not
// wait for the call: every other conn on the worker would wait with it, for
// up to the call's whole poll loop. The construction is deterministic:
//   - X is queued, empty, and the worker parked in P (c881QueueX).
//   - X's owner calls WriteAndPollMulti(X). It takes X's recvMu and masks
//     EPOLLIN, and a one-byte reply is written into X from inside its first
//     read (testHookBeforeRead), after the mask, so the worker collects no
//     event of X. The call's onRecv for the reply holds the call, and X's
//     recvMu, until B is served, for 5 s at most.
//   - R is sent a byte, then P is released. The next round's batch is R's
//     event, whose onRecv sends B a byte; then the round serves X's queued
//     turn. B's event is collected by the round after that.
//
// B must be served while the call still holds X's recvMu. A worker that
// waits for recvMu in X's queued turn serves B only once the call gives up.
func TestAQueuedTurnDoesNotWaitForAPollingCaller881(t *testing.T) {
	x, xw := c881Pipe(t)
	var armed atomic.Bool
	var replies atomic.Int32
	testHookBeforeRead = func(fd int) {
		if fd == x && armed.CompareAndSwap(true, false) {
			if n, err := unix.Write(xw, []byte{'r'}); n == 1 && err == nil {
				replies.Add(1)
			}
		}
	}
	t.Cleanup(func() { testHookBeforeRead = nil }) // runs last, after Close
	l, err := New(1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	w := l.WorkerLoop(0).(*worker)

	var xBytes atomic.Int64
	if err := w.RegisterConn(x, func(b []byte) { xBytes.Add(int64(len(b))) }, func(error) {}); err != nil {
		t.Fatalf("RegisterConn(X): %v", err)
	}
	t.Cleanup(func() { _ = w.UnregisterConn(x); _ = unix.Close(x); _ = unix.Close(xw) })
	b, bPeer := socketPair(t)
	bServed := make(chan struct{})
	var bOnce sync.Once
	if err := w.RegisterConn(b, func([]byte) { bOnce.Do(func() { close(bServed) }) }, func(error) {}); err != nil {
		t.Fatalf("RegisterConn(B): %v", err)
	}
	t.Cleanup(func() { _ = w.UnregisterConn(b); _ = unix.Close(b); _ = unix.Close(bPeer) })
	r, rPeer := socketPair(t)
	var rOnce sync.Once
	if err := w.RegisterConn(r, func([]byte) {
		rOnce.Do(func() {
			if _, err := unix.Write(bPeer, []byte{'b'}); err != nil {
				t.Errorf("write B's byte: %v", err)
			}
		})
	}, func(error) {}); err != nil {
		t.Fatalf("RegisterConn(R): %v", err)
	}
	t.Cleanup(func() { _ = w.UnregisterConn(r); _ = unix.Close(r); _ = unix.Close(rPeer) })

	p := c881QueueX(t, w, x, xw, &xBytes)

	holding := make(chan struct{})
	callDone := make(chan struct{})
	var servedWhileHeld atomic.Bool
	var held time.Duration
	go func() {
		defer close(callDone)
		first := true
		armed.Store(true)
		_, _ = w.WriteAndPollMulti(x, nil, make([]byte, 1024), func(got []byte) {
			if first && len(got) > 0 {
				first = false
				close(holding)
				s := time.Now()
				select {
				case <-bServed:
					servedWhileHeld.Store(true)
				case <-time.After(5 * time.Second):
				}
				held = time.Since(s)
			}
		}, func() bool { return true }, nil)
	}()
	c881Wait(t, holding, "the WriteAndPollMulti call read X's reply (it holds X's recvMu)")
	if n := replies.Load(); n != 1 {
		t.Fatalf("X's reply was written %d times, want 1", n)
	}
	c784Queue(t, rPeer, []byte("r"))
	p.let()
	select {
	case <-callDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the WriteAndPollMulti call did not return within 10 s")
	}
	served := false
	select {
	case <-bServed:
		served = true
	case <-time.After(5 * time.Second):
	}
	t.Logf("C881 queued turn vs caller: B served while the call held X's recvMu: %v (the call held it %v); B served at all: %v",
		servedWhileHeld.Load(), held, served)
	if !servedWhileHeld.Load() {
		t.Errorf("B was not served while the WriteAndPollMulti call held X's recvMu (served after it let go: %v): the worker waited for recvMu in X's queued turn", served)
	}
}

// TestAQueuedTurnKeepsAnEventCollectedWhileACallerPolls881: the worker drops
// a queued turn whose recvMu a WriteAndPoll* call holds, because the call
// reads the conn and its re-arm makes epoll report what is left. It must not
// drop a turn that an event of the conn was merged into: that event can be
// the re-arm's own report, collected while the call still holds recvMu, and
// no other event will come for the bytes it reports. X is queued, empty, and
// the worker parked in P (c881QueueX). X's owner calls WriteAndPollMulti(X):
// X is empty, so the call polls once and gives up. After its re-arm, with
// recvMu still held (testHookAfterRearm), a byte z is written into X, and P
// is released. The next round collects X's event for z, merges it into X's
// queued turn, and finds recvMu held (testHookQueuedTurnBusy); only then
// does the call let go. z must reach X's onRecv.
func TestAQueuedTurnKeepsAnEventCollectedWhileACallerPolls881(t *testing.T) {
	x, xw := c881Pipe(t)
	busy := make(chan bool, 1)
	testHookQueuedTurnBusy = func(fd int, fresh bool) {
		if fd == x {
			select {
			case busy <- fresh:
			default:
			}
		}
	}
	t.Cleanup(func() { testHookQueuedTurnBusy = nil }) // runs last, after Close
	l, err := New(1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	w := l.WorkerLoop(0).(*worker)

	var xBytes atomic.Int64
	gotZ := make(chan struct{})
	var zOnce sync.Once
	if err := w.RegisterConn(x, func(b []byte) {
		xBytes.Add(int64(len(b)))
		if len(b) > 0 && b[len(b)-1] == 'z' {
			zOnce.Do(func() { close(gotZ) })
		}
	}, func(error) {}); err != nil {
		t.Fatalf("RegisterConn(X): %v", err)
	}
	t.Cleanup(func() { _ = w.UnregisterConn(x); _ = unix.Close(x); _ = unix.Close(xw) })

	p := c881QueueX(t, w, x, xw, &xBytes)

	rearmed := make(chan struct{})
	var sawBusy, fresh atomic.Bool
	var rearms atomic.Int32
	testHookAfterRearm = func(fd int) {
		if fd != x || rearms.Add(1) != 1 {
			return
		}
		if _, err := unix.Write(xw, []byte{'z'}); err != nil {
			t.Errorf("write z: %v", err)
		}
		close(rearmed)
		select {
		case f := <-busy:
			sawBusy.Store(true)
			fresh.Store(f)
		case <-time.After(5 * time.Second):
		}
	}
	callDone := make(chan struct{})
	go func() {
		defer close(callDone)
		_, _ = w.WriteAndPollMulti(x, nil, make([]byte, 1024), func([]byte) {}, func() bool { return true }, nil)
	}()
	c881Wait(t, rearmed, "the WriteAndPollMulti call re-armed X")
	p.let()
	select {
	case <-callDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the WriteAndPollMulti call did not return within 10 s")
	}
	testHookAfterRearm = nil
	delivered := false
	select {
	case <-gotZ:
		delivered = true
	case <-time.After(5 * time.Second):
	}
	t.Logf("C881 queued turn keeps event: X's queued turn found recvMu held: %v (an event merged into it: %v); z reached X's onRecv: %v",
		sawBusy.Load(), fresh.Load(), delivered)
	if !sawBusy.Load() {
		t.Fatal("X's queued turn never found X's recvMu held: the test did not drive the window")
	}
	if !fresh.Load() {
		t.Fatal("X's queued turn found recvMu held with no event merged into it: the round did not collect z's event while X was queued")
	}
	if !delivered {
		t.Errorf("z, written after the call's re-arm while it held X's recvMu, never reached X's onRecv: the worker dropped the queued turn its event was merged into")
	}
}

// TestAWorkerWithAQueuedConnDoesNotWaitInEpollWait881: while a conn is owed a
// read turn, the worker calls epoll_wait with timeout 0. Edge-triggered epoll
// reports nothing new for bytes already in the socket, so a wait with the
// idle timeout (100 ms) would delay each further turn of a backlogged conn by
// a full timeout whenever nothing else arrives. A pipe filled with 1 MiB (four
// turns) is registered; the test records the timeout of every epoll_wait the
// worker makes while a conn is queued (testHookEpollWait).
func TestAWorkerWithAQueuedConnDoesNotWaitInEpollWait881(t *testing.T) {
	var queuedWaits, blockingWaits atomic.Int64
	testHookEpollWait = func(queued, timeoutMs int) {
		if queued > 0 {
			queuedWaits.Add(1)
			if timeoutMs != 0 {
				blockingWaits.Add(1)
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

	x, xw := c881Pipe(t)
	t.Cleanup(func() { _ = unix.Close(x); _ = unix.Close(xw) })
	const size = 1 << 20
	if n, err := unix.Write(xw, make([]byte, size)); n != size || err != nil {
		t.Fatalf("fill the pipe: wrote %d of %d (%v)", n, size, err)
	}
	var got atomic.Int64
	all := make(chan struct{})
	if err := w.RegisterConn(x, func(b []byte) {
		if got.Add(int64(len(b))) == size {
			close(all)
		}
	}, func(error) {}); err != nil {
		t.Fatalf("RegisterConn: %v", err)
	}
	t.Cleanup(func() { _ = w.UnregisterConn(x) })
	delivered := false
	select {
	case <-all:
		delivered = true
	case <-time.After(5 * time.Second):
	}
	q, bw := queuedWaits.Load(), blockingWaits.Load()
	t.Logf("C881 queued wait: %d of %d bytes delivered; epoll_waits with a conn queued: %d, of which with a nonzero timeout: %d",
		got.Load(), size, q, bw)
	if !delivered {
		t.Fatalf("X's onRecv got %d of %d bytes within 5 s", got.Load(), size)
	}
	if q < 3 {
		t.Fatalf("the worker made %d epoll_waits with a conn queued, want at least 3 (four turns): the test did not queue the conn", q)
	}
	if bw != 0 {
		t.Errorf("the worker made %d of its %d epoll_waits with a conn queued with a nonzero timeout: a backlogged conn waits a full timeout per turn when nothing else arrives", bw, q)
	}
}
