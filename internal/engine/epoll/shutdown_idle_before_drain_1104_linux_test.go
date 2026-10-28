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

// celeris#1104 (the flakes lane; TestShutdownPoolWaitGateRequestsInProgressAnd
// IdleConns885 failed once in CI with an idle keep-alive conn that had no FIN
// 1.5 s into the shutdown). A loop with no HTTP/2 conn has nothing to wait for
// (h2PoolSettled is true at once), so it never raises the pool wait's gate and
// goes straight into shutdown(): phase 2b is a send drain that polls every conn
// with a response still queued until its socket takes it, up to the budget of
// the Shutdown (20 s there), and phase 3, which closes the conns, comes after.
// A client that is slow to read one download so held the FIN of every idle
// keep-alive conn on the same loop for as long as the drain lasted, where the
// loops that have an HTTP/2 conn close theirs 250 ms in (the gate), and where
// net/http's Shutdown closes its idle conns at once.

// idleConn1104 adds an HTTP/1 conn that holds nothing (a keep-alive conn after
// its response) to the loop of r, and returns the peer's descriptor.
func (r *drainRig874) idleConn1104(t *testing.T) (fd, peer int) {
	t.Helper()
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	t.Cleanup(func() { _ = unix.Close(pair[1]) })
	if pair[0] >= len(r.l.conns) {
		t.Fatalf("socketpair fd %d outside the test conn table", pair[0])
	}
	cs := acquireConnState(t.Context(), pair[0], 4096, false)
	cs.protocol = engine.HTTP1
	cs.detected = true
	cs.h1State = conn.NewH1State()
	cs.remoteAddr = "127.0.0.1:10"
	r.l.conns[pair[0]] = cs
	r.l.addLiveConn(cs)
	r.l.connCount++
	r.l.activeConns.Add(1)
	return pair[0], pair[1]
}

func TestShutdownClosesAnIdleConnWhileAnotherConnsSendDrainWaits1104(t *testing.T) {
	const queued = 2 << 20
	r := newDrainRig874(t, queued) // a conn whose peer reads nothing
	_, idlePeer := r.idleConn1104(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	var budget atomic.Pointer[context.Context]
	c := ctx
	budget.Store(&c)
	r.l.drainBudget = &budget

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		r.l.shutdown()
	}()

	// The idle conn's FIN, with the other conn's peer still not reading.
	begin := time.Now()
	fds := []unix.PollFd{{Fd: int32(idlePeer), Events: unix.POLLIN}}
	var fin bool
	for time.Since(begin) < 1500*time.Millisecond && !fin {
		if n, _ := unix.Poll(fds, 50); n > 0 {
			var b [1]byte
			if m, err := unix.Read(idlePeer, b[:]); m == 0 && err == nil {
				fin = true
			}
		}
	}
	took := time.Since(begin).Round(time.Millisecond)
	// Let the drain finish: the stalled conn's peer reads now.
	r.startReader()
	select {
	case <-shutdownDone:
	case <-time.After(20 * time.Second):
		t.Fatal("shutdown did not return once the stalled conn's peer read")
	}
	r.wait(t)
	if !fin {
		t.Fatalf("the idle conn had no FIN %v into the shutdown, while the send drain of another conn of the loop waited for its peer (celeris#1104)", took)
	}
	t.Logf("the idle conn had its FIN %v into the shutdown", took)
}
