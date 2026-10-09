//go:build linux

package eventloop

// celeris#881, review rounds 1 and 2 of #934: the read queue's turns and the
// WriteAndPoll* calls that hold a conn's recvMu, the worker's epoll_wait
// timeout while a conn is queued, and two conns queued at once.

import (
	"fmt"
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
	// However the test ends, the hook is cleared once the call that runs it
	// has returned (the hook itself waits 5 s at most).
	t.Cleanup(func() {
		select {
		case <-callDone:
		case <-time.After(10 * time.Second):
		}
		testHookAfterRearm = nil
	})
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

// c881Sink receives one conn's bytes, which carry the pattern
// byte((offset+seed)%251), and records the first one out of order.
type c881Sink struct {
	mu                             sync.Mutex
	seed, got, bad, reads, atClose int
	all                            chan struct{}
	closed                         chan error
	onRead                         func(reads int) // on the worker, after each read's bytes are counted
}

func c881NewSink(seed int) *c881Sink {
	return &c881Sink{seed: seed, bad: -1, atClose: -1, all: make(chan struct{}), closed: make(chan error, 1)}
}

func (s *c881Sink) onRecv(b []byte) {
	s.mu.Lock()
	s.reads++
	r := s.reads
	for i, c := range b {
		if s.bad < 0 && c != byte((s.got+i+s.seed)%251) {
			s.bad = s.got + i
		}
	}
	s.got += len(b)
	if s.got == 1<<20 {
		close(s.all)
	}
	h := s.onRead
	s.mu.Unlock()
	if h != nil {
		h(r)
	}
}

func (s *c881Sink) onClose(err error) {
	s.mu.Lock()
	s.atClose = s.got
	s.mu.Unlock()
	s.closed <- err
}

func c881Pattern(seed int) []byte {
	p := make([]byte, 1<<20)
	for i := range p {
		p[i] = byte((i + seed) % 251)
	}
	return p
}

// TestTwoBackloggedConnsGetEveryByte881: two conns are backlogged at once, so
// serveReadQ must carry a conn queued during a round's batch over to the next
// round (the tail of readQ) while it serves a conn owed from an earlier round.
// X1 is a pipe filled with 1 MiB (64 reads, four turns) before it is
// registered; X2 is a pipe registered empty. At X1's third read, X1's onRecv
// fills X2 with 1 MiB, so X2's first event is in the batch of the round that
// owes X1 its second turn: X2's turn uses up its budget and queues X2 while X1
// is still queued (the test checks this on the worker, at X2's last read of
// that turn). Every byte of both must arrive in order, and each conn's
// onClose(nil) must fire after its last byte once its write end is closed. A
// worker that dropped the conns queued during a batch would leave X2 queued
// for a turn that never comes, with one turn (256 KiB) of its bytes read.
func TestTwoBackloggedConnsGetEveryByte881(t *testing.T) {
	l, err := New(1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	w := l.WorkerLoop(0).(*worker)

	const size = 1 << 20
	x1, x1w := c881Pipe(t)
	x2, x2w := c881Pipe(t)
	t.Cleanup(func() { _ = unix.Close(x1); _ = unix.Close(x2) })
	s1, s2 := c881NewSink(0), c881NewSink(97)
	var fillErr error
	filled := make(chan struct{})
	s1.onRead = func(reads int) {
		if reads != 3 {
			return
		}
		if n, err := unix.Write(x2w, c881Pattern(97)); n != size || err != nil {
			fillErr = fmt.Errorf("wrote %d of %d (%v)", n, size, err)
		}
		close(filled)
	}
	var x1QueuedAtX2Queue atomic.Bool
	s2.onRead = func(reads int) {
		if reads == readBudget { // X2's last read of its first turn: readQ is the worker's
			if c := c784Lookup(w, x1); c != nil && c.readQueued {
				x1QueuedAtX2Queue.Store(true)
			}
		}
	}
	if err := w.RegisterConn(x2, s2.onRecv, s2.onClose); err != nil {
		t.Fatalf("RegisterConn(X2): %v", err)
	}
	t.Cleanup(func() { _ = w.UnregisterConn(x2) })
	if n, err := unix.Write(x1w, c881Pattern(0)); n != size || err != nil {
		t.Fatalf("fill X1: wrote %d of %d (%v)", n, size, err)
	}
	if err := w.RegisterConn(x1, s1.onRecv, s1.onClose); err != nil {
		t.Fatalf("RegisterConn(X1): %v", err)
	}
	t.Cleanup(func() { _ = w.UnregisterConn(x1) })
	c881Wait(t, filled, "X1's third read, which fills X2")
	if fillErr != nil {
		t.Fatalf("fill X2: %v", fillErr)
	}

	d1, d2 := false, false
	all1, all2 := s1.all, s2.all
	deadline := time.After(5 * time.Second)
wait:
	for !d1 || !d2 {
		select {
		case <-all1:
			d1, all1 = true, nil
		case <-all2:
			d2, all2 = true, nil
		case <-deadline:
			break wait
		}
	}
	_ = unix.Close(x1w)
	_ = unix.Close(x2w)
	var c1, c2 error
	f1, f2 := false, false
	closeDeadline := time.After(5 * time.Second)
closing:
	for !f1 || !f2 {
		select {
		case c1 = <-s1.closed:
			f1 = true
		case c2 = <-s2.closed:
			f2 = true
		case <-closeDeadline:
			break closing
		}
	}
	s1.mu.Lock()
	g1, b1, r1, a1 := s1.got, s1.bad, s1.reads, s1.atClose
	s1.mu.Unlock()
	s2.mu.Lock()
	g2, b2, r2, a2 := s2.got, s2.bad, s2.reads, s2.atClose
	s2.mu.Unlock()
	t.Logf("C881 two backlogs: X2 queued while X1 was queued: %v; X1 %d of %d bytes in %d reads (first wrong byte at %d; onClose fired %v (%v) at %d bytes); X2 %d of %d bytes in %d reads (first wrong byte at %d; onClose fired %v (%v) at %d bytes)",
		x1QueuedAtX2Queue.Load(), g1, size, r1, b1, f1, c1, a1, g2, size, r2, b2, f2, c2, a2)
	if !x1QueuedAtX2Queue.Load() {
		t.Fatal("X2 used up its first turn's budget while X1 was not queued: the test did not backlog two conns at once")
	}
	if !d1 || !d2 {
		t.Errorf("not every byte arrived within 5 s: X1 %d, X2 %d of %d each; a conn queued during a batch was not carried to the next round", g1, g2, size)
	}
	if b1 >= 0 || b2 >= 0 {
		t.Errorf("bytes out of order: X1's first wrong byte at %d, X2's at %d", b1, b2)
	}
	if !f1 || c1 != nil || a1 != size || !f2 || c2 != nil || a2 != size {
		t.Errorf("onClose: X1 fired %v (%v) at %d bytes, X2 fired %v (%v) at %d bytes; want onClose(nil) after the last byte of each", f1, c1, a1, f2, c2, a2)
	}
}
