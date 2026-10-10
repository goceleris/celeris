//go:build linux

package epoll

import (
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/resource"
)

// TestRepeatedCloseRequestsDoNotRestartTheDrainClock is the invariant the
// bound rests on: only send progress moves closeSince. An H2 conn whose write
// was refused stays on h2Conns with writeRefused set, so the run loop's h2Conns
// pass re-enters closeWhenFlushed for every frame a handler goes on queueing,
// and an EPOLLET RDHUP bit is reported again with each later EPOLLOUT edge, so
// onPeerHalfClose re-enters markClosing. None of those is progress. The clock
// is aged by hand, no sleep, and must come back unchanged.
func TestRepeatedCloseRequestsDoNotRestartTheDrainClock(t *testing.T) {
	r := newClosingRig876(t, resource.Config{ReadTimeout: time.Hour, WriteTimeout: time.Minute}, 1<<20)
	r.l.closeWhenFlushed(r.cs)
	if !r.cs.peerClosed || r.cs.closeSince == 0 {
		t.Fatalf("celeris876 PREMISE: closeWhenFlushed did not start the clock (peerClosed=%v closeSince=%d)",
			r.cs.peerClosed, r.cs.closeSince)
	}
	aged := time.Now().Add(-10 * time.Second).UnixNano()
	r.cs.closeSince = aged

	for i := 0; i < 3; i++ {
		r.cs.writeBuf = append(r.cs.writeBuf, make([]byte, 100)...) // a frame queued behind the rest
		r.l.closeWhenFlushed(r.cs)
		if r.cs.closeSince != aged {
			t.Fatalf("closeWhenFlushed, called again with no send progress (call %d), moved closeSince by %v: "+
				"a conn whose handler keeps queueing frames would never reach its bound (celeris#876)",
				i+1, time.Duration(r.cs.closeSince-aged))
		}
	}
	r.l.onPeerHalfClose(r.fd)
	if r.cs.closeSince != aged {
		t.Fatalf("onPeerHalfClose, on a conn already closing and with no send progress, moved closeSince by %v (celeris#876)",
			time.Duration(r.cs.closeSince-aged))
	}
	// A flush that takes bytes is still progress.
	r.read(256 << 10)
	r.l.handleWritable(r.cs)
	if r.cs.closeSince == aged {
		t.Fatal("celeris876 PREMISE: a flush that took bytes did not restart the clock")
	}
}
