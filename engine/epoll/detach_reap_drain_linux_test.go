//go:build linux

package epoll

import (
	"bytes"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/resource"
)

// newReapLoop builds a bare Loop carrying only the state closeConn and
// checkTimeouts touch, mirroring the construction used by the other
// in-package loop tests.
func newReapLoop(t *testing.T) *Loop {
	t.Helper()
	epfd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		t.Skipf("epoll_create1 unavailable: %v", err)
	}
	t.Cleanup(func() { _ = unix.Close(epfd) })
	return &Loop{
		epollFD:      epfd,
		conns:        make([]*connState, connTableSize),
		liveConns:    make([]int, 0, 4),
		activeConns:  &atomic.Int64{},
		closeCount:   &atomic.Uint64{},
		acceptCount:  &atomic.Uint64{},
		bytesRead:    &atomic.Uint64{},
		bytesWritten: &atomic.Uint64{},
		eventFD:      -1,
		timerFD:      -1,
		cfg:          resource.Config{},
	}
}

// flushDirtyOnce replays the event loop's dirty-list flush pass (run(),
// loop.go) for the detached case: flush under detachMu, drop the conn off
// the list once drained, and leave a partial write on the list (truly
// detached conns are never handed to EPOLLOUT). Lets the test advance a
// connection exactly as one loop iteration would without standing up a
// full engine.
func flushDirtyOnce(l *Loop) {
	for cs := l.dirtyHead; cs != nil; {
		next := cs.dirtyNext
		if mu := cs.detachMu; mu != nil {
			mu.Lock()
		}
		err := l.flushWrites(cs, true)
		switch {
		case err != nil:
			if mu := cs.detachMu; mu != nil {
				mu.Unlock()
			}
			l.removeDirty(cs)
			l.closeConn(cs.fd)
		case !csWritePending(cs):
			cs.pendingBytes = 0
			if mu := cs.detachMu; mu != nil {
				mu.Unlock()
			}
			l.removeDirty(cs)
			if cs.peerClosed {
				l.closeConn(cs.fd)
			}
		default:
			cs.pendingBytes = csPendingBytes(cs)
			if mu := cs.detachMu; mu != nil {
				mu.Unlock()
			}
		}
		cs = next
	}
}

// fillSendBuffer writes into fd until the kernel refuses more, so a
// subsequent write parks on EAGAIN. Returns the number of bytes queued.
func fillSendBuffer(t *testing.T, fd int) int {
	t.Helper()
	chunk := make([]byte, 32<<10)
	total := 0
	for total < 32<<20 {
		n, err := unix.Write(fd, chunk)
		if err == unix.EAGAIN || err == unix.EWOULDBLOCK {
			return total
		}
		if err != nil {
			t.Fatalf("fill write: %v", err)
		}
		total += n
	}
	t.Fatalf("socket never filled after %d bytes", total)
	return total
}

// drainPeer reads everything currently buffered on fd into dst, stopping at
// EAGAIN. Reports whether the peer saw EOF (server closed).
func drainPeer(t *testing.T, fd int, dst *bytes.Buffer) bool {
	t.Helper()
	buf := make([]byte, 64<<10)
	for {
		n, err := unix.Read(fd, buf)
		if err == unix.EAGAIN || err == unix.EWOULDBLOCK {
			return false
		}
		if err != nil {
			t.Fatalf("peer read: %v", err)
		}
		if n == 0 {
			return true
		}
		dst.Write(buf[:n])
	}
}

// TestDetachedIdleReapDrainsTerminalChunk pins celeris#498 item 5: the
// idle-deadline reap must not FIN a truly-detached connection out from
// under bytes it has already accepted from the middleware.
//
// Both native-engine stream middlewares end a stream by queueing the
// terminal bytes and then asking the engine to drop the conn via
// SetWSIdleDeadline(1) — the SSE last event (celeris#494) and the WS close
// echo. When the peer is behind, that write parks on EAGAIN and the
// remainder sits in cs.writeBuf on the dirty list. closeConn's SHUT_WR only
// commits what the kernel already took, so a reap that fires before the
// dirty flush drains the conn silently truncates the stream.
//
// The scenario is driven at the loop-primitive level: fill the socket, stage
// the terminal chunk behind the resulting EAGAIN, expire the deadline, and
// only then let the peer catch up — the shape of a slow client on a stream
// that just ended.
func TestDetachedIdleReapDrainsTerminalChunk(t *testing.T) {
	const terminal = "event: end\ndata: bye\n\n"

	l := newReapLoop(t)

	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Skipf("socketpair unavailable: %v", err)
	}
	local, peer := pair[0], pair[1]
	t.Cleanup(func() { _ = unix.Close(peer) })
	if local >= connTableSize {
		_ = unix.Close(local)
		_ = unix.Close(peer)
		t.Skip("fd out of table range")
	}
	if err := unix.SetNonblock(local, true); err != nil {
		t.Fatalf("set local nonblock: %v", err)
	}
	if err := unix.SetNonblock(peer, true); err != nil {
		t.Fatalf("set peer nonblock: %v", err)
	}

	// A truly-detached conn: OnDetach installed detachMu and published
	// Detached, so checkTimeouts honours only the middleware's deadline.
	cs := &connState{fd: local, liveIdx: -1}
	cs.h1State = conn.NewH1State()
	cs.h1State.Detached.Store(true)
	cs.detachMu = &sync.Mutex{}
	l.conns[local] = cs
	l.addLiveConn(cs)
	l.connCount++
	l.activeConns.Add(1)
	l.detachedCount = 1

	// The stream body the peer has not read yet.
	fillSendBuffer(t, local)

	// The middleware queues its terminal chunk. The inline flush the
	// guarded writeFn performs parks on EAGAIN, leaving the remainder on
	// the dirty list for the event loop to finish.
	cs.writeBuf = append(cs.writeBuf, terminal...)
	cs.pendingBytes = len(terminal)
	l.markDirty(cs)
	if err := l.flushWrites(cs, true); err != nil {
		t.Fatalf("staging flush: %v", err)
	}
	if !csWritePending(cs) {
		t.Fatalf("socket absorbed the terminal chunk; backpressure not reproduced")
	}

	// ... and then asks the engine to reap the conn.
	cs.h1State.IdleDeadlineNs.Store(1)

	l.checkTimeouts()

	// The peer catches up. Anything still queued must reach it before FIN.
	var got bytes.Buffer
	eof := drainPeer(t, peer, &got)
	for i := 0; i < 16 && l.conns[local] != nil; i++ {
		flushDirtyOnce(l)
		l.checkTimeouts()
		if drainPeer(t, peer, &got) {
			eof = true
		}
	}
	if l.conns[local] != nil {
		t.Fatalf("conn still live after 16 sweeps past its idle deadline")
	}
	for i := 0; i < 16 && !eof; i++ {
		eof = drainPeer(t, peer, &got)
	}
	if !eof {
		t.Fatalf("peer never saw EOF after the reap")
	}
	if !bytes.HasSuffix(got.Bytes(), []byte(terminal)) {
		t.Fatalf("terminal chunk dropped by the idle-deadline reap: peer got %d bytes ending %q, want them to end with %q",
			got.Len(), tailOf(got.Bytes(), len(terminal)), terminal)
	}
}

// TestDetachedIdleReapDrainIsBounded pins the other half of the drain
// defer: a peer that never reads again must still lose its connection.
// Deferring on pending writes with no ceiling would strand the fd AND hold
// the loop at a 0 ms epoll_wait forever, since a truly-detached conn keeps
// its remainder on the dirty list. It also pins that a middleware which
// pushes the idle deadline back out (a WS frame) clears the grace stamp, so
// the next expiry gets a full window rather than an already-spent one.
func TestDetachedIdleReapDrainIsBounded(t *testing.T) {
	l := newReapLoop(t)

	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Skipf("socketpair unavailable: %v", err)
	}
	local, peer := pair[0], pair[1]
	t.Cleanup(func() { _ = unix.Close(peer) })
	if local >= connTableSize {
		_ = unix.Close(local)
		_ = unix.Close(peer)
		t.Skip("fd out of table range")
	}
	if err := unix.SetNonblock(local, true); err != nil {
		t.Fatalf("set local nonblock: %v", err)
	}

	cs := &connState{fd: local, liveIdx: -1}
	cs.h1State = conn.NewH1State()
	cs.h1State.Detached.Store(true)
	cs.detachMu = &sync.Mutex{}
	l.conns[local] = cs
	l.addLiveConn(cs)
	l.connCount++
	l.activeConns.Add(1)
	l.detachedCount = 1

	fillSendBuffer(t, local)
	cs.writeBuf = append(cs.writeBuf, "event: end\ndata: bye\n\n"...)
	cs.pendingBytes = len(cs.writeBuf)
	l.markDirty(cs)
	if err := l.flushWrites(cs, true); err != nil {
		t.Fatalf("staging flush: %v", err)
	}
	if !csWritePending(cs) {
		t.Fatalf("socket absorbed the terminal chunk; backpressure not reproduced")
	}

	cs.h1State.IdleDeadlineNs.Store(1)
	l.checkTimeouts()
	if l.conns[local] == nil {
		t.Fatalf("reap closed the conn with bytes still queued")
	}
	if cs.drainDeadline == 0 {
		t.Fatalf("deferred reap did not stamp a drain deadline")
	}

	// Middleware extends the idle deadline: the stamp must not survive it.
	cs.h1State.IdleDeadlineNs.Store(time.Now().Add(time.Hour).UnixNano())
	l.checkTimeouts()
	if l.conns[local] == nil {
		t.Fatalf("conn closed while its idle deadline was in the future")
	}
	if cs.drainDeadline != 0 {
		t.Fatalf("drainDeadline = %d after the middleware refreshed the idle deadline, want 0", cs.drainDeadline)
	}

	// Expire it again, then let the grace window lapse with the peer still
	// not reading: the conn must go.
	cs.h1State.IdleDeadlineNs.Store(1)
	l.checkTimeouts()
	if cs.drainDeadline == 0 {
		t.Fatalf("second expiry did not re-stamp a drain deadline")
	}
	cs.drainDeadline = time.Now().Add(-time.Second).UnixNano()
	l.checkTimeouts()
	if l.conns[local] != nil {
		t.Fatalf("conn survived the drain grace with a peer that never reads")
	}
	if l.connCount != 0 || len(l.liveConns) != 0 {
		t.Errorf("connCount=%d liveConns=%d after close, want 0/0", l.connCount, len(l.liveConns))
	}
}

// tailOf returns the last n bytes of b (all of b when shorter), for
// failure messages that must not dump a multi-megabyte stream body.
func tailOf(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return string(b)
}
