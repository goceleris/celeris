//go:build linux

package eventloop

// celeris#784 item 1: once UnregisterConn has returned, the standalone loop
// must not read the conn's descriptor NUMBER again, nor tear down whatever
// the number names by then. The owner closes the fd as soon as UnregisterConn
// returns (redis, memcached), or the conn's onClose closes it (postgres), and
// the next socket the process opens, a driver conn or an HTTP connection the
// server accepts, usually gets the same number.
//
// Each test parks the reader inside the conn's onRecv (or uses a worker with no
// goroutine of its own), unregisters the conn, closes it, puts a new socket on
// the same number with dup3, and only then lets the reader go on. Nothing here
// depends on timing except the bound on how long a worker may take to serve
// another conn.

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine"
)

// c784Stolen is what the socket that takes the number holds for its own
// reader. A reader that still reads the old conn's number gets these bytes.
const c784Stolen = "XXXXXXXXXX"

// c784Conn is a registered conn whose first onRecv parks until release is
// closed, so a test can unregister it while its reader is between two reads.
type c784Conn struct {
	fd, peer int
	entered  chan int
	release  chan struct{}
	once     sync.Once

	mu    sync.Mutex
	after [][]byte // what onRecv got after the park
}

func (c *c784Conn) letGo() { c.once.Do(func() { close(c.release) }) }

func (c *c784Conn) afterPark() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]byte(nil), c.after...)
}

// c784Queue writes all of b to the non-blocking fd, or fails the test.
func c784Queue(t *testing.T, fd int, b []byte) {
	t.Helper()
	for off := 0; off < len(b); {
		n, err := unix.Write(fd, b[off:])
		if err != nil {
			t.Fatalf("queue %d bytes on fd %d: %v", len(b), fd, err)
		}
		off += n
	}
}

// c784Payload is one full 16 KiB read buffer plus 100 bytes, so the reader
// reads again after the first onRecv returns.
func c784Payload() []byte { return bytes.Repeat([]byte{'A'}, 16<<10+100) }

// c784RegisterParked registers a socketpair end on w whose peer has already
// queued c784Payload (and shut its write side when shutWr is set, so the
// event carries EPOLLRDHUP), and waits until w's reader is parked in the
// first onRecv.
func c784RegisterParked(t *testing.T, w engine.WorkerLoop, shutWr bool) *c784Conn {
	t.Helper()
	c := &c784Conn{entered: make(chan int, 1), release: make(chan struct{})}
	c.fd, c.peer = socketPair(t)
	t.Cleanup(func() { _ = unix.Close(c.peer) })
	t.Cleanup(c.letGo)
	c784Queue(t, c.peer, c784Payload())
	if shutWr {
		if err := unix.Shutdown(c.peer, unix.SHUT_WR); err != nil {
			t.Fatalf("shutdown peer: %v", err)
		}
	}
	first := true
	if err := w.RegisterConn(c.fd, func(b []byte) {
		if first {
			first = false
			c.entered <- len(b)
			<-c.release
			return
		}
		c.mu.Lock()
		c.after = append(c.after, append([]byte(nil), b...))
		c.mu.Unlock()
	}, func(error) {}); err != nil {
		t.Fatalf("RegisterConn(A): %v", err)
	}
	select {
	case n := <-c.entered:
		if n != 16<<10 {
			t.Fatalf("first read returned %d bytes, want a full 16 KiB buffer", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the worker never entered A's onRecv")
	}
	return c
}

// c784TakeNumber closes nothing: the caller has closed fd. It opens a new
// socketpair and puts one end on fd's number, the way the next accept or dial
// of the process would reuse it. It returns the socket now on the number
// (== fd) and its peer. Either end of the new pair may already be fd, the
// lowest free number; otherwise the first end is moved onto fd with dup3,
// and the other end is never fd, so dup3 never closes the peer.
func c784TakeNumber(t *testing.T, fd int, nonblock bool) (int, int) {
	t.Helper()
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	var peer int
	switch fd {
	case pair[0]:
		peer = pair[1]
	case pair[1]:
		peer = pair[0]
	default:
		if err := unix.Dup3(pair[0], fd, unix.O_CLOEXEC); err != nil {
			t.Fatalf("dup3 onto %d: %v", fd, err)
		}
		_ = unix.Close(pair[0])
		peer = pair[1]
	}
	if nonblock {
		if err := unix.SetNonblock(fd, true); err != nil {
			t.Fatalf("nonblock: %v", err)
		}
		if err := unix.SetNonblock(peer, true); err != nil {
			t.Fatalf("nonblock: %v", err)
		}
	}
	t.Cleanup(func() {
		_ = unix.Close(fd)
		_ = unix.Close(peer)
	})
	return fd, peer
}

// c784Serves reports whether w delivers a byte to another conn within d,
// i.e. whether the worker goroutine is still running its event loop.
func c784Serves(t *testing.T, w engine.WorkerLoop, d time.Duration) bool {
	t.Helper()
	r, p := socketPair(t)
	defer func() { _ = unix.Close(r); _ = unix.Close(p) }()
	got := make(chan struct{}, 1)
	if err := w.RegisterConn(r, func([]byte) {
		select {
		case got <- struct{}{}:
		default:
		}
	}, func(error) {}); err != nil {
		t.Fatalf("RegisterConn(R): %v", err)
	}
	defer func() { _ = w.UnregisterConn(r) }()
	if _, err := unix.Write(p, []byte{'r'}); err != nil {
		t.Fatalf("write R peer: %v", err)
	}
	select {
	case <-got:
		return true
	case <-time.After(d):
		return false
	}
}

// c784ReadAll reads what fd holds right now (non-blocking).
func c784ReadAll(fd int) (string, error) {
	var buf [256]byte
	n, err := unix.Read(fd, buf[:])
	if n > 0 {
		return string(buf[:n]), nil
	}
	return "", err
}

func c784Contains(chunks [][]byte, s string) bool {
	for _, b := range chunks {
		if bytes.Contains(b, []byte(s)) {
			return true
		}
	}
	return false
}

// TestUnregisterConnStopsTheReadLoop784: the worker is between two reads of
// A when UnregisterConn(A) returns; A is closed and X takes its number with
// bytes waiting. The worker must not read X: X's bytes stay with X, and A's
// onRecv never sees them.
func TestUnregisterConnStopsTheReadLoop784(t *testing.T) {
	l, err := New(1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	w := l.WorkerLoop(0)

	a := c784RegisterParked(t, w, false)
	if err := w.UnregisterConn(a.fd); err != nil {
		t.Fatalf("UnregisterConn(A): %v", err)
	}
	if err := unix.Close(a.fd); err != nil {
		t.Fatalf("close A: %v", err)
	}
	x, xPeer := c784TakeNumber(t, a.fd, true)
	c784Queue(t, xPeer, []byte(c784Stolen))
	a.letGo()

	if !c784Serves(t, w, 5*time.Second) {
		t.Fatal("the worker served no other conn within 5 s")
	}
	got, rerr := c784ReadAll(x)
	after := a.afterPark()
	t.Logf("C784 read-loop: X (fd %d) reads %q (err %v); A's onRecv after UnregisterConn returned got %d chunk(s), X's bytes among them: %v",
		x, got, rerr, len(after), c784Contains(after, c784Stolen))
	if c784Contains(after, c784Stolen) {
		t.Errorf("A's onRecv got X's bytes after UnregisterConn(A) returned: the worker read A's number after A was closed and X took it")
	}
	if got != c784Stolen {
		t.Errorf("X, which took A's number after UnregisterConn(A) returned, reads %q (err %v), want %q: the worker consumed X's bytes", got, rerr, c784Stolen)
	}
}

// TestUnregisterConnNeverBlocksTheWorkerOnAReusedNumber784: as above, but X
// is a BLOCKING socket with nothing to read. A worker that reads the number
// blocks in read(2) and serves none of its other conns.
func TestUnregisterConnNeverBlocksTheWorkerOnAReusedNumber784(t *testing.T) {
	l, err := New(1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	w := l.WorkerLoop(0)

	a := c784RegisterParked(t, w, false)
	if err := w.UnregisterConn(a.fd); err != nil {
		t.Fatalf("UnregisterConn(A): %v", err)
	}
	if err := unix.Close(a.fd); err != nil {
		t.Fatalf("close A: %v", err)
	}
	x, xPeer := c784TakeNumber(t, a.fd, false)
	a.letGo()

	served := c784Serves(t, w, 3*time.Second)
	t.Logf("C784 blocking: the worker served another conn within 3 s: %v", served)
	if !served {
		// Unblock the worker so Close can finish: make X non-blocking for
		// the reads after this one, then give the blocked read a byte.
		_ = unix.SetNonblock(x, true)
		_, _ = unix.Write(xPeer, []byte{'u'})
		t.Errorf("the worker served no other conn for 3 s: it is blocked reading the blocking socket that took A's number after UnregisterConn(A) returned")
	}
}

// TestUnregisterConnSparesTheConnThatTakesTheNumber784: A's event carries
// EPOLLRDHUP (its peer shut down). While the worker is between two reads of
// A, A is unregistered and closed, and B, a new driver conn, takes A's number
// and registers on the same worker. B must stay registered and working: the
// reader must not finish A's event (the read, or the RDHUP teardown) on the
// number that is B's now.
func TestUnregisterConnSparesTheConnThatTakesTheNumber784(t *testing.T) {
	l, err := New(1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	w := l.WorkerLoop(0)

	a := c784RegisterParked(t, w, true)
	if err := w.UnregisterConn(a.fd); err != nil {
		t.Fatalf("UnregisterConn(A): %v", err)
	}
	if err := unix.Close(a.fd); err != nil {
		t.Fatalf("close A: %v", err)
	}
	b, bPeer := c784TakeNumber(t, a.fd, true)
	bRecv := make(chan []byte, 8)
	bClosed := make(chan error, 1)
	if err := w.RegisterConn(b, func(p []byte) {
		bRecv <- append([]byte(nil), p...)
	}, func(err error) {
		bClosed <- err
	}); err != nil {
		t.Fatalf("RegisterConn(B) on A's number: %v", err)
	}
	t.Cleanup(func() { _ = w.UnregisterConn(b) })
	a.letGo()

	if !c784Serves(t, w, 5*time.Second) {
		t.Fatal("the worker served no other conn within 5 s")
	}
	select {
	case err := <-bClosed:
		t.Fatalf("B's onClose fired (err %v): the worker finished A's EPOLLRDHUP event on A's number, which is B's now", err)
	default:
	}
	if err := w.Write(b, []byte("hi")); err != nil {
		t.Fatalf("Write(B): %v: B is no longer registered", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	var got string
	for time.Now().Before(deadline) && got == "" {
		got, _ = c784ReadAll(bPeer)
		if got == "" {
			time.Sleep(time.Millisecond)
		}
	}
	if got != "hi" {
		t.Fatalf("B's peer read %q, want \"hi\"", got)
	}
	c784Queue(t, bPeer, []byte("yo"))
	select {
	case p := <-bRecv:
		if string(p) != "yo" {
			t.Fatalf("B's onRecv got %q, want \"yo\"", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("B's onRecv got nothing within 2 s: B's number is no longer in the worker's epoll set")
	}
	after := a.afterPark()
	t.Logf("C784 rdhup: B alive after A's event; A's onRecv after UnregisterConn returned got %d chunk(s)", len(after))
}

// TestWriteAndPollStopsReadingAfterUnregisterConn784: the caller-side reads
// of the three sync round-trip paths. The caller is inside the onRecv of its
// first read when another goroutine unregisters the conn, the conn is closed
// and X takes the number. The call must not read X, and must report that the
// conn went away under it.
//
// The worker has no goroutine of its own (newWorker, no run), so nothing but
// the call under test reads the conn.
func TestWriteAndPollStopsReadingAfterUnregisterConn784(t *testing.T) {
	type call func(w *worker, fd int, rbuf []byte, onRecv func([]byte)) (bool, error)
	calls := []struct {
		name string
		call call
	}{
		{"WriteAndPoll", func(w *worker, fd int, rbuf []byte, onRecv func([]byte)) (bool, error) {
			return w.WriteAndPoll(fd, []byte("q"), rbuf, onRecv)
		}},
		{"WriteAndPollBusy", func(w *worker, fd int, rbuf []byte, onRecv func([]byte)) (bool, error) {
			return w.WriteAndPollBusy(fd, []byte("q"), rbuf, onRecv)
		}},
		{"WriteAndPollMulti", func(w *worker, fd int, rbuf []byte, onRecv func([]byte)) (bool, error) {
			return w.WriteAndPollMulti(fd, []byte("q"), rbuf, onRecv, func() bool { return false }, nil)
		}},
	}
	for _, tc := range calls {
		t.Run(tc.name, func(t *testing.T) {
			w, err := newWorker(0)
			if err != nil {
				t.Fatalf("newWorker: %v", err)
			}
			t.Cleanup(func() { _ = w.shutdown() })

			a, aPeer := socketPair(t)
			t.Cleanup(func() { _ = unix.Close(aPeer) })
			c784Queue(t, aPeer, c784Payload())
			if err := w.RegisterConn(a, func([]byte) {}, func(error) {}); err != nil {
				t.Fatalf("RegisterConn(A): %v", err)
			}

			entered := make(chan int, 1)
			release := make(chan struct{})
			var once sync.Once
			letGo := func() { once.Do(func() { close(release) }) }
			t.Cleanup(letGo)
			var mu sync.Mutex
			var after [][]byte
			first := true
			onRecv := func(b []byte) {
				if first {
					first = false
					entered <- len(b)
					<-release
					return
				}
				mu.Lock()
				after = append(after, append([]byte(nil), b...))
				mu.Unlock()
			}
			type result struct {
				ok  bool
				err error
			}
			done := make(chan result, 1)
			go func() {
				ok, err := tc.call(w, a, make([]byte, 16<<10), onRecv)
				done <- result{ok, err}
			}()
			select {
			case n := <-entered:
				if n != 16<<10 {
					t.Fatalf("first read returned %d bytes, want a full 16 KiB buffer", n)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the call never entered onRecv")
			}

			if err := w.UnregisterConn(a); err != nil {
				t.Fatalf("UnregisterConn(A): %v", err)
			}
			if err := unix.Close(a); err != nil {
				t.Fatalf("close A: %v", err)
			}
			x, xPeer := c784TakeNumber(t, a, true)
			c784Queue(t, xPeer, []byte(c784Stolen))
			letGo()

			var res result
			select {
			case res = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the call did not return within 5 s")
			}
			got, rerr := c784ReadAll(x)
			mu.Lock()
			stole := c784Contains(after, c784Stolen)
			n := len(after)
			mu.Unlock()
			t.Logf("C784 %s: returned (%v, %v); X reads %q (err %v); onRecv after UnregisterConn returned got %d chunk(s), X's bytes among them: %v",
				tc.name, res.ok, res.err, got, rerr, n, stole)
			if stole {
				t.Errorf("%s handed X's bytes to A's onRecv after UnregisterConn(A) returned", tc.name)
			}
			if got != c784Stolen {
				t.Errorf("X, which took A's number after UnregisterConn(A) returned, reads %q (err %v), want %q: %s consumed X's bytes", got, rerr, c784Stolen, tc.name)
			}
			if !errors.Is(res.err, engine.ErrUnknownFD) {
				t.Errorf("%s returned (%v, %v) for a conn unregistered under it, want engine.ErrUnknownFD", tc.name, res.ok, res.err)
			}
		})
	}
}
