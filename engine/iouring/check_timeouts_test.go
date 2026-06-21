//go:build linux

package iouring

import (
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/resource"
)

// TestCheckTimeoutsClosesFDZero is the regression guard for v1.5.0 review 1.9.
// A conn at fd 0 must be processed by checkTimeouts like any other. With the
// pre-fix forward `range` over liveConns, swap-remove during the loop caused
// the zeroed tail slot to be read as fd 0 (and real fd-0 conns to be skipped
// or double-visited). It registers conns at fds {0,5,10,15}, marks every
// deadline expired, and asserts ALL FOUR are torn down.
//
// closeConn is short-circuited via cs.sending=true so the deferred-close
// branch sets cs.closing=true WITHOUT issuing any real close syscall on the
// synthetic fds (notably fd 0 = stdin). This still proves the iteration
// reached every registered fd, fd 0 included.
func TestCheckTimeoutsClosesFDZero(t *testing.T) {
	w := &Worker{
		conns:     make([]*connState, 1024),
		liveConns: make([]int, 0, 8),
		errCount:  &atomic.Uint64{},
		// sending=true routes the timeout check to the WriteTimeout branch.
		cfg: resource.Config{WriteTimeout: time.Second},
	}
	past := time.Now().Add(-time.Hour).UnixNano()
	fds := []int{0, 5, 10, 15}
	css := make([]*connState, len(fds))
	for i, fd := range fds {
		cs := &connState{fd: fd, liveIdx: -1, lastActivity: past, sending: true}
		w.conns[fd] = cs
		w.addLiveConn(cs)
		css[i] = cs
		if fd > w.maxFD {
			w.maxFD = fd
		}
	}

	w.checkTimeouts()

	for i, cs := range css {
		if !cs.closing {
			t.Errorf("conn at fd %d was not visited/closed by checkTimeouts (closing=false)", fds[i])
		}
	}
}

// TestCheckTimeoutsSwapRemoveNoSkip exercises the real swap-remove path: each
// expired conn is fully torn down (removeLiveConn shrinks the slice mid-scan).
// Real socketpair fds are used so finishClose's close syscalls are harmless.
// All conns must be removed; none may survive (a forward range would skip the
// swapped-in tail conns).
func TestCheckTimeoutsSwapRemoveNoSkip(t *testing.T) {
	const n = 32
	w := &Worker{
		conns:       make([]*connState, 65536),
		liveConns:   make([]int, 0, n),
		errCount:    &atomic.Uint64{},
		activeConns: &atomic.Int64{},
		closeCount:  &atomic.Uint64{},
		cfg:         resource.Config{IdleTimeout: time.Second},
	}
	past := time.Now().Add(-time.Hour).UnixNano()

	var peers []int
	t.Cleanup(func() {
		for _, p := range peers {
			_ = unix.Close(p)
		}
	})
	registered := 0
	for i := 0; i < n; i++ {
		pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
		if err != nil {
			t.Skipf("socketpair unavailable: %v", err)
		}
		local, peer := pair[0], pair[1]
		peers = append(peers, peer)
		if local >= len(w.conns) {
			_ = unix.Close(local)
			continue
		}
		cs := &connState{fd: local, liveIdx: -1, lastActivity: past}
		w.conns[local] = cs
		w.addLiveConn(cs)
		w.connCount++
		w.activeConns.Add(1)
		if local > w.maxFD {
			w.maxFD = local
		}
		registered++
	}
	if registered == 0 {
		t.Skip("no fds registered")
	}

	w.checkTimeouts()

	if got := len(w.liveConns); got != 0 {
		t.Fatalf("checkTimeouts left %d live conns, want 0 (forward-range skip regression)", got)
	}
	if w.connCount != 0 {
		t.Errorf("connCount = %d, want 0", w.connCount)
	}
}
