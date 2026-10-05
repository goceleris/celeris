//go:build linux

package epoll

// celeris#770. RegisterConn armed the descriptor (EPOLL_CTL_ADD, edge
// triggered) before it put the conn in driverConns and set hasDriverConns.
// The worker looks a driver conn up only while hasDriverConns is set, so on a
// loop with no other driver conn, a worker that took the conn's first event in
// between handed it to the HTTP path, which found no connection and read
// nothing. Edge triggered, the event does not come again: bytes the peer wrote
// before the register never reached onRecv until more arrived.
//
// The window is a few instructions wide, so this test counts: registrations
// of a conn whose peer has already written a byte, on a loop with no other
// driver conn, while another goroutine signals the loop's wakeup eventfd in a
// loop, so the worker keeps returning from epoll_wait. (HTTP connections
// churning on the loop keep it awake too, but the worker's closeConn writes
// l.conns without driverMu while RegisterConn reads it under driverMu, which
// the race detector reports.)

import (
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestRegisterConnDeliversBytesQueuedBeforeIt(t *testing.T) {
	eng, stop := newTestEngine(t)
	t.Cleanup(stop)
	wl := eng.WorkerLoop(0)
	l := eng.loops[0]

	done := make(chan struct{})
	woken := make(chan int, 1)
	go func() {
		n := 0
		defer func() { woken <- n }()
		for {
			select {
			case <-done:
				return
			default:
			}
			l.wakeFD.Signal()
			n++
		}
	}()

	const registrations = 5000
	lost := 0
	for i := 0; i < registrations; i++ {
		if l.hasDriverConns.Load() {
			t.Fatal("precondition: the loop has a driver conn, so the lookup would wait for the register")
		}
		local, peer := socketpairNonblocking(t)
		if _, err := unix.Write(peer, []byte{'x'}); err != nil {
			t.Fatalf("write peer: %v", err)
		}
		got := make(chan struct{}, 1)
		if err := wl.RegisterConn(local, func([]byte) {
			select {
			case got <- struct{}{}:
			default:
			}
		}, func(error) {}); err != nil {
			t.Fatalf("RegisterConn: %v", err)
		}
		select {
		case <-got:
		case <-time.After(200 * time.Millisecond):
			lost++
		}
		if err := wl.UnregisterConn(local); err != nil {
			t.Fatalf("UnregisterConn: %v", err)
		}
		_ = unix.Close(local)
		_ = unix.Close(peer)
	}
	close(done)
	t.Logf("CELERIS770 registrations=%d byte queued before RegisterConn never reached onRecv=%d (wakeups signalled=%d)", registrations, lost, <-woken)
	if lost > 0 {
		t.Errorf("%d of %d registrations never delivered the byte their peer wrote before RegisterConn (celeris#770)", lost, registrations)
	}
}
