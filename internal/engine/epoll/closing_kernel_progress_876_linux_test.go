//go:build linux

package epoll

import (
	"net"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/resource"
)

// celeris#876, review round 3. The drain clock counted progress only when the
// USERSPACE queue shrank, i.e. at an EPOLLOUT edge, and a socket raises
// EPOLLOUT only once about half of its send queue has drained. A client that
// takes a Connection: close response steadily but slowly (a send queue of a
// few MiB on loopback, 64 KiB/s) goes seconds between two edges, so with
// WriteTimeout disabled (5 s floor) or below ReadTimeout it was reaped at the
// bound with the response cut, while main kept the conn until ReadTimeout.
// Any evidence that the peer is taking bytes is progress: here, the kernel's
// count of bytes the peer acknowledged (tcp_info bytes_acked).
//
// These tests need a real TCP socket (tcp_info; a socketpair has none), a
// small send queue on the server side and a small receive buffer on the client
// side. They drive the real closeWhenFlushed and checkTimeouts and call
// handleWritable never, which is what a slow reader looks like to the loop: no
// EPOLLOUT edge yet.

// newTCPClosingRig876 is newClosingRig876 on a loopback TCP pair: the conn the
// loop owns has the largest send buffer an unprivileged socket may ask for
// (net.core.wmem_max, 208 KiB by default; a queue of MiB is the engine's normal
// case, and one the test peer takes minutes to drain) and the peer a 16 KiB
// receive buffer.
func newTCPClosingRig876(t *testing.T, cfg resource.Config, queued int) *closingRig876 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("loopback unavailable: %v", err)
	}
	defer func() { _ = ln.Close() }()
	d := net.Dialer{Timeout: 3 * time.Second, Control: func(_, _ string, c syscall.RawConn) error {
		return c.Control(func(fd uintptr) {
			_ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF, 16<<10)
		})
	}}
	type dialed struct {
		c   net.Conn
		err error
	}
	dc := make(chan dialed, 1)
	go func() { c, err := d.Dial("tcp", ln.Addr().String()); dc <- dialed{c, err} }()
	sc, err := ln.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	cl := <-dc
	if cl.err != nil {
		t.Fatalf("dial: %v", cl.err)
	}
	dup := func(c net.Conn) int {
		f, err := c.(*net.TCPConn).File()
		if err != nil {
			t.Fatalf("File: %v", err)
		}
		fd, err := unix.Dup(int(f.Fd()))
		_ = f.Close()
		_ = c.Close()
		if err != nil {
			t.Fatalf("dup: %v", err)
		}
		if err := unix.SetNonblock(fd, true); err != nil {
			t.Fatalf("nonblock: %v", err)
		}
		return fd
	}
	fd, peer := dup(sc), dup(cl.c)
	_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_SNDBUF, 256<<10)
	return newClosingRigOn876(t, cfg, queued, fd, peer)
}

// TestClosingConnWhoseClientReadsSteadilyIsNotReapedBeforeTheNextEdge is the
// review's defect at the loop: the peer takes 8 KiB every 100 ms (80 KiB/s)
// over five times the 400 ms bound, no EPOLLOUT edge is delivered (handleWritable
// is never called: half of the queue has not drained), and the userspace queue
// stays where it was. The conn must survive every sweep.
func TestClosingConnWhoseClientReadsSteadilyIsNotReapedBeforeTheNextEdge(t *testing.T) {
	const bound = 400 * time.Millisecond
	r := newTCPClosingRig876(t, resource.Config{ReadTimeout: time.Hour, WriteTimeout: bound}, 8<<20)
	r.l.closeWhenFlushed(r.cs)
	if !r.cs.peerClosed || r.cs.closeSince == 0 {
		t.Fatalf("celeris876 PREMISE: closeWhenFlushed did not defer the close (peerClosed=%v closeSince=%d)",
			r.cs.peerClosed, r.cs.closeSince)
	}
	pending := r.cs.pendingBytes

	start := time.Now()
	for time.Since(start) < 5*bound {
		r.read(8 << 10)
		r.l.checkTimeouts()
		if !r.open() {
			t.Fatalf("the conn was reaped %v into a drain whose peer was taking 8 KiB every 100 ms (%d bytes so far), "+
				"with the %v bound and the userspace queue unmoved: the kernel was draining and no EPOLLOUT edge had "+
				"come yet (celeris#876)", time.Since(start).Round(time.Millisecond), r.got, bound)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if r.cs.pendingBytes != pending {
		t.Fatalf("celeris876 PREMISE: the userspace queue moved (%d -> %d), so the userspace progress rule was in play",
			pending, r.cs.pendingBytes)
	}
	if r.got < 100<<10 {
		t.Fatalf("celeris876 PREMISE: the peer took only %d bytes in %v", r.got, time.Since(start))
	}
}

// TestClosingConnWhoseClientStopsReadingIsReapedAtTheBound is the other half,
// on the same rig: a client that read for a while and then takes nothing is
// reaped about a bound after its last byte, so the kernel signal does not make
// a stalled peer immortal.
func TestClosingConnWhoseClientStopsReadingIsReapedAtTheBound(t *testing.T) {
	const bound = 400 * time.Millisecond
	r := newTCPClosingRig876(t, resource.Config{ReadTimeout: time.Hour, WriteTimeout: bound}, 8<<20)
	r.l.closeWhenFlushed(r.cs)
	for i := 0; i < 10; i++ { // a second of reading, past the bound
		r.read(8 << 10)
		r.l.checkTimeouts()
		if !r.open() {
			t.Fatalf("celeris876 PREMISE: the conn was reaped %d reads into the steady phase", i)
		}
		time.Sleep(100 * time.Millisecond)
	}
	stalled := time.Now()
	for r.open() && time.Since(stalled) < 10*time.Second {
		r.l.checkTimeouts()
		time.Sleep(10 * time.Millisecond)
	}
	if r.open() {
		t.Fatalf("a closing conn whose client stopped reading was not reaped within 10 s of a %v bound", bound)
	}
	// One bound for the baseline taken at a sweep and the window the peer's
	// own buffer still held, one more for the sweep cadence.
	if took := time.Since(stalled); took > 3*bound {
		t.Fatalf("the conn lived %v after the client stopped reading; want about the %v bound", took.Round(time.Millisecond), bound)
	}
	if r.l.closeCount.Load() != 1 {
		t.Fatalf("closeCount=%d, want 1", r.l.closeCount.Load())
	}
}
