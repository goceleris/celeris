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
//
// Since celeris#771 an event carries its registration's generation, so the
// worker routes a driver event by that and not by hasDriverConns alone, and
// lookupDriver waits on driverMu, which RegisterConn holds from the map insert
// to the ADD: the order of the two inside the lock no longer loses the byte
// (the aa2331d order, checked under celeris#885 item 9). What this test still
// catches is an ADD that leaves driverMu before the conn is in the map, and a
// callback that is never delivered.

import (
	"sync"
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
	stopSignal := sync.OnceFunc(func() { close(done) }) // also on the Fatal paths: the signaller must not outlive the test
	t.Cleanup(stopSignal)
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

	// A registration that has not delivered its byte within lostBound is
	// lost for good: the event is edge triggered and does not come again. The
	// bound is seconds, not tens of milliseconds, so a loaded or race-built
	// host that is merely slow to run the callback is not counted as a loss
	// (celeris#885 item 9). The run stops at the first loss, after the conn is
	// unregistered and closed as every pass does: one loss fails the test, and
	// a defect that loses every registration then costs one bound, not
	// registrations of them.
	const registrations = 5000
	const lostBound = 5 * time.Second
	lost, attempts := 0, 0
	for i := 0; i < registrations && lost == 0; i++ {
		if l.hasDriverConns.Load() {
			t.Fatal("precondition: the loop has a driver conn, so the lookup would wait for the register")
		}
		attempts++
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
		timer := time.NewTimer(lostBound)
		select {
		case <-got:
		case <-timer.C:
			lost++
		}
		timer.Stop()
		if err := wl.UnregisterConn(local); err != nil {
			t.Fatalf("UnregisterConn after attempt %d (lost=%d): %v", attempts, lost, err)
		}
		_ = unix.Close(local)
		_ = unix.Close(peer)
	}
	stopSignal()
	t.Logf("CELERIS770 attempts=%d of %d registrations, byte queued before RegisterConn never reached onRecv=%d within %v (wakeups signalled=%d)",
		attempts, registrations, lost, lostBound, <-woken)
	if lost > 0 {
		t.Errorf("registration %d of %d never delivered the byte its peer wrote before RegisterConn, in %v (celeris#770)", attempts, registrations, lostBound)
	}
}
