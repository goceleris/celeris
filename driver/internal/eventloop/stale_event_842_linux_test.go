//go:build linux

package eventloop

// celeris#842: the worker dispatches each event of an epoll_wait batch to the
// conn it was collected for, not to whatever conn holds the event's
// descriptor number when the event is dispatched. An event collected for conn
// A, still waiting in the batch when A is unregistered and closed and a new
// conn B takes A's number on the same worker, must not reach B: A's
// EPOLLRDHUP (A's server had hung up) would tear B down.
//
// Each registration carries a generation in the epoll event data (Pad), on
// the ADD and on every MOD, and the worker drops an event whose generation is
// not the one of the conn registered on the number.

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// c842Parker is a registered conn whose first onRecv parks the worker until
// letGo, so a test can hold the worker between two events of one batch.
type c842Parker struct {
	fd, peer int
	entered  chan struct{}
	release  chan struct{}
	once     sync.Once
}

func (p *c842Parker) letGo() { p.once.Do(func() { close(p.release) }) }

func (p *c842Parker) waitEntered(t *testing.T, name string) {
	t.Helper()
	select {
	case <-p.entered:
	case <-time.After(5 * time.Second):
		t.Fatalf("the worker never entered %s's onRecv", name)
	}
}

func c842NewParker(t *testing.T, w *worker) *c842Parker {
	t.Helper()
	p := &c842Parker{entered: make(chan struct{}, 1), release: make(chan struct{})}
	p.fd, p.peer = socketPair(t)
	t.Cleanup(func() {
		p.letGo()
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

// TestStaleEventSparesTheConnThatTakesTheNumber842 holds the worker in Q's
// onRecv while P and then A become ready (A's peer shuts its write side, so
// A's event carries EPOLLRDHUP). When Q is let go, the next epoll_wait returns
// P's event and A's, in that order, and P's onRecv parks the worker again:
// A's event is collected and not yet dispatched. The test unregisters and
// closes A, puts a new conn B on A's number, and registers B on the same
// worker. Then it lets P go, and the worker dispatches A's event. B must not
// be torn down, and must still be served. The worker's drop of the event is
// observed (testHookDroppedEvent), so the test cannot pass with A's event
// missing from the batch.
func TestStaleEventSparesTheConnThatTakesTheNumber842(t *testing.T) {
	type dropped struct {
		fd     int
		events uint32
	}
	drops := make(chan dropped, 16)
	// Registered first, so it runs last: after the loop is closed.
	t.Cleanup(func() { testHookDroppedEvent = nil })
	testHookDroppedEvent = func(fd int, events uint32) {
		select {
		case drops <- dropped{fd, events}:
		default:
		}
	}

	l, err := New(1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	w := l.WorkerLoop(0).(*worker)

	q := c842NewParker(t, w)
	p := c842NewParker(t, w)
	a, aPeer := socketPair(t)
	t.Cleanup(func() { _ = unix.Close(aPeer) })
	aClosed := make(chan error, 1)
	if err := w.RegisterConn(a, func([]byte) {}, func(err error) { aClosed <- err }); err != nil {
		t.Fatalf("RegisterConn(A): %v", err)
	}
	c784Queue(t, q.peer, []byte("q"))
	q.waitEntered(t, "Q")
	c784Queue(t, p.peer, []byte("p"))
	if err := unix.Shutdown(aPeer, unix.SHUT_WR); err != nil {
		t.Fatalf("shutdown A's peer: %v", err)
	}
	q.letGo()
	p.waitEntered(t, "P")

	// The worker is in P's onRecv; A's event is in the batch it is working on.
	select {
	case err := <-aClosed:
		t.Fatalf("A was torn down (%v) before the test unregistered it: A's event was dispatched before P's, and the test cannot hold it", err)
	default:
	}
	if err := w.UnregisterConn(a); err != nil {
		t.Fatalf("UnregisterConn(A): %v", err)
	}
	<-aClosed // UnregisterConn's onClose(nil)
	if err := unix.Close(a); err != nil {
		t.Fatalf("close A: %v", err)
	}
	b, bPeer := c784TakeNumber(t, a, true)
	bRecv := make(chan []byte, 8)
	bClosed := make(chan error, 1)
	if err := w.RegisterConn(b, func(p []byte) {
		bRecv <- append([]byte(nil), p...)
	}, func(err error) { bClosed <- err }); err != nil {
		t.Fatalf("RegisterConn(B) on A's number: %v", err)
	}
	t.Cleanup(func() { _ = w.UnregisterConn(b) })

	p.letGo()
	var drop *dropped
	tornDown, closeErr := false, error(nil)
	select {
	case d := <-drops:
		drop = &d
	case closeErr = <-bClosed:
		tornDown = true
	case <-time.After(5 * time.Second):
	}
	c784Queue(t, bPeer, []byte("yo"))
	var got []byte
	select {
	case got = <-bRecv:
	case <-time.After(2 * time.Second):
	}
	if !tornDown {
		select {
		case closeErr = <-bClosed:
			tornDown = true
		default:
		}
	}
	werr := w.Write(b, []byte("x"))
	echo := c784Drain(bPeer, 1, 2*time.Second)
	t.Logf("C842 stale event: A and then B on number %d; dropped event: %+v; B torn down: %v (%v); B's onRecv got %q; Write(B) = %v, B's peer got %q",
		b, drop, tornDown, closeErr, got, werr, echo)
	if tornDown {
		t.Errorf("A's stale event (EPOLLRDHUP), dispatched by number, tore down B, the conn that took A's number: B's onClose(%v)", closeErr)
	}
	if !bytes.Equal(got, []byte("yo")) {
		t.Errorf("B's onRecv got %q within 2 s, want \"yo\": B is no longer served", got)
	}
	if werr != nil || string(echo) != "x" {
		t.Errorf("Write(B) = %v and B's peer got %q, want nil and \"x\"", werr, echo)
	}
	if !tornDown && drop == nil {
		t.Errorf("the worker never dropped A's stale event: the test did not see it dispatched")
	}
	if drop != nil && (drop.fd != a || drop.events&unix.EPOLLRDHUP == 0) {
		t.Errorf("the dropped event is fd %d events %#x, want A's number %d with EPOLLRDHUP", drop.fd, drop.events, a)
	}
}

// TestEventsReachTheConnAfterEveryEpollCtlMod842: every EPOLL_CTL_MOD of a
// conn must keep its generation in the event data. A MOD replaces the whole
// event, and one that left the generation out would make the worker drop
// every later event of the conn as stale. This drives each MOD and then needs
// the next event: the EPOLLOUT arm (a flush stops at EAGAIN; the rest is sent
// only on EPOLLOUT), the disarm when the flush completes (the next EPOLLIN),
// and a WriteAndPoll* call's mask and re-arm (the next EPOLLIN).
func TestEventsReachTheConnAfterEveryEpollCtlMod842(t *testing.T) {
	l, err := New(1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	w := l.WorkerLoop(0).(*worker)
	a, aPeer := socketPair(t)
	t.Cleanup(func() {
		_ = w.UnregisterConn(a)
		_ = unix.Close(a)
		_ = unix.Close(aPeer)
	})
	recv := make(chan []byte, 64)
	if err := w.RegisterConn(a, func(p []byte) { recv <- append([]byte(nil), p...) }, func(error) {}); err != nil {
		t.Fatalf("RegisterConn: %v", err)
	}
	c := c784Lookup(w, a)
	if c == nil {
		t.Fatal("A is not registered")
	}
	wantIn := func(step string, msg []byte) {
		t.Helper()
		c784Queue(t, aPeer, msg)
		var got []byte
		deadline := time.After(2 * time.Second)
		for len(got) < len(msg) {
			select {
			case b := <-recv:
				got = append(got, b...)
				continue
			case <-deadline:
			}
			break
		}
		if !bytes.Equal(got, msg) {
			t.Fatalf("%s: A's onRecv got %q within 2 s, want %q: the worker dropped A's EPOLLIN", step, got, msg)
		}
	}

	wantIn("after the ADD", []byte("one"))

	// EPOLLOUT arm: more than the socket takes at once.
	flush := bytes.Repeat([]byte{'F'}, 1<<20)
	if err := w.Write(a, flush); err != nil {
		t.Fatalf("Write: %v", err)
	}
	c.mu.Lock()
	armed := c.epollOut
	c.mu.Unlock()
	if !armed {
		t.Fatal("the flush was taken whole: EPOLLOUT was never armed, so the test cannot drive the arm MOD")
	}
	if got := c784Drain(aPeer, len(flush), 5*time.Second); len(got) != len(flush) {
		t.Fatalf("after the EPOLLOUT arm: A's peer got %d of %d bytes within 5 s: the worker dropped A's EPOLLOUT", len(got), len(flush))
	}
	// The flush completed on EPOLLOUT, so it disarmed EPOLLOUT with a MOD.
	disarmed := false
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		c.mu.Lock()
		disarmed = !c.epollOut
		c.mu.Unlock()
		if disarmed {
			break
		}
	}
	if !disarmed {
		t.Fatal("EPOLLOUT still armed 2 s after the flush completed: the test cannot drive the disarm MOD")
	}
	wantIn("after the EPOLLOUT disarm", []byte("two"))

	// WriteAndPoll*: mask EPOLLIN, read nothing (the peer does not answer), re-arm.
	rbuf := make([]byte, 1024)
	nop := func([]byte) {}
	for _, call := range []struct {
		name string
		do   func() (bool, error)
	}{
		{"WriteAndPoll", func() (bool, error) { return w.WriteAndPoll(a, []byte("q"), rbuf, nop) }},
		{"WriteAndPollBusy", func() (bool, error) { return w.WriteAndPollBusy(a, []byte("q"), rbuf, nop) }},
		{"WriteAndPollMulti", func() (bool, error) {
			return w.WriteAndPollMulti(a, []byte("q"), rbuf, nop, func() bool { return false }, nil)
		}},
	} {
		ok, err := call.do()
		if ok || err != nil {
			t.Fatalf("%s = (%v, %v), want (false, nil): the peer sent nothing", call.name, ok, err)
		}
		if got := c784Drain(aPeer, 1, 2*time.Second); string(got) != "q" {
			t.Fatalf("%s: A's peer got %q, want \"q\"", call.name, got)
		}
		wantIn("after "+call.name+"'s mask and re-arm", []byte(call.name))
	}
	t.Logf("C842 MODs: A received after the ADD, the EPOLLOUT arm and disarm, and each WriteAndPoll* mask and re-arm")
}
