//go:build linux

package iouring

import (
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/resource"
)

// TestPeerCloseSurfacesEOF guards celeris#564.
//
// A recv completion of zero bytes is the peer's orderly shutdown. handleRecv
// used to hand it to errIORingRecv along with every genuine failure, and
// unix.Errno(uint32(-0)) is unix.Errno(0) — an "error" that prints as
// "errno 0" and satisfies none of the checks callers make. The WebSocket
// middleware stores it verbatim, hands it back from ReadMessage, and every
// classifier that asks errors.Is(err, io.EOF) says no, so an ordinary
// disconnect was reported as a protocol error.
//
// The epoll loop has always had a dedicated branch for the zero-byte read.
// This asserts io_uring agrees: what reaches middleware on a peer close must
// satisfy errors.Is(err, io.EOF).
func TestPeerCloseSurfacesEOF(t *testing.T) {
	ring := newTestRing(t)

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

	var mu sync.Mutex
	var got error
	h1 := &conn.H1State{OnError: func(e error) {
		mu.Lock()
		got = e
		mu.Unlock()
	}}
	cs := &connState{
		fd:         local,
		liveIdx:    -1,
		generation: 7,
		detachMu:   &sync.Mutex{},
		h1State:    h1,
	}
	h1.Detached.Store(true)
	w.conns[local] = cs
	w.addLiveConn(cs)
	w.connCount = 1
	w.activeConns.Add(1)

	w.handleRecv(&completionEntry{Res: 0}, local, w.cachedNow)

	mu.Lock()
	defer mu.Unlock()
	if got == nil {
		t.Fatal("peer close was never surfaced to detached middleware")
	}
	if !errors.Is(got, io.EOF) {
		t.Fatalf("peer close surfaced as %q (%T); middleware classifies a clean "+
			"disconnect with errors.Is(err, io.EOF), so this is scored as a "+
			"protocol error (celeris#564)", got, got)
	}
}
