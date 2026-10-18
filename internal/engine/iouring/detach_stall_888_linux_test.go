//go:build linux

package iouring

import (
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/engine/internal/errclass"
	"github.com/goceleris/celeris/internal/resource"
)

// A truly-detached conn (WS/SSE) whose peer takes none of the bytes queued for
// it is closed WriteTimeout after the sweep found them queued (celeris#888
// items 38 and 39, celeris#884 item 69): checkTimeouts skipped a detached conn
// whatever it had queued, and a write on a native engine only queues, so a
// client that stayed connected and read nothing held its stream for ever. The
// clock is "the peer took none of it", not the age of the stream or the last
// read of the connection. The conn is driven through checkTimeouts and
// completeSend against a loopback TCP pair, since the sweep reads the peer's
// acknowledged byte count from a TCP socket.

const stall888Bound = 300 * time.Millisecond

type stall888 struct {
	w       *Worker
	cs      *connState
	peer    net.Conn
	closeCB atomic.Int32 // OnDetachClose calls
}

func newStall888(t *testing.T, wt time.Duration) *stall888 {
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
	t.Cleanup(func() { _ = unix.Close(fd) })
	if err := unix.SetNonblock(fd, true); err != nil {
		t.Fatalf("set nonblock: %v", err)
	}
	s := &stall888{peer: peer}
	s.w = &Worker{
		conns:       make([]*connState, fd+16),
		liveConns:   make([]int, 0, 4),
		errs:        &errclass.Counters{},
		activeConns: &atomic.Int64{},
		closeCount:  &atomic.Uint64{},
		cfg:         resource.Config{WriteTimeout: wt},
	}
	cs := &connState{fd: fd, liveIdx: -1, detected: true}
	cs.h1State = conn.NewH1State()
	cs.h1State.Detached.Store(true)
	cs.h1State.WSReady.Store(true)
	cs.h1State.OnDetachClose = func() { s.closeCB.Add(1) }
	cs.detachMu = &sync.Mutex{}
	// A SEND the peer is not taking: 1 MiB in flight, nothing taken yet.
	cs.sendBuf = make([]byte, 1<<20)
	cs.sending = true
	s.cs = cs
	s.w.conns[fd] = cs
	s.w.addLiveConn(cs)
	s.w.connCount++
	s.w.activeConns.Add(1)
	s.w.maxFD = fd
	return s
}

// sweepNow is one timeout sweep that takes the stall look: the look has a rate
// limit (stallLookDue), which a test that sweeps back to back must not meet.
func (s *stall888) sweepNow() {
	s.w.lastStallLook = 0
	s.w.checkTimeouts()
}

// sweepUntilClosing sweeps every 10 ms until the conn is closing (closeConn
// deferred its descriptor behind the SEND in flight) or the limit passes.
func (s *stall888) sweepUntilClosing(limit time.Duration, between func()) (after time.Duration, closing bool) {
	start := time.Now()
	for time.Since(start) < limit {
		s.w.checkTimeouts()
		if s.cs.closing {
			return time.Since(start), true
		}
		if between != nil {
			between()
		}
		time.Sleep(10 * time.Millisecond)
	}
	return time.Since(start), false
}

// TestDetachedPeerThatTakesNothingIsClosed888: no idle deadline, and a peer
// that takes nothing: closed after WriteTimeout, not before, and the
// middleware is told (OnDetachClose).
func TestDetachedPeerThatTakesNothingIsClosed888(t *testing.T) {
	s := newStall888(t, stall888Bound)
	// The last read of the connection is fresh and stays so: a WebSocket ping,
	// an SSE client's request. The clock is not keyed on it.
	s.cs.lastActivity = time.Now().UnixNano()
	el, closing := s.sweepUntilClosing(5*time.Second, func() { s.cs.lastActivity = time.Now().UnixNano() })
	if !closing {
		t.Fatalf("a detached conn whose peer takes nothing was not closed within %v (WriteTimeout %v)", el, stall888Bound)
	}
	if el < stall888Bound {
		t.Errorf("closed after %v, before WriteTimeout %v", el, stall888Bound)
	}
	if s.closeCB.Load() != 1 {
		t.Errorf("OnDetachClose ran %d times, want 1: the middleware was not told", s.closeCB.Load())
	}
}

// TestDetachedStallIsBoundedWhateverTheIdleDeadlineSays888: a WebSocket peer
// that sends keeps pushing the middleware's idle deadline out; the stall bound
// does not depend on it.
func TestDetachedStallIsBoundedWhateverTheIdleDeadlineSays888(t *testing.T) {
	s := newStall888(t, stall888Bound)
	push := func() { s.cs.h1State.IdleDeadlineNs.Store(time.Now().Add(time.Hour).UnixNano()) }
	push()
	if el, closing := s.sweepUntilClosing(5*time.Second, push); !closing {
		t.Fatalf("a stalled conn with its idle deadline pushed out was not closed within %v", el)
	}
}

// TestDetachedPeerThatTakesBytesIsNotClosed888: the clock restarts when a SEND
// took bytes (completeSend) and when the peer's acknowledged count moved. A
// conn whose peer keeps taking some is not closed for several WriteTimeouts.
func TestDetachedPeerThatTakesBytesIsNotClosed888(t *testing.T) {
	for _, mode := range []string{"send-completions", "acked-bytes"} {
		t.Run(mode, func(t *testing.T) {
			s := newStall888(t, stall888Bound)
			s.cs.lastActivity = time.Now().Add(-time.Hour).UnixNano() // the request is long ago
			// Fill the socket so that acknowledged bytes are the peer's reads.
			filled := fillSocket888(t, s.cs.fd)
			t.Logf("socket took %d bytes before parking", filled)
			buf := make([]byte, 4096)
			tick := 0
			run := 5 * stall888Bound
			el, closing := s.sweepUntilClosing(run, func() {
				tick++
				if tick%5 != 0 { // every 50 ms
					return
				}
				switch mode {
				case "acked-bytes":
					// The peer reads a little; no completion reaches the worker
					// (an inline write of the stream's goroutine, or one very
					// large SEND, completes later).
					_ = s.peer.SetReadDeadline(time.Now().Add(5 * time.Millisecond))
					_, _ = s.peer.Read(buf)
				case "send-completions":
					// A SEND that took bytes completed and another is queued
					// behind it (zcNotifPending stands in for the re-send, which
					// needs a ring).
					s.cs.detachMu.Lock()
					s.cs.zcNotifPending = true
					s.cs.sendBuf = make([]byte, 1<<20)
					s.cs.sending = true
					s.w.completeSend(s.cs, s.cs.fd, 4096, time.Now().UnixNano(), false)
					s.cs.sending = true
					s.cs.zcNotifPending = false
					s.cs.detachMu.Unlock()
				}
			})
			if closing {
				t.Fatalf("a peer that kept taking bytes was closed after %v (WriteTimeout %v)", el, stall888Bound)
			}
		})
	}
}

// fillSocket888 writes into fd until the kernel refuses more.
func fillSocket888(t *testing.T, fd int) int {
	t.Helper()
	chunk := make([]byte, 32<<10)
	total := 0
	for total < 64<<20 {
		n, err := unix.Write(fd, chunk)
		if err == unix.EAGAIN {
			return total
		}
		if err != nil {
			t.Fatalf("fill write: %v", err)
		}
		total += n
	}
	t.Fatalf("socket never filled")
	return total
}

// TestDetachedStallNeedsAWriteTimeout888: WriteTimeout 0 (none, or -1) is no
// bound.
func TestDetachedStallNeedsAWriteTimeout888(t *testing.T) {
	s := newStall888(t, 0)
	if el, closing := s.sweepUntilClosing(3*stall888Bound, nil); closing {
		t.Fatalf("closed after %v with WriteTimeout 0", el)
	}
}
