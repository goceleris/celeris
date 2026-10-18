//go:build linux

package epoll

import (
	"net"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/conn"
)

// A truly-detached conn (WS/SSE) whose peer takes none of the bytes queued for
// it is closed WriteTimeout after the sweep found them queued (celeris#888
// items 38 and 39, celeris#884 item 69): checkTimeouts skipped a detached conn
// altogether, and a write on a native engine only queues, so a client that
// stayed connected and read nothing held its stream for ever. The clock is
// "the peer took none", not the age of the stream or the last read. These
// tests drive the loop primitives against a loopback TCP pair, because the
// evidence the sweep reads (tcp_info bytes_acked) exists on a TCP socket only.

const stall888Bound = 300 * time.Millisecond

// stall888Conn is a detached conn on the server end of a loopback TCP pair
// whose client end has a small receive buffer, registered with l.
type stall888Conn struct {
	cs   *connState
	peer net.Conn
}

func newStall888Conn(t *testing.T, l *Loop) *stall888Conn {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("loopback unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	d := net.Dialer{Timeout: 2 * time.Second, Control: func(_, _ string, c syscall.RawConn) error {
		return c.Control(func(fd uintptr) {
			_ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF, 4096)
		})
	}}
	peer, err := d.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	srv, err := ln.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	f, err := srv.(*net.TCPConn).File()
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	fd, err := unix.Dup(int(f.Fd()))
	_ = f.Close()
	_ = srv.Close()
	if err != nil {
		t.Fatalf("dup: %v", err)
	}
	if fd >= connTableSize {
		_ = unix.Close(fd)
		t.Skip("fd out of table range")
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		t.Fatalf("set nonblock: %v", err)
	}
	cs := &connState{fd: fd, liveIdx: -1}
	cs.h1State = conn.NewH1State()
	cs.h1State.Detached.Store(true)
	cs.detachMu = &sync.Mutex{}
	registerForOut885(t, l, cs)
	l.conns[fd] = cs
	l.addLiveConn(cs)
	l.connCount++
	l.activeConns.Add(1)
	l.detachedCount = 1
	t.Cleanup(func() {
		if l.conns[fd] == cs {
			l.closeConn(fd)
		}
	})
	return &stall888Conn{cs: cs, peer: peer}
}

// queue appends n bytes to the conn's write buffer and flushes, as the guarded
// writeFn and the dirty pass do, so that what the socket does not take stays
// queued.
func (c *stall888Conn) queue(t *testing.T, l *Loop, n int) {
	t.Helper()
	cs := c.cs
	cs.detachMu.Lock()
	cs.writeBuf = append(cs.writeBuf, make([]byte, n)...)
	cs.pendingBytes = csPendingBytes(cs)
	if err := l.flushWrites(cs, true); err != nil {
		cs.detachMu.Unlock()
		t.Fatalf("flush: %v", err)
	}
	cs.pendingBytes = csPendingBytes(cs)
	cs.detachMu.Unlock()
	if !cs.dirty {
		l.markDirty(cs)
	}
}

// stall888Loop is a reap loop whose WriteTimeout is the bound.
func stall888Loop(t *testing.T, wt time.Duration) *Loop {
	l := newReapLoop(t)
	l.cfg.WriteTimeout = wt
	return l
}

// fillAndQueue fills the socket's buffers until a write parks, then queues a
// further chunk behind it.
func (c *stall888Conn) fillAndQueue(t *testing.T, l *Loop) {
	t.Helper()
	fillSendBuffer(t, c.cs.fd)
	c.queue(t, l, 64<<10)
	if !csWritePending(c.cs) {
		t.Fatalf("the socket took the queued bytes; backpressure not reproduced")
	}
}

// sweepUntil sweeps every 10 ms until the conn is gone or the limit passes,
// and returns how long it took.
func sweepUntil(l *Loop, fd int, cs *connState, limit time.Duration, between func()) (closedAfter time.Duration, closed bool) {
	start := time.Now()
	for time.Since(start) < limit {
		l.checkTimeouts()
		if l.conns[fd] != cs {
			return time.Since(start), true
		}
		if between != nil {
			between()
		}
		time.Sleep(10 * time.Millisecond)
	}
	return time.Since(start), false
}

// TestDetachedPeerThatTakesNothingIsClosed888: no idle deadline, no read, and
// a peer that reads nothing: closed after WriteTimeout, not before.
func TestDetachedPeerThatTakesNothingIsClosed888(t *testing.T) {
	l := stall888Loop(t, stall888Bound)
	c := newStall888Conn(t, l)
	c.fillAndQueue(t, l)
	// A read of the connection (a WebSocket ping, an SSE client's request) is
	// not evidence that the peer takes what is queued: the clock is not keyed
	// on it.
	c.cs.lastActivity = time.Now().UnixNano()

	el, closed := sweepUntil(l, c.cs.fd, c.cs, 5*time.Second, func() { c.cs.lastActivity = time.Now().UnixNano() })
	if !closed {
		t.Fatalf("a detached conn whose peer takes nothing was not closed within %v (WriteTimeout %v)", el, stall888Bound)
	}
	if el < stall888Bound {
		t.Errorf("closed after %v, before WriteTimeout %v", el, stall888Bound)
	}
	t.Logf("closed %v after the first sweep (WriteTimeout %v)", el.Round(time.Millisecond), stall888Bound)
}

// TestDetachedStallIsBoundedWhateverTheIdleDeadlineSays888: a WebSocket peer
// that sends keeps pushing the middleware's idle deadline out; the stall bound
// does not depend on it.
func TestDetachedStallIsBoundedWhateverTheIdleDeadlineSays888(t *testing.T) {
	l := stall888Loop(t, stall888Bound)
	c := newStall888Conn(t, l)
	c.fillAndQueue(t, l)
	push := func() { c.cs.h1State.IdleDeadlineNs.Store(time.Now().Add(time.Hour).UnixNano()) }
	push()
	if el, closed := sweepUntil(l, c.cs.fd, c.cs, 5*time.Second, push); !closed {
		t.Fatalf("a stalled conn with its idle deadline pushed out was not closed within %v", el)
	}
}

// TestDetachedReadingPeerIsNotClosed888: a peer that takes bytes, slowly,
// keeps its conn for several WriteTimeouts, with bytes queued for it the whole
// time. A clock keyed on the last read of the connection, or on its age, would
// close it.
func TestDetachedReadingPeerIsNotClosed888(t *testing.T) {
	l := stall888Loop(t, stall888Bound)
	c := newStall888Conn(t, l)
	c.fillAndQueue(t, l)
	c.cs.lastActivity = time.Now().Add(-time.Hour).UnixNano() // the request is long ago

	buf := make([]byte, 4096)
	tick := 0
	run := 5 * stall888Bound
	el, closed := sweepUntil(l, c.cs.fd, c.cs, run, func() {
		tick++
		if tick%5 == 0 { // every 50 ms: a little
			_ = c.peer.SetReadDeadline(time.Now().Add(5 * time.Millisecond))
			_, _ = c.peer.Read(buf)
		}
		flushDirtyOnce(l)
		if csWritePending(c.cs) && len(c.cs.writeBuf)-c.cs.writePos < 64<<10 {
			c.queue(t, l, 64<<10) // the producer keeps queueing
		}
	})
	if closed {
		t.Fatalf("a peer that kept reading was closed after %v (WriteTimeout %v)", el, stall888Bound)
	}
	if !csWritePending(c.cs) {
		t.Fatalf("nothing was queued at the end: the control did not keep bytes queued")
	}
}

// TestDetachedStallNeedsAWriteTimeout888: WriteTimeout 0 (none, or -1) is no
// bound.
func TestDetachedStallNeedsAWriteTimeout888(t *testing.T) {
	l := stall888Loop(t, 0)
	c := newStall888Conn(t, l)
	c.fillAndQueue(t, l)
	if el, closed := sweepUntil(l, c.cs.fd, c.cs, 3*stall888Bound, nil); closed {
		t.Fatalf("closed after %v with WriteTimeout 0", el)
	}
	if c.cs.closeSince != 0 {
		t.Errorf("a stall clock runs with WriteTimeout 0: closeSince=%d", c.cs.closeSince)
	}
}

// TestDetachedStallClockEndsWithTheQueue888: a conn whose queue drained ends
// its clock, so the next queue is not charged for the time before it.
func TestDetachedStallClockEndsWithTheQueue888(t *testing.T) {
	l := stall888Loop(t, stall888Bound)
	c := newStall888Conn(t, l)
	c.fillAndQueue(t, l)
	l.checkTimeouts()
	if c.cs.closeSince == 0 {
		t.Fatalf("no stall clock after a sweep that found bytes queued")
	}
	// The peer reads it all.
	buf := make([]byte, 64<<10)
	deadline := time.Now().Add(5 * time.Second)
	for csWritePending(c.cs) && time.Now().Before(deadline) {
		_ = c.peer.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
		_, _ = c.peer.Read(buf)
		flushDirtyOnce(l)
	}
	if csWritePending(c.cs) {
		t.Fatalf("the queue did not drain")
	}
	l.checkTimeouts()
	if c.cs.closeSince != 0 {
		t.Errorf("closeSince=%d with nothing queued, want 0", c.cs.closeSince)
	}
	if l.conns[c.cs.fd] != c.cs {
		t.Fatalf("a drained conn was closed")
	}
}
