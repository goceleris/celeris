//go:build linux

package iouring

import (
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/resource"
)

// newClosingDrainWorker returns a synthetic worker holding one connection in
// the state closeConn's deferred-close branch leaves behind: the response tail
// queued and a SEND in the kernel. A real socketpair fd is used so
// finishClose's shutdown/drain/close syscalls are harmless, and the peer end
// is kept open and never read — the wedge this exercises.
func newClosingDrainWorker(t *testing.T, ring *Ring) (*Worker, *connState) {
	t.Helper()
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Skipf("socketpair unavailable: %v", err)
	}
	local, peer := pair[0], pair[1]
	t.Cleanup(func() { _ = unix.Close(peer) })

	w := &Worker{
		ring:        ring,
		conns:       make([]*connState, local+1),
		liveConns:   make([]int, 0, 4),
		errCount:    &atomic.Uint64{},
		activeConns: &atomic.Int64{},
		closeCount:  &atomic.Uint64{},
		cfg:         resource.Config{IdleTimeout: time.Second},
	}
	w.cachedNow = time.Now().UnixNano()
	cs := &connState{
		fd:             local,
		liveIdx:        -1,
		generation:     7,
		sending:        true,
		kernelInflight: 1,
		sendBuf:        []byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"),
		// Idle for an hour: the conn is being closed BECAUSE it timed out.
		lastActivity: time.Now().Add(-time.Hour).UnixNano(),
	}
	w.conns[local] = cs
	w.addLiveConn(cs)
	w.connCount = 1
	w.activeConns.Add(1)
	return w, cs
}

// TestCheckTimeoutsReapsWedgedClosingConn is the regression guard for
// celeris#498 item 4.
//
// closeConn defers the fd close while sends are outstanding so the last bytes
// (GOAWAY / RST_STREAM / WS close-echo) reach the client, and the only exit is
// the SEND's own CQE. A peer that stops reading never lets that SEND complete,
// and checkTimeouts skipped every cs.closing conn — so the fd, the connState
// and the activeConns slot were pinned until the peer eventually disconnected.
// The sweep must bound that wait.
func TestCheckTimeoutsReapsWedgedClosingConn(t *testing.T) {
	ring := newTestRing(t)
	w, cs := newClosingDrainWorker(t, ring)
	fd := cs.fd
	// Parked by the deferred-close branch, with the drain deadline base
	// stamped long enough ago that no plausible bound is still running.
	cs.closing = true
	cs.lastActivity = time.Now().Add(-time.Hour).UnixNano()

	before := ring.Pending()
	w.checkTimeouts()

	if w.conns[fd] != nil {
		t.Fatalf("checkTimeouts left the wedged closing conn registered at fd %d", fd)
	}
	if got := len(w.liveConns); got != 0 {
		t.Errorf("liveConns = %d, want 0 (wedged closing conn not removed)", got)
	}
	if w.connCount != 0 {
		t.Errorf("connCount = %d, want 0", w.connCount)
	}
	if got := w.activeConns.Load(); got != 0 {
		t.Errorf("activeConns = %d, want 0", got)
	}
	// The reap must go through the cancel-then-release discipline, not merely
	// close the fd: the SEND still targets cs.sendBuf, so its ASYNC_CANCEL is
	// what makes the terminal CQE — and therefore the connState release —
	// prompt instead of leaving it to the 5 s pendingRelease backstop.
	if after := ring.Pending(); after <= before {
		t.Errorf("reap submitted no ASYNC_CANCEL for the in-flight SEND: ring pending %d→%d", before, after)
	}
	if len(w.pendingRelease) != 1 {
		t.Errorf("pendingRelease = %d entries, want 1 (connState not queued for deferred release)", len(w.pendingRelease))
	}
}

// TestCheckTimeoutsLetsFreshClosingConnDrain guards the other half: the reap
// must not fight the normal completion path. closeConn most often defers a
// close that a timeout just triggered, so the conn enters the closing state
// with an already-expired lastActivity. Unless closeConn restamps the drain
// deadline base, the very next sweep tears the conn down before the queued
// bytes can reach the kernel — exactly the flush the deferred close exists for.
func TestCheckTimeoutsLetsFreshClosingConnDrain(t *testing.T) {
	ring := newTestRing(t)
	w, cs := newClosingDrainWorker(t, ring)
	fd := cs.fd
	base := w.cachedNow

	w.closeConn(fd)
	if !cs.closing {
		t.Fatalf("closeConn did not defer the close (closing=false) — the test no longer models the wedge")
	}
	if cs.lastActivity < base {
		t.Fatalf("closeConn left the drain deadline base %v stale; the drain is over before it starts",
			time.Duration(base-cs.lastActivity))
	}

	w.checkTimeouts()

	if w.conns[fd] != cs {
		t.Fatalf("checkTimeouts reaped a conn that had only just entered the drain; the queued bytes never got a chance to flush")
	}
	if got := len(w.liveConns); got != 1 {
		t.Errorf("liveConns = %d, want 1", got)
	}
}
