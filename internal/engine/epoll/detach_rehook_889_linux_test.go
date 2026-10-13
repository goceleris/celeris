//go:build linux

package epoll

import (
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
)

// A panic out of OnDetach after the guarded writeFn was installed leaves the
// connection open (sync mode, and async mode before promotion: Context.Detach
// does not mark the Context detached then, and the router answers 500). The
// next Detach on that connection runs the hook again. The second run must
// guard the connection's own write function, not the first guard: two guards
// over the same non-reentrant cs.detachMu took the lock twice on the first
// write, which in sync mode is the event-loop thread (#889 item 34, review of
// PR #1014).
func TestOnDetachRunTwiceAfterPanic889(t *testing.T) {
	l := &Loop{}
	cs := &connState{fd: -1, protocol: engine.HTTP1}
	var wrote []string
	cs.writeFn = func(b []byte) { wrote = append(wrote, string(b)) }
	l.initProtocol(cs)
	hook := cs.h1State.OnDetach
	if hook == nil {
		t.Fatal("initProtocol installed no OnDetach")
	}

	// First run: the engine faults after the guard is installed (the state
	// the hook publishes through is gone), and the panic reaches the router.
	h1 := cs.h1State
	cs.h1State = nil
	func() {
		defer func() {
			if recover() == nil {
				t.Error("the injected fault did not panic")
			}
		}()
		hook()
	}()
	cs.h1State = h1
	if l.detachedCount != 0 || cs.detachCounted {
		t.Fatalf("after the panicked run detachedCount=%d counted=%v, want 0 false (the count is undone)", l.detachedCount, cs.detachCounted)
	}

	// Second run on the same conn, then a write through the installed guard.
	done := make(chan struct{})
	go func() {
		defer close(done)
		hook()
		cs.writeFn([]byte("x"))
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("DEADLOCK: the first write after the second OnDetach never returned (guard wrapped in a guard over one detachMu)")
	}
	if len(wrote) != 1 || wrote[0] != "x" {
		t.Errorf("the connection's own write function saw %q, want exactly [x]", wrote)
	}
	if l.detachedCount != 1 {
		t.Errorf("detachedCount = %d after two runs of the hook on one conn, want 1 (closeConn decrements once)", l.detachedCount)
	}
}
