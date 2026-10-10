//go:build linux

package epoll

import (
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#876. checkTimeouts measured ReadTimeout and IdleTimeout from
// cs.lastActivity, which only a read stamps, so a conn left to
// closeWhenFlushed (or to the deferred close an EPOLLRDHUP arms) with a large
// response was reaped ReadTimeout after its LAST READ, however steadily the
// client was taking the response and however recently the close was asked.
// closeWhenFlushed stops reading the conn, so nothing ever moved that stamp:
// a response the client takes for longer than ReadTimeout (60 s by default)
// was cut. io_uring's closing drain has restarted its clock on send progress
// since celeris#805.
//
// The tests drive the real closeWhenFlushed, onPeerHalfClose, handleWritable
// and checkTimeouts on a Loop literal with a real epoll set and a socketpair
// whose peer the test reads. They name no field the fix adds, so they build
// on main and fail there.

// closingRig876 is a loop with one conn that has a response too large for the
// socket queued on it, the peer end of which the test reads.
type closingRig876 struct {
	l      *Loop
	cs     *connState
	fd     int
	peer   int
	queued int
	got    int
	hooks  int
}

func newClosingRig876(t *testing.T, cfg resource.Config, queued int) *closingRig876 {
	t.Helper()
	r := &closingRig876{queued: queued}
	cfg.OnDisconnect = func(string) { r.hooks++ }
	l := newLedgerLoop(t)
	l.cfg = cfg
	r.l = l
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		t.Skipf("socketpair unavailable: %v", err)
	}
	r.fd, r.peer = pair[0], pair[1]
	t.Cleanup(func() { _ = unix.Close(r.peer) })
	if r.fd >= len(l.conns) {
		_ = unix.Close(r.fd)
		t.Skipf("socketpair fd %d outside the test conn table", r.fd)
	}
	t.Cleanup(func() {
		if l.conns[r.fd] != nil {
			_ = unix.Close(r.fd)
		}
	})
	// A small queue (a unix stream socket queues on the sender's side), so a
	// response of a few hundred KiB already overruns it.
	_ = unix.SetsockoptInt(r.fd, unix.SOL_SOCKET, unix.SO_SNDBUF, 32<<10)
	if err := unix.EpollCtl(l.epollFD, unix.EPOLL_CTL_ADD, r.fd, &unix.EpollEvent{
		Events: unix.EPOLLIN | unix.EPOLLET | unix.EPOLLRDHUP, Fd: int32(r.fd),
	}); err != nil {
		t.Fatalf("epoll_ctl add: %v", err)
	}
	cs := acquireConnState(t.Context(), r.fd, 4096, false)
	cs.protocol = engine.HTTP1
	cs.detected = true
	cs.h1State = conn.NewH1State()
	cs.remoteAddr = "127.0.0.1:9"
	cs.writeBuf = append(cs.writeBuf[:0], make([]byte, queued)...)
	l.conns[r.fd] = cs
	l.addLiveConn(cs)
	l.connCount++
	l.activeConns.Add(1)
	r.cs = cs
	// The handler's response has been written and flushed as far as the
	// socket takes it: the state closeWhenFlushed is asked to close in.
	if err := l.flushWrites(cs, true); err != nil {
		t.Fatalf("flushWrites: %v", err)
	}
	cs.pendingBytes = csPendingBytes(cs)
	if !csWritePending(cs) {
		t.Fatalf("celeris876 PREMISE: %d queued bytes all went out; the socket must be too small to take them", queued)
	}
	return r
}

// read takes up to n bytes from the peer and reports how many it got.
func (r *closingRig876) read(n int) int {
	buf := make([]byte, n)
	k, err := unix.Read(r.peer, buf)
	if err != nil || k < 0 {
		return 0
	}
	r.got += k
	return k
}

// open reports whether the loop still holds the conn.
func (r *closingRig876) open() bool { return r.l.conns[r.fd] == r.cs }

// TestClosingConnIsNotReapedFromItsLastRead is the issue's shape: the conn's
// last read is an hour old, ReadTimeout is 50 ms, and the engine has only now
// asked to close it behind a response the client has not finished taking. The
// reap must not fire: the close request, not the last read, starts the drain.
func TestClosingConnIsNotReapedFromItsLastRead(t *testing.T) {
	r := newClosingRig876(t, resource.Config{ReadTimeout: 50 * time.Millisecond, WriteTimeout: time.Minute}, 1<<20)
	r.cs.lastActivity = time.Now().Add(-time.Hour).UnixNano()

	r.l.closeWhenFlushed(r.cs)
	if !r.cs.peerClosed || !r.cs.epollOut {
		t.Fatalf("celeris876 PREMISE: closeWhenFlushed did not defer the close (peerClosed=%v epollOut=%v)",
			r.cs.peerClosed, r.cs.epollOut)
	}
	r.l.checkTimeouts()

	if !r.open() || r.l.closeCount.Load() != 0 {
		t.Fatalf("a conn the engine asked to close a moment ago, behind a response still being sent, was reaped "+
			"by ReadTimeout %v measured from a read an hour old (closeCount=%d) (celeris#876)",
			r.l.cfg.ReadTimeout, r.l.closeCount.Load())
	}
}

// TestPeerHalfClosedConnIsNotReapedFromItsLastRead is the same defect on the
// other deferred close, the one EPOLLRDHUP arms while a response is still
// flushing: it also reaches the sweep with a clock a read set long ago.
func TestPeerHalfClosedConnIsNotReapedFromItsLastRead(t *testing.T) {
	r := newClosingRig876(t, resource.Config{ReadTimeout: 50 * time.Millisecond, WriteTimeout: time.Minute}, 1<<20)
	r.cs.lastActivity = time.Now().Add(-time.Hour).UnixNano()

	r.l.onPeerHalfClose(r.fd)
	if !r.cs.peerClosed {
		t.Fatal("celeris876 PREMISE: onPeerHalfClose did not defer the close")
	}
	r.l.checkTimeouts()

	if !r.open() || r.l.closeCount.Load() != 0 {
		t.Fatalf("a half-closed conn still being sent its response was reaped by ReadTimeout measured from a read "+
			"an hour old (closeCount=%d) (celeris#876)", r.l.closeCount.Load())
	}
}

// TestClosingDrainClockRestartsOnProgress is the steady reader: the client
// takes the response over about three times the bound, in pieces, each
// EPOLLOUT edge (handleWritable) moving more of it, and the sweep runs
// between. The conn must survive every sweep, and the whole response must
// arrive. The bound is WriteTimeout, 300 ms; the drain runs for ~1.2 s.
func TestClosingDrainClockRestartsOnProgress(t *testing.T) {
	const bound = 300 * time.Millisecond
	const total = 1 << 20
	r := newClosingRig876(t, resource.Config{ReadTimeout: time.Hour, WriteTimeout: bound}, total)
	r.l.closeWhenFlushed(r.cs)
	if !r.cs.peerClosed {
		t.Fatal("celeris876 PREMISE: closeWhenFlushed did not defer the close")
	}

	start := time.Now()
	steps := 0
	for r.open() && time.Since(start) < 20*time.Second {
		if r.read(64<<10) > 0 {
			steps++
			r.l.handleWritable(r.cs) // the EPOLLOUT edge the read causes
		}
		r.l.checkTimeouts()
		time.Sleep(60 * time.Millisecond)
	}
	elapsed := time.Since(start)
	// What the socket still holds when the conn closes is the client's to
	// read (a close delivers it); what the loop had not sent is lost.
	for r.read(1<<20) > 0 {
	}
	if r.got < total {
		t.Fatalf("the conn closed %v into a drain that was making progress, with %d of %d bytes delivered after "+
			"%d steps and a %v bound: the rest was cut (celeris#876)", elapsed.Round(time.Millisecond), r.got, total,
			steps, bound)
	}
	if elapsed < 2*bound {
		t.Fatalf("celeris876 PREMISE: the drain took %v, not longer than twice the %v bound; the test would pass "+
			"without a restarting clock", elapsed.Round(time.Millisecond), bound)
	}
	if r.open() || r.l.closeCount.Load() != 1 || r.hooks != 1 {
		t.Fatalf("after the response was delivered the conn should have closed once (open=%v closeCount=%d hooks=%d)",
			r.open(), r.l.closeCount.Load(), r.hooks)
	}
	if r.got != total {
		t.Fatalf("the client took %d bytes, want %d", r.got, total)
	}
	t.Logf("drain ran %v over a %v bound in %d steps", elapsed.Round(time.Millisecond), bound, steps)
}

// TestClosingConnWithNoProgressIsStillReaped keeps the fix from making a
// stalled peer immortal: nothing reads, so once the bound has passed since the
// close was asked, the sweep reaps the conn.
func TestClosingConnWithNoProgressIsStillReaped(t *testing.T) {
	const bound = 100 * time.Millisecond
	r := newClosingRig876(t, resource.Config{ReadTimeout: time.Hour, WriteTimeout: bound}, 1<<20)
	r.l.closeWhenFlushed(r.cs)
	r.l.checkTimeouts()
	if !r.open() {
		t.Fatalf("the sweep reaped a conn whose close was asked a moment ago, before the %v bound", bound)
	}
	deadline := time.Now().Add(10 * time.Second)
	for r.open() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		r.l.checkTimeouts()
	}
	if r.open() {
		t.Fatalf("a closing conn whose peer reads nothing was not reaped within 10 s of a %v bound", bound)
	}
	if r.l.closeCount.Load() != 1 || r.hooks != 1 {
		t.Fatalf("closeCount=%d hooks=%d, want 1 and 1", r.l.closeCount.Load(), r.hooks)
	}
}
