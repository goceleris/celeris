//go:build linux

package eventloop

// celeris#931: the worker must never wait on a conn's recvMu. A
// WriteAndPoll* call holds the lock for its whole poll loop (about 50 ms for
// WriteAndPollMulti), and every other conn on the worker waits for a worker
// that waits for the lock.

import (
	"sync"
	"sync/atomic"
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

// TestAnEventTurnThatFindsRecvMuHeldIsKeptAndRetried931: the turn for an event
// of a conn that was not queued finds the conn's recvMu held by a
// WriteAndPoll* call. The worker must not drop the turn (the event can be the
// re-arm's own report, and no other event would come for its bytes) and must
// not poll for it with timeout 0. The worker is parked in P. X's owner calls
// WriteAndPollMulti(X): X is empty, so the call polls once and gives up. After
// its re-arm, with recvMu still held (testHookAfterRearm), a byte z is written
// into X, and P is let go. The worker collects X's event for z and finds
// recvMu held (testHookEventTurnBusy). The call holds on until the worker has
// made three epoll_waits with the turn queued, every one with the busy
// timeout, and then lets go. z must reach X's onRecv.
func TestAnEventTurnThatFindsRecvMuHeldIsKeptAndRetried931(t *testing.T) {
	x, xPeer := socketPair(t)
	var recording atomic.Bool
	var waits, wrongTimeout atomic.Int64
	testHookEpollWait = func(queued, timeoutMs int) {
		if queued > 0 && recording.Load() {
			waits.Add(1)
			if timeoutMs != busyRetryMs {
				wrongTimeout.Add(1)
			}
		}
	}
	busy := make(chan struct{}, 1)
	testHookEventTurnBusy = func(fd int) {
		if fd == x {
			select {
			case busy <- struct{}{}:
			default:
			}
		}
	}
	t.Cleanup(func() { testHookEpollWait, testHookEventTurnBusy = nil, nil }) // runs last, after Close
	l, err := New(1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	w := l.WorkerLoop(0).(*worker)

	gotZ := make(chan struct{})
	var zOnce sync.Once
	if err := w.RegisterConn(x, func(b []byte) {
		if len(b) > 0 && b[len(b)-1] == 'z' {
			zOnce.Do(func() { close(gotZ) })
		}
	}, func(error) {}); err != nil {
		t.Fatalf("RegisterConn(X): %v", err)
	}
	t.Cleanup(func() { _ = w.UnregisterConn(x); _ = unix.Close(x); _ = unix.Close(xPeer) })
	p := c881NewParker(t, w)
	c784Queue(t, p.peer, []byte("p"))
	c881Wait(t, p.entered, "P parked the worker")

	var sawBusy atomic.Bool
	var rearms atomic.Int32
	testHookAfterRearm = func(fd int) {
		if fd != x || rearms.Add(1) != 1 {
			return
		}
		if _, err := unix.Write(xPeer, []byte{'z'}); err != nil {
			t.Errorf("write z: %v", err)
		}
		recording.Store(true)
		p.let()
		select {
		case <-busy:
			sawBusy.Store(true)
		case <-time.After(5 * time.Second):
			return
		}
		for dl := time.Now().Add(5 * time.Second); waits.Load() < 3 && time.Now().Before(dl); {
			time.Sleep(time.Millisecond)
		}
		recording.Store(false)
	}
	callDone := make(chan struct{})
	go func() {
		defer close(callDone)
		_, _ = w.WriteAndPollMulti(x, nil, make([]byte, 1024), func([]byte) {}, func() bool { return true }, nil)
	}()
	t.Cleanup(func() {
		select {
		case <-callDone:
		case <-time.After(15 * time.Second):
		}
		testHookAfterRearm = nil
	})
	select {
	case <-callDone:
	case <-time.After(15 * time.Second):
		t.Fatal("the WriteAndPollMulti call did not return within 15 s")
	}
	delivered := false
	select {
	case <-gotZ:
		delivered = true
	case <-time.After(2 * time.Second):
	}
	t.Logf("C931 event turn kept: the turn found recvMu held: %v; epoll_waits with it queued while the call held on: %d, of which with another timeout than %d ms: %d; z reached X's onRecv: %v",
		sawBusy.Load(), waits.Load(), busyRetryMs, wrongTimeout.Load(), delivered)
	if !sawBusy.Load() {
		t.Fatal("the turn for X's event never found X's recvMu held: the test did not drive the window")
	}
	if wrongTimeout.Load() != 0 {
		t.Errorf("%d of the worker's %d epoll_waits with the busy turn queued did not use the %d ms retry timeout: 0 spins, a longer one delays the retry", wrongTimeout.Load(), waits.Load(), busyRetryMs)
	}
	if !delivered {
		t.Errorf("z, written after the call's re-arm while it held X's recvMu, never reached X's onRecv: the worker dropped the turn its event was dispatched for")
	}
}

// TestAFreshQueuedTurnDoesNotWaitForAPollingCaller931: the queued turn of X
// has an event merged into it and finds X's recvMu held. The worker must keep
// the turn without waiting for the call (the 881 tests pin that it is kept):
// B, on the same worker, must be served while the call still holds X's
// recvMu. X is queued, empty, and the worker parked in P (c881QueueX). X's
// owner calls WriteAndPollMulti(X). After its re-arm, with recvMu held, z is
// written into X and P is let go; the next round merges X's event into its
// queued turn and finds recvMu held (testHookQueuedTurnBusy); then B is sent
// a byte, and the call lets go only when B is served or after 5 s.
func TestAFreshQueuedTurnDoesNotWaitForAPollingCaller931(t *testing.T) {
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
	b, bPeer := socketPair(t)
	bServed := make(chan struct{})
	var bOnce sync.Once
	if err := w.RegisterConn(b, func([]byte) { bOnce.Do(func() { close(bServed) }) }, func(error) {}); err != nil {
		t.Fatalf("RegisterConn(B): %v", err)
	}
	t.Cleanup(func() { _ = w.UnregisterConn(b); _ = unix.Close(b); _ = unix.Close(bPeer) })

	p := c881QueueX(t, w, x, xw, &xBytes)

	var sawBusy, fresh, servedWhileHeld atomic.Bool
	var rearms atomic.Int32
	testHookAfterRearm = func(fd int) {
		if fd != x || rearms.Add(1) != 1 {
			return
		}
		if _, err := unix.Write(xw, []byte{'z'}); err != nil {
			t.Errorf("write z: %v", err)
		}
		p.let()
		select {
		case f := <-busy:
			sawBusy.Store(true)
			fresh.Store(f)
		case <-time.After(5 * time.Second):
			return
		}
		c784Queue(t, bPeer, []byte{'b'})
		select {
		case <-bServed:
			servedWhileHeld.Store(true)
		case <-time.After(5 * time.Second):
		}
	}
	callDone := make(chan struct{})
	go func() {
		defer close(callDone)
		_, _ = w.WriteAndPollMulti(x, nil, make([]byte, 1024), func([]byte) {}, func() bool { return true }, nil)
	}()
	t.Cleanup(func() {
		select {
		case <-callDone:
		case <-time.After(15 * time.Second):
		}
		testHookAfterRearm = nil
	})
	select {
	case <-callDone:
	case <-time.After(15 * time.Second):
		t.Fatal("the WriteAndPollMulti call did not return within 15 s")
	}
	delivered := false
	select {
	case <-gotZ:
		delivered = true
	case <-time.After(2 * time.Second):
	}
	t.Logf("C931 fresh queued turn: found recvMu held: %v (event merged: %v); B served while the call held X's recvMu: %v; z reached X's onRecv: %v",
		sawBusy.Load(), fresh.Load(), servedWhileHeld.Load(), delivered)
	if !sawBusy.Load() || !fresh.Load() {
		t.Fatal("X's queued turn did not find recvMu held with an event merged into it: the test did not drive the window")
	}
	if !servedWhileHeld.Load() {
		t.Errorf("B was not served while the call held X's recvMu: the worker waited for recvMu in X's queued turn")
	}
	if !delivered {
		t.Errorf("z never reached X's onRecv")
	}
}

// TestCallersAndTheWorkerLoseNoBytesAndDoNotDeadlock931 is the check of the
// worker's TryLock-and-requeue against the callers it races with. Four conns
// each have a peer that pushes 1 to 200 bytes at random moments for 2 s, and an
// owner that calls WriteAndPollMulti back to back, so events of a conn are
// collected while a call holds its recvMu, between calls and in the middle of
// the re-arm. Every byte a peer sent must be delivered, by a call's onRecv or by
// the worker's, once the pushes stop, and the callers must keep making calls
// (a deadlock between the worker and a caller shows as a caller that stops).
func TestCallersAndTheWorkerLoseNoBytesAndDoNotDeadlock931(t *testing.T) {
	l, err := New(1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	w := l.WorkerLoop(0).(*worker)

	const conns = 4
	type rig struct {
		fd, peer   int
		sent, recv atomic.Int64
		calls      atomic.Int64
	}
	rigs := make([]*rig, conns)
	for i := range rigs {
		r := &rig{}
		r.fd, r.peer = socketPair(t)
		rigs[i] = r
		if err := w.RegisterConn(r.fd, func(b []byte) { r.recv.Add(int64(len(b))) }, func(error) {}); err != nil {
			t.Fatalf("RegisterConn: %v", err)
		}
		t.Cleanup(func() { _ = w.UnregisterConn(r.fd); _ = unix.Close(r.fd); _ = unix.Close(r.peer) })
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i, r := range rigs {
		wg.Add(2)
		go func() { // the peer: pushes at random moments, and drains what the owner sends
			defer wg.Done()
			rng := uint32(i*7919 + 1)
			sink := make([]byte, 4096)
			chunk := make([]byte, 200)
			for {
				select {
				case <-stop:
					return
				default:
				}
				rng = rng*1664525 + 1013904223
				n := 1 + int(rng>>8)%200
				if wrote, err := unix.Write(r.peer, chunk[:n]); wrote > 0 {
					r.sent.Add(int64(wrote))
				} else if err != nil && err != unix.EAGAIN {
					return
				}
				for {
					if m, _ := unix.Read(r.peer, sink); m <= 0 {
						break
					}
				}
				time.Sleep(time.Duration(rng>>20%300) * time.Microsecond)
			}
		}()
		go func() { // the owner: back-to-back calls
			defer wg.Done()
			rbuf := make([]byte, 1024)
			onRecv := func(b []byte) { r.recv.Add(int64(len(b))) }
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, _ = w.WriteAndPollMulti(r.fd, []byte("req"), rbuf, onRecv, func() bool { return true }, nil)
				r.calls.Add(1)
			}
		}()
	}
	time.Sleep(2 * time.Second)
	var callsMid [conns]int64
	for i, r := range rigs {
		callsMid[i] = r.calls.Load()
	}
	time.Sleep(200 * time.Millisecond)
	for i, r := range rigs {
		if r.calls.Load() == callsMid[i] {
			t.Errorf("conn %d: its owner made no call in the last 200 ms: a caller is stuck", i)
		}
	}
	close(stop)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the callers and peers did not stop within 10 s: a deadlock")
	}
	for dl := time.Now().Add(5 * time.Second); time.Now().Before(dl); time.Sleep(5 * time.Millisecond) {
		all := true
		for _, r := range rigs {
			all = all && r.recv.Load() == r.sent.Load()
		}
		if all {
			break
		}
	}
	var sent, recv, calls int64
	for i, r := range rigs {
		sent += r.sent.Load()
		recv += r.recv.Load()
		calls += r.calls.Load()
		if r.recv.Load() != r.sent.Load() {
			t.Errorf("conn %d: the peer sent %d bytes and %d were delivered: bytes stranded", i, r.sent.Load(), r.recv.Load())
		}
	}
	t.Logf("C931 callers vs worker: %d bytes sent and %d delivered over %d WriteAndPollMulti calls on %d conns", sent, recv, calls, conns)
}
