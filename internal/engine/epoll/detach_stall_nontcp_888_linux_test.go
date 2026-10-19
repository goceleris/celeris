//go:build linux

package epoll

import (
	"sync"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/conn"
)

// TestDetachedStallNeedsAnAckedCount888: the stall bound reads the peer's
// acknowledged byte count (tcp_info), which a socket that is not TCP does not
// report. The stream's own goroutine flushes with no lock the loop's clocks are
// under, so without that count a peer that reads cannot be told from one that
// does not, and the conn is not closed (no bound, as before celeris#888 items
// 38 and 39) rather than on no evidence.
func TestDetachedStallNeedsAnAckedCount888(t *testing.T) {
	l := stall888Loop(t, stall888Bound)
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Skipf("socketpair unavailable: %v", err)
	}
	local, peer := pair[0], pair[1]
	t.Cleanup(func() { _ = unix.Close(peer) })
	if local >= connTableSize {
		_ = unix.Close(local)
		t.Skip("fd out of table range")
	}
	if err := unix.SetNonblock(local, true); err != nil {
		t.Fatalf("set nonblock: %v", err)
	}
	cs := &connState{fd: local, liveIdx: -1}
	cs.h1State = conn.NewH1State()
	cs.h1State.Detached.Store(true)
	cs.detachMu = &sync.Mutex{}
	registerForOut885(t, l, cs)
	l.conns[local] = cs
	l.addLiveConn(cs)
	l.connCount++
	l.activeConns.Add(1)
	l.detachedCount = 1
	t.Cleanup(func() {
		if l.conns[local] == cs {
			l.closeConn(local)
		}
	})

	fillSendBuffer(t, local)
	cs.writeBuf = append(cs.writeBuf, make([]byte, 64<<10)...)
	cs.pendingBytes = len(cs.writeBuf)
	if err := l.flushWrites(cs, true); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if !csWritePending(cs) {
		t.Fatalf("the socket took the queued bytes; backpressure not reproduced")
	}
	if el, closed := sweepUntil(l, local, cs, 3*stall888Bound, nil); closed {
		t.Fatalf("a conn on a socket with no acknowledged count was closed after %v (WriteTimeout %v)", el, stall888Bound)
	}
}
