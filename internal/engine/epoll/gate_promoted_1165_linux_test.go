//go:build linux

package epoll

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/engine"
)

// celeris#1165. closeIdleH1 skipped every conn with a dispatch goroutine
// (asyncPromoted: sticky, set for every conn that ever served an Async route
// and, on a server with any Async route, for every conn whose request once
// arrived in pieces), so such an idle keep-alive conn was held behind another
// conn's send drain for the whole Shutdown budget on a loop with no HTTP/2 conn,
// and for the whole pool wait on one with. It now looks inside the conn as
// io_uring's gate does: closed only when its goroutine is parked with no input,
// nothing is in progress in its parser, nothing is detached or queued; the
// rules below are each pinned by a case that must NOT be closed.

type promoted1165 struct {
	l       *Loop
	cs      *connState
	fd      int
	peer    int
	discons atomic.Int64
}

// newPromoted1165 is a conn as production leaves it after its dispatch goroutine
// answered a request and parked: asyncPromoted, asyncRun and asyncParked set,
// no input, a parser at a request boundary, nothing queued. No goroutine runs:
// the state is what the gate reads.
func newPromoted1165(t *testing.T) *promoted1165 {
	t.Helper()
	r := &promoted1165{}
	r.l = shutdownLoop863(t, func(string) { r.discons.Add(1) })
	r.l.asyncPromoted = &atomic.Uint64{}
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	r.fd, r.peer = pair[0], pair[1]
	t.Cleanup(func() { _ = unix.Close(r.peer) })
	if r.fd >= len(r.l.conns) {
		t.Fatalf("socketpair fd %d outside the test conn table", r.fd)
	}
	cs := acquireConnState(t.Context(), r.fd, 4096, true)
	cs.protocol = engine.HTTP1
	cs.detected = true
	cs.remoteAddr = "127.0.0.1:9"
	cs.writeFn = r.l.makeWriteFn(cs)
	r.l.conns[r.fd] = cs
	r.l.addLiveConn(cs)
	r.l.connCount++
	r.l.activeConns.Add(1)
	r.l.initProtocol(cs)
	cs.asyncPromoted = true
	r.l.asyncPromoted.Add(1)
	cs.asyncInMu.Lock()
	cs.asyncRun, cs.asyncParked = true, true
	cs.asyncInMu.Unlock()
	r.cs = cs
	return r
}

func TestGateClosesAPromotedConnOnlyWhenItIsIdle1165(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(r *promoted1165)
		closed bool
	}{
		{"parked-idle", func(*promoted1165) {}, true},
		{"goroutine-exited-idle", func(r *promoted1165) {
			r.cs.asyncInMu.Lock()
			r.cs.asyncRun, r.cs.asyncParked = false, false
			r.cs.asyncInMu.Unlock()
		}, true},
		{"input-waiting", func(r *promoted1165) {
			r.cs.asyncInMu.Lock()
			r.cs.asyncInBuf = append(r.cs.asyncInBuf, "GET /x HTTP/1.1\r\n"...)
			r.cs.asyncInMu.Unlock()
		}, false},
		{"inside-a-handler", func(r *promoted1165) {
			r.cs.asyncInMu.Lock()
			r.cs.asyncParked = false
			r.cs.asyncInMu.Unlock()
		}, false},
		{"detachMu-held", func(r *promoted1165) { r.cs.detachMu.Lock() }, false},
		{"request-in-progress", func(r *promoted1165) {
			_ = conn.ProcessH1(context.Background(), []byte("GET /x HTTP/1.1\r\nHo"), r.cs.h1State, r.l.handler, func([]byte) {})
			if !r.cs.h1State.HasPendingData() {
				panic("premise: the partial head is not pending data")
			}
		}, false},
		{"switched-to-h2", func(r *promoted1165) {
			r.cs.asyncInMu.Lock()
			r.cs.h2Switched = true
			r.cs.asyncInMu.Unlock()
		}, false},
		{"response-queued", func(r *promoted1165) { r.cs.writeBuf = append(r.cs.writeBuf, 'x') }, false},
		{"detached-stream-running", func(r *promoted1165) { r.cs.h1State.Detached.Store(true) }, false},
		{"detached-stream-ended", func(r *promoted1165) {
			r.cs.h1State.Detached.Store(true)
			r.cs.detachClosed = true
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newPromoted1165(t)
			tc.mutate(r)
			locked := tc.name == "detachMu-held"
			n := r.l.closeIdleH1(false, time.Now().UnixNano())
			if locked {
				r.cs.detachMu.Unlock()
			}
			if tc.closed {
				if n != 1 || r.l.conns[r.fd] != nil || r.discons.Load() != 1 || r.l.closeCount.Load() != 1 || r.l.activeConns.Load() != 0 {
					t.Fatalf("closed=%d live=%v OnDisconnect=%d closeCount=%d activeConns=%d, want 1 closed once",
						n, r.l.conns[r.fd] != nil, r.discons.Load(), r.l.closeCount.Load(), r.l.activeConns.Load())
				}
				return
			}
			if tc.name == "request-in-progress" && !r.cs.h1State.CloseAfter {
				t.Fatal("a request in progress on a promoted conn is not marked close-after: it would be answered with keep-alive")
			}
			if n != 0 || r.l.conns[r.fd] == nil || r.discons.Load() != 0 || r.l.closeCount.Load() != 0 || r.l.activeConns.Load() != 1 {
				t.Fatalf("closed=%d live=%v OnDisconnect=%d closeCount=%d activeConns=%d, want the conn left alone",
					n, r.l.conns[r.fd] != nil, r.discons.Load(), r.l.closeCount.Load(), r.l.activeConns.Load())
			}
		})
	}
}

// The first pass holds, and does not close, a promoted conn whose next request
// is already in the kernel's receive queue (celeris#1104): it is read on, which
// for a promoted conn is a hand-over of the bytes to its goroutine.
func TestGateHoldsAPromotedConnWithAnUnreadRequest1165(t *testing.T) {
	r := newPromoted1165(t)
	if _, err := unix.Write(r.peer, []byte("GET /y HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	if n := r.l.closeIdleH1(true, time.Now().UnixNano()); n != 0 {
		t.Fatalf("the first pass closed %d conns, want the promoted conn with an unread request held", n)
	}
	if r.l.conns[r.fd] == nil || !r.cs.gateRead {
		t.Fatalf("live=%v gateRead=%v, want live and marked to be read on", r.l.conns[r.fd] != nil, r.cs.gateRead)
	}
	r.cs.asyncInMu.Lock()
	got := len(r.cs.asyncInBuf)
	r.cs.asyncInMu.Unlock()
	if got == 0 {
		t.Fatal("the request was not handed to the dispatch goroutine's input")
	}
}
