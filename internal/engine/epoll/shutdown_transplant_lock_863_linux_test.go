//go:build linux

package epoll

import (
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestCloseDeferredTransplantsHoldsNoLockAcrossTheClose is the RULE 10 check
// for the one lock closeDeferredTransplants takes besides the connState's own
// detachMu: detachQMu, read under, with nothing else held. Producers that
// take detachQMu (every enqueueDetach: a dispatch goroutine, a detached
// WS/SSE callback, the H2 write queue) keep running through shutdown, so the
// pass must neither wait on them while holding anything they need, nor
// invert the asyncInMu -> detachQMu order. Hammered under -race with a
// deadline; a hang fails the test.
func TestCloseDeferredTransplantsHoldsNoLockAcrossTheClose(t *testing.T) {
	l := shutdownLoop863(t, func(string) {})
	cs, fd := pendingTransplant(t, l)
	other := &connState{fd: -1}

	stop := make(chan struct{})
	done := make(chan struct{})
	producers := 4
	for range producers {
		go func() {
			defer func() { done <- struct{}{} }()
			for {
				select {
				case <-stop:
					return
				default:
				}
				other.asyncInMu.Lock() // the order a goroutine's park takes: asyncInMu, then detachQMu
				l.enqueueDetach(other)
				other.asyncInMu.Unlock()
			}
		}()
	}
	l.detachQMu.Lock()
	l.detachQueue = append(l.detachQueue, cs)
	l.detachQPending.Store(1)
	l.detachQMu.Unlock()

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		l.closeDeferredTransplants()
	}()
	select {
	case <-finished:
	case <-time.After(20 * time.Second):
		t.Fatal("closeDeferredTransplants did not return while producers hammered detachQMu: a lock is held across the close")
	}
	close(stop)
	for range producers {
		<-done
	}
	if fdIsOpen(fd) {
		_ = unix.Close(fd)
		t.Error("the owed conn's descriptor is still open")
	}
	if l.transplantInFlight != 0 {
		t.Errorf("transplantInFlight = %d, want 0", l.transplantInFlight)
	}
}
