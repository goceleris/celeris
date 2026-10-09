//go:build linux

package epoll

// celeris#771: the epoll worker named a driver conn by its descriptor number
// where it needed the conn. Each registration now has a generation, carried
// in every epoll_event of the conn (Pad), and an event is applied only to the
// conn whose generation it carries; closeDriver removes the conn it claimed,
// not whatever holds the number by then.

import (
	"math"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
)

type parker771 struct {
	fd, peer int
	entered  chan struct{}
	release  chan struct{}
}

// newParker771 registers a conn whose onRecv parks the worker until release is
// closed.
func newParker771(t *testing.T, wl engine.WorkerLoop) *parker771 {
	t.Helper()
	p := &parker771{entered: make(chan struct{}, 1), release: make(chan struct{})}
	p.fd, p.peer = socketpairNonblocking(t)
	if err := wl.RegisterConn(p.fd, func([]byte) {
		p.entered <- struct{}{}
		<-p.release
	}, func(error) {}); err != nil {
		t.Fatalf("RegisterConn(parker): %v", err)
	}
	t.Cleanup(func() {
		select {
		case <-p.release:
		default:
			close(p.release)
		}
		_ = wl.UnregisterConn(p.fd)
		_ = unix.Close(p.fd)
		_ = unix.Close(p.peer)
	})
	return p
}

func (p *parker771) wait(t *testing.T) {
	t.Helper()
	select {
	case <-p.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the worker never entered the parker's onRecv")
	}
}

// TestStaleEventDoesNotReachConnOnReusedNumber771: the worker has collected
// an event of conn A (A's peer closed) when A is unregistered and closed, and
// X is registered on A's number. A's event is dispatched after that, and must
// not close X nor hide X's bytes. Forced: the worker is parked in a conn
// P's onRecv while A's event waits behind P's in the same batch.
func TestStaleEventDoesNotReachConnOnReusedNumber771(t *testing.T) {
	eng, stop := newTestEngine(t)
	t.Cleanup(stop)
	wl := eng.WorkerLoop(0)

	q := newParker771(t, wl)
	p := newParker771(t, wl)
	a, aPeer := socketpairNonblocking(t)
	if err := wl.RegisterConn(a, func([]byte) {}, func(error) {}); err != nil {
		t.Fatalf("RegisterConn(A): %v", err)
	}
	if !workerRoundTrip(t, wl, 5*time.Second) {
		t.Fatal("round trip")
	}

	// Park in Q; make P ready, then A (its peer closes); release Q: the next
	// epoll_wait returns P's event and A's, in that order.
	_, _ = unix.Write(q.peer, []byte{'q'})
	q.wait(t)
	_, _ = unix.Write(p.peer, []byte{'p'})
	_ = unix.Close(aPeer)
	close(q.release)
	p.wait(t)

	// The worker is in P's onRecv, with A's event still to dispatch.
	if err := wl.UnregisterConn(a); err != nil {
		t.Fatalf("UnregisterConn(A): %v", err)
	}
	_ = unix.Close(a)
	x0, x1 := takeNumber710(t, a, true)
	defer closeX710(a, x0, x1)
	xClosed := make(chan error, 1)
	xRecv := make(chan string, 4)
	if err := wl.RegisterConn(a, func(b []byte) { xRecv <- string(b) }, func(err error) { xClosed <- err }); err != nil {
		t.Fatalf("RegisterConn(X on A's number %d): %v", a, err)
	}
	close(p.release)
	if !workerRoundTrip(t, wl, 5*time.Second) {
		t.Fatal("round trip after release")
	}

	closedX := false
	select {
	case err := <-xClosed:
		closedX = true
		t.Logf("771 X's onClose fired with %v", err)
	default:
	}
	_, _ = unix.Write(x1, []byte("xx"))
	gotX := ""
	select {
	case gotX = <-xRecv:
	case <-time.After(2 * time.Second):
	}
	t.Logf("771 X closed=%v, a byte from X's peer reached X's onRecv=%q", closedX, gotX)
	if closedX || gotX == "" {
		t.Errorf("A's stale event, dispatched after UnregisterConn(A) returned, reached X, the conn registered on A's number: X closed=%v, X receives=%q", closedX, gotX)
	}
	_ = wl.UnregisterConn(a)
}

// TestCloseDriverSparesConnOnReusedNumber771: closeDriver(A), driven by the
// worker's read of A's EOF, claims A closed and lets UnregisterConn return
// before it touches A's number again: the hook runs where UnregisterConn(A)
// can return, the caller closes A and registers X on the number. closeDriver
// must then neither delete X's map entry nor remove X from the interest set.
func TestCloseDriverSparesConnOnReusedNumber771(t *testing.T) {
	var (
		target     atomic.Int64
		fired      atomic.Bool
		hookDone   = make(chan struct{})
		hookErr    error
		xFD, xPeer int
		xClosed    = make(chan error, 1)
		xRecv      = make(chan string, 4)
		wlHook     engine.WorkerLoop
	)
	target.Store(-1)
	testHookCloseDriverClaimed = func(dc *driverConn) {
		if int64(dc.fd) != target.Load() || !fired.CompareAndSwap(false, true) {
			return
		}
		defer close(hookDone)
		a := dc.fd
		if hookErr = wlHook.UnregisterConn(a); hookErr != nil {
			return
		}
		_ = unix.Close(a)
		pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, 0)
		if err != nil {
			hookErr = err
			return
		}
		xFD, xPeer = pair[0], pair[1]
		if xFD != a {
			if hookErr = unix.Dup3(xFD, a, unix.O_CLOEXEC); hookErr != nil {
				return
			}
		}
		hookErr = wlHook.RegisterConn(a, func(b []byte) { xRecv <- string(b) }, func(err error) { xClosed <- err })
	}
	t.Cleanup(func() { testHookCloseDriverClaimed = nil })

	eng, stop := newTestEngine(t)
	t.Cleanup(stop)
	wl := eng.WorkerLoop(0)
	wlHook = wl
	l := eng.loops[0]

	a, aPeer := socketpairNonblocking(t)
	aClosed := make(chan error, 1)
	target.Store(int64(a))
	if err := wl.RegisterConn(a, func([]byte) {}, func(err error) { aClosed <- err }); err != nil {
		t.Fatalf("RegisterConn(A): %v", err)
	}
	_ = unix.Close(aPeer) // the worker reads EOF and closes A

	select {
	case <-hookDone:
	case <-time.After(5 * time.Second):
		t.Fatal("closeDriver(A) never ran")
	}
	defer func() {
		_ = wl.UnregisterConn(a)
		_ = unix.Close(xFD)
		_ = unix.Close(xPeer)
		if xFD != a {
			_ = unix.Close(a)
		}
	}()
	if hookErr != nil {
		t.Fatalf("hook (UnregisterConn(A), close, X on the number, RegisterConn(X)): %v", hookErr)
	}
	select {
	case <-aClosed:
	case <-time.After(5 * time.Second):
		t.Fatal("A's onClose never fired")
	}

	if dc := l.lookupDriver(a); dc == nil || dc.fd != a || dc.gen == 0 {
		t.Errorf("closeDriver(A) deleted X's entry from the map: lookup(%d) = %v", a, dc)
	} else if err := l.driverEpollCtl(unix.EPOLL_CTL_MOD, a, ptr(dc.epollEvent(unix.EPOLLIN|unix.EPOLLET|unix.EPOLLRDHUP))); err != nil {
		t.Errorf("closeDriver(A) removed X from the interest set: EPOLL_CTL_MOD: %v", err)
	}
	_, _ = unix.Write(xPeer, []byte("xx"))
	select {
	case got := <-xRecv:
		if got != "xx" {
			t.Errorf("X received %q, want %q", got, "xx")
		}
	case err := <-xClosed:
		t.Errorf("X was closed by closeDriver(A): onClose(%v)", err)
	case <-time.After(2 * time.Second):
		t.Errorf("a byte from X's peer never reached X's onRecv: closeDriver(A) acted on X's number")
	}
}

func ptr[T any](v T) *T { return &v }

// TestDispatchDropsEventsOfEndedRegistrations771 pins the dispatch rule
// itself, on the loop's own table: an event reaches the conn it carries the
// generation of and no other; a driver event whose conn has gone is dropped
// and not given to the HTTP path; an HTTP conn's event (generation 0) on a
// driver conn's number is stale and dropped; and an event of no driver conn
// and no generation is the HTTP path's.
func TestDispatchDropsEventsOfEndedRegistrations771(t *testing.T) {
	eng, stop := newTestEngine(t)
	t.Cleanup(stop)
	wl := eng.WorkerLoop(0)
	l := eng.loops[0]

	x, xPeer := socketpairNonblocking(t)
	defer func() { _ = unix.Close(x); _ = unix.Close(xPeer) }()
	var closed atomic.Int32
	if err := wl.RegisterConn(x, func([]byte) {}, func(error) { closed.Add(1) }); err != nil {
		t.Fatalf("RegisterConn(X): %v", err)
	}
	defer func() { _ = wl.UnregisterConn(x) }()
	dc := l.lookupDriver(x)
	if dc == nil || dc.gen == 0 {
		t.Fatalf("X is not registered with a generation: %v", dc)
	}
	hangup := uint32(unix.EPOLLIN | unix.EPOLLRDHUP | unix.EPOLLHUP | unix.EPOLLERR)
	for _, c := range []struct {
		name string
		gen  uint32
	}{
		{"a driver event of another registration", dc.gen + 1},
		{"a driver event of the previous registration", dc.gen - 1},
		{"an HTTP conn's event (generation 0) on a driver conn's number", 0},
	} {
		if !l.dispatchDriver(x, c.gen, hangup) {
			t.Errorf("%s: reported as not a driver event; it would go to the HTTP path", c.name)
		}
		if n := closed.Load(); n != 0 {
			t.Fatalf("%s: closed X (%d onClose calls)", c.name, n)
		}
		if l.lookupDriver(x) != dc {
			t.Fatalf("%s: removed X from the map", c.name)
		}
	}

	// A number nothing is registered on.
	free := x + 1000
	if !l.dispatchDriver(free, 7, hangup) {
		t.Error("a driver event whose conn has gone was given to the HTTP path")
	}
	if l.dispatchDriver(free, 0, hangup) {
		t.Error("an event with no generation, on a number no driver conn has, was taken for a driver's")
	}

	// The gate: a generation says driver even when no driver conn is registered.
	if !l.driverCandidate(7) || !l.driverCandidate(1) {
		t.Error("an event with a generation is not looked up when hasDriverConns is false or true")
	}
	if !l.driverCandidate(0) { // X is registered: hasDriverConns is set
		t.Error("an event without a generation is not looked up while a driver conn is registered")
	}
	_ = wl.UnregisterConn(x)
	if l.hasDriverConns.Load() {
		t.Fatal("hasDriverConns still set after the last UnregisterConn")
	}
	if !l.driverCandidate(7) {
		t.Error("a stale driver event would go to the HTTP path once the last driver conn is gone")
	}
	if l.driverCandidate(0) {
		t.Error("an HTTP conn's event is looked up as a driver's with no driver conn registered")
	}
}

// TestGenerationsWrapPastZero771: the loop's registration generation wraps
// from MaxUint32 to 1, never to 0 (0 names an HTTP conn), and the conns
// registered on either side of the wrap are each served.
func TestGenerationsWrapPastZero771(t *testing.T) {
	eng, stop := newTestEngine(t)
	t.Cleanup(stop)
	wl := eng.WorkerLoop(0)
	l := eng.loops[0]
	l.driverMu.Lock()
	l.driverGen = math.MaxUint32 - 1
	l.driverMu.Unlock()

	type conn struct {
		fd, peer int
		got      chan []byte
	}
	var cs []conn
	for i := range 3 {
		fd, peer := socketpairNonblocking(t)
		t.Cleanup(func() { _ = unix.Close(fd); _ = unix.Close(peer) })
		c := conn{fd, peer, make(chan []byte, 4)}
		if err := wl.RegisterConn(fd, func(b []byte) { c.got <- append([]byte(nil), b...) }, func(error) {}); err != nil {
			t.Fatalf("RegisterConn %d: %v", i, err)
		}
		t.Cleanup(func() { _ = wl.UnregisterConn(fd) })
		cs = append(cs, c)
	}
	var gens []uint32
	for _, c := range cs {
		gens = append(gens, l.lookupDriver(c.fd).gen)
	}
	if gens[0] != math.MaxUint32 || gens[1] != 1 || gens[2] != 2 {
		t.Errorf("generations %v across the wrap, want [%d 1 2]", gens, uint32(math.MaxUint32))
	}
	for i, c := range cs {
		_, _ = unix.Write(c.peer, []byte{'a' + byte(i)})
		select {
		case b := <-c.got:
			if len(b) != 1 || b[0] != 'a'+byte(i) {
				t.Errorf("conn %d (generation %d) got %q, want %q", i, gens[i], b, []byte{'a' + byte(i)})
			}
		case <-time.After(2 * time.Second):
			t.Errorf("conn %d (generation %d) was not served within 2 s", i, gens[i])
		}
	}
}

// TestEpolloutKeepsGeneration771: a driver Write that meets EAGAIN arms
// EPOLLOUT with an EPOLL_CTL_MOD, and the flush that empties the backlog
// disarms it with another. A MOD replaces the whole event data, so each must
// carry the generation or the worker drops every later event of the conn: the
// backlog would never drain, and the conn would never receive again.
func TestEpolloutKeepsGeneration771(t *testing.T) {
	eng, stop := newTestEngine(t)
	t.Cleanup(stop)
	wl := eng.WorkerLoop(0)

	fd, peer := socketpairNonblocking(t)
	defer func() { _ = unix.Close(fd); _ = unix.Close(peer) }()
	recv := make(chan []byte, 4)
	if err := wl.RegisterConn(fd, func(b []byte) { recv <- append([]byte(nil), b...) }, func(error) {}); err != nil {
		t.Fatalf("RegisterConn: %v", err)
	}
	defer func() { _ = wl.UnregisterConn(fd) }()

	// Far more than a unix socket buffer holds: the first write(2) is partial,
	// the next meets EAGAIN, and EPOLLOUT is armed (MOD).
	const total = 8 << 20
	payload := make([]byte, total)
	if err := wl.Write(fd, payload); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// The peer drains what arrives; EPOLLOUT events (dispatched by generation)
	// resume the flush until every byte is out.
	got := 0
	scratch := make([]byte, 1<<20)
	deadline := time.Now().Add(10 * time.Second)
	for got < total && time.Now().Before(deadline) {
		n, err := unix.Read(peer, scratch)
		if err != nil {
			time.Sleep(200 * time.Microsecond)
			continue
		}
		got += n
	}
	if got != total {
		t.Fatalf("the peer received %d of %d bytes: the EPOLLOUT armed by the MOD did not resume the flush (its events lost the generation)", got, total)
	}
	// The flush is done and EPOLLOUT disarmed (a second MOD): the conn must
	// still receive.
	time.Sleep(50 * time.Millisecond)
	_, _ = unix.Write(peer, []byte("after"))
	select {
	case b := <-recv:
		if string(b) != "after" {
			t.Errorf("received %q, want %q", b, "after")
		}
	case <-time.After(3 * time.Second):
		t.Error("the conn received nothing after its EPOLLOUT was disarmed (the disarming MOD lost the generation)")
	}
}
