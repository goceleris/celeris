package postgres

// celeris#859: every event loop finds a driver conn by its descriptor
// NUMBER (Write, WriteAndPoll, UnregisterConn). A pgConn must therefore keep
// its number until its last call on it: once the number is released, the
// next socket the process opens usually gets it, and a later call by number
// acts on that socket and on its registration.
//
// These tests build a pgConn by hand on a loop that records, for every call
// by number, which socket the number named at that moment (by inode). They
// need no event loop and no server, so they run on every platform.

import (
	"errors"
	"io"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/driver/internal/async"
	"github.com/goceleris/celeris/driver/postgres/protocol"
)

// c859Loop is an engine.WorkerLoop that keeps nothing and records every
// call by number. Its UnregisterConn returns without firing onClose, as on
// the io_uring engine and the non-Linux loop, and on the other loops when
// their own teardown claimed the conn first; a test fires onClose itself
// when it wants to.
type c859Loop struct {
	mu    sync.Mutex
	calls []c859Call
	unreg chan struct{} // closed on the first UnregisterConn
	once  sync.Once
}

type c859Call struct {
	op  string
	fd  int
	ino uint64 // inode the number named at the call; 0 when it named nothing
}

func newC859Loop() *c859Loop { return &c859Loop{unreg: make(chan struct{})} }

func (l *c859Loop) record(op string, fd int) {
	l.mu.Lock()
	l.calls = append(l.calls, c859Call{op: op, fd: fd, ino: c859Ino(fd)})
	l.mu.Unlock()
}

func (l *c859Loop) RegisterConn(int, func([]byte), func(error)) error { return nil }

func (l *c859Loop) UnregisterConn(fd int) error {
	l.record("UnregisterConn", fd)
	l.once.Do(func() { close(l.unreg) })
	return nil
}

func (l *c859Loop) Write(fd int, _ []byte) error {
	l.record("Write", fd)
	return nil
}

func (l *c859Loop) CPUID() int { return -1 }

func (l *c859Loop) snapshot() []c859Call {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]c859Call(nil), l.calls...)
}

// c859Ino returns the inode of the file fd names, or 0 if fd names nothing.
func c859Ino(fd int) uint64 {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return 0
	}
	return st.Ino
}

// c859Open reports whether fd names an open file.
func c859Open(fd int) bool {
	_, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	return err == nil
}

// c859Pair returns a connected stream socket pair; the test closes neither
// end itself unless it says so.
func c859Pair(t *testing.T) (int, int) {
	t.Helper()
	p, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	return p[0], p[1]
}

// c859Socket returns a new connected socket and its peer. With n >= 0 the
// socket is put on number n, which must be free (the test fails if it is
// not): it stands in for the next socket the process opens, which the
// kernel gives the lowest free number. The caller closes both.
func c859Socket(t *testing.T, n int) (b, peer int) {
	t.Helper()
	b, peer = c859Pair(t)
	switch n {
	case -1, b:
	case peer:
		// A lower number was free, so the pair took it and n: n is the
		// peer's. Swap the roles rather than dup2 over the peer.
		b, peer = peer, b
	default:
		if c859Open(n) {
			_, _ = unix.Close(b), unix.Close(peer)
			t.Fatalf("fixture: number %d is not free; dup2 would close the file on it", n)
		}
		if err := unix.Dup2(b, n); err != nil {
			t.Fatal(err)
		}
		_ = unix.Close(b)
		b = n
	}
	return b, peer
}

// c859OnNumber puts a new socket on number n, which must be free, checks
// that it is not the conn's socket (notIno), and returns its peer. Cleanup
// closes both.
func c859OnNumber(t *testing.T, n int, notIno uint64) (peer int) {
	t.Helper()
	_, peer = c859Socket(t, n)
	t.Cleanup(func() { _ = unix.Close(n); _ = unix.Close(peer) })
	if got := c859Ino(n); got == 0 || got == notIno {
		t.Fatalf("fixture: the socket on number %d has inode %d, want nonzero and not the conn's %d", n, got, notIno)
	}
	return peer
}

// newC859Conn returns a loop-mode pgConn on one end of a socket pair, the
// pair's other end (the "server"), and the inode of the conn's socket.
func newC859Conn(t *testing.T, loop *c859Loop) (c *pgConn, server int, ino uint64) {
	t.Helper()
	fd, server := c859Pair(t)
	t.Cleanup(func() { _ = unix.Close(server) })
	c = &pgConn{
		fd:     fd,
		fdFile: os.NewFile(uintptr(fd), "pg-859"),
		loop:   loop,
		reader: protocol.NewReader(),
		writer: protocol.NewWriter(),
		bridge: async.NewBridge(),
	}
	ino = c859Ino(fd)
	if ino == 0 {
		t.Fatalf("fixture: the conn's socket (number %d) has no inode", fd)
	}
	return c, server, ino
}

// checkCalls fails t for every call by number that did not reach the conn's
// own socket.
func checkCalls(t *testing.T, loop *c859Loop, n int, ino uint64) {
	t.Helper()
	calls := loop.snapshot()
	if len(calls) == 0 {
		t.Fatal("the conn made no call by number; the test exercised nothing")
	}
	for _, call := range calls {
		if call.fd != n {
			t.Errorf("%s(%d): want the conn's number %d", call.op, call.fd, n)
			continue
		}
		if call.ino != ino {
			t.Errorf("%s(%d) reached inode %d, not the conn's socket (inode %d): the conn had released its number", call.op, n, call.ino, ino)
		}
	}
}

// After a loop has torn the conn down (onClose), the conn's Close still
// writes Terminate and unregisters by number. Both must reach the conn's own
// socket: onClose must not release the number. onClose shuts the socket
// down, so the server sees the client leave as soon as the loop drops the
// conn.
func TestPgOnCloseKeepsTheNumberUntilClose859(t *testing.T) {
	loop := newC859Loop()
	c, server, ino := newC859Conn(t, loop)
	n := c.fd

	c.onClose(io.EOF) // the loop tore the conn down (peer hangup)

	if !c859Open(n) {
		// What the next socket the process opens would do: take the number.
		_ = c859OnNumber(t, n, ino)
		t.Errorf("onClose released number %d before Close", n)
	} else if got := c859Ino(n); got != ino {
		t.Fatalf("number %d names inode %d after onClose, want the conn's socket %d", n, got, ino)
	} else {
		// The socket must be shut down: the server reads EOF.
		_ = unix.SetNonblock(server, true)
		var b [8]byte
		k, err := unix.Read(server, b[:])
		if k != 0 || err != nil {
			t.Errorf("server read (%d, %v) after onClose, want EOF (0, nil): onClose left the socket open", k, err)
		}
	}

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	checkCalls(t, loop, n, ino)
	if c859Ino(n) == ino {
		t.Errorf("number %d still names the conn's socket after Close: Close did not release it", n)
	}
}

// Close must release the number only after the conn's background goroutines
// (dropPreparedAsync, tracked by closeWG) are done: one of them may still be
// about to write by number.
func TestPgCloseReleasesTheNumberAfterBackgroundCalls859(t *testing.T) {
	loop := newC859Loop()
	c, _, ino := newC859Conn(t, loop)
	n := c.fd

	// A request in flight: failAll fails it, which tells the test that
	// Close has finished its once body.
	req := &pgRequest{doneCh: make(chan struct{})}
	c.enqueue(req)

	// A background goroutine that writes by number once the test lets it,
	// as dropPreparedAsync does when it loses the race with Close.
	gate := make(chan struct{})
	c.closeWG.Add(1)
	go func() {
		defer c.closeWG.Done()
		<-gate
		_ = c.writeRaw([]byte("late"))
	}()

	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()
	select {
	case <-req.doneCh:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not fail the pending request within 5 s")
	}
	close(gate)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return within 5 s of the background goroutine's release")
	}

	checkCalls(t, loop, n, ino)
	if c859Ino(n) == ino {
		t.Errorf("number %d still names the conn's socket after Close: Close did not release it", n)
	}
}

// onClose can fire after UnregisterConn has returned (always on the io_uring
// engine and the non-Linux loop; on the standalone Linux loop and the epoll
// engine when their own teardown claimed the conn first), possibly after
// Close has released the number and another socket has taken it. onClose
// must then leave that socket alone.
func TestPgLateOnCloseLeavesTheReusedNumberAlone859(t *testing.T) {
	loop := newC859Loop()
	c, _, ino := newC859Conn(t, loop)
	n := c.fd

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if c859Open(n) {
		t.Fatalf("number %d is still open after Close", n)
	}
	bPeer := c859OnNumber(t, n, ino) // the next socket takes the number
	var b [8]byte
	// Control: B works before the late onClose.
	if _, err := unix.Write(n, []byte("c")); err != nil {
		t.Fatalf("control: write on B (number %d): %v", n, err)
	}
	if k, err := unix.Read(bPeer, b[:]); k != 1 || err != nil {
		t.Fatalf("control: B's peer read (%d, %v), want 1 byte", k, err)
	}

	c.onClose(nil) // the engine's deferred onClose for the conn

	// B must still work both ways.
	if _, err := unix.Write(n, []byte("b")); err != nil {
		t.Errorf("write on B (number %d) after the conn's late onClose: %v", n, err)
	}
	_ = unix.SetNonblock(bPeer, true)
	if k, err := unix.Read(bPeer, b[:]); k != 1 || err != nil {
		t.Errorf("B's peer read (%d, %v), want B's 1 byte: the late onClose shut B down", k, err)
	}
	if _, err := unix.Write(bPeer, []byte("p")); err != nil {
		t.Errorf("write to B from its peer after the late onClose: %v", err)
	}
	_ = unix.SetNonblock(n, true)
	k, err := unix.Read(n, b[:])
	if k != 1 || err != nil {
		if errors.Is(err, syscall.EAGAIN) {
			t.Errorf("B read nothing after its peer wrote")
		} else {
			t.Errorf("B read (%d, %v) after the late onClose, want its peer's 1 byte", k, err)
		}
	}
}

// A loop can deliver onRecv once more after UnregisterConn has returned
// (driver/internal/eventloop UnregisterConn), so an onRecv that read the
// startup request as its head before Close failed it can still send the
// startup exchange's response, the one write onRecv makes, after Close has
// released the number. The test puts the request back at the head after
// Close, as that onRecv sees it, and delivers an authentication request.
func TestPgLateStartupWriteStaysOffTheReleasedNumber859(t *testing.T) {
	loop := newC859Loop()
	c, _, ino := newC859Conn(t, loop)
	n := c.fd
	st := &protocol.StartupState{User: "u", Password: "p"}
	_ = st.Start(protocol.NewWriter())
	req := &pgRequest{kind: reqStartup, startup: st, doneCh: make(chan struct{})}
	c.enqueue(req)

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_ = c859OnNumber(t, n, ino) // the next socket takes the number

	c.pendingMu.Lock()
	c.pending = append(c.pending, req) // the head the late onRecv read
	c.pendingMu.Unlock()
	c.onRecv([]byte{protocol.BackendAuthentication, 0, 0, 0, 8, 0, 0, 0, byte(protocol.AuthCleartextPassword)})

	checkCalls(t, loop, n, ino)
}
