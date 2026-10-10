//go:build linux

package iouring

import (
	"errors"
	"testing"
	"time"
)

// celeris#867: a recv-end error (peer FIN, recv failure) that meets a running
// async handler is parked in cs.closeErr and delivered to the detached
// middleware (OnError) by closeConn when the close actually runs. Worker.shutdown
// finishes the deferred close itself when it gets there first, and did not
// deliver it: OnError was never called. And a second recv-end error for the
// same conn replaced the first.

var (
	errFirst867  = errors.New("first recv-end error")
	errSecond867 = errors.New("second recv-end error")
)

// parkRecvEnd867 leaves the conn as a recv-end error that met a running
// handler leaves it: the handler holds detachMu, closeOnRecvEnd parks the
// error and asks for the close (closeOwed). The returned release is the
// handler returning. With keepRing false the worker has no ring, so shutdown
// can run on it (the rig's ring stays open for its own cleanup).
func parkRecvEnd867(t *testing.T, keepRing bool, errs ...error) (rig *stallRig704, release func()) {
	t.Helper()
	rig = newStallRig704(t)
	if !keepRing {
		rig.w.ring = nil
	}
	rig.w.listenFD = -1
	release = holdAsHandler704(t, rig.cs, true)
	for _, err := range errs {
		rig.w.closeOnRecvEnd(rig.local, rig.cs, err)
	}
	if rig.w.conns[rig.local] != rig.cs {
		t.Fatal("setup: the conn closed while its handler was running")
	}
	if len(errs) > 0 && rig.cs.closeErr == nil {
		t.Fatal("setup: the recv-end error was not parked")
	}
	return rig, release
}

// shutdownWithin867 runs shutdown on its own goroutine: RULE 10, a change
// that takes a lock the handler holds is shown not to wedge. The handler is
// released first, as shutdown waits for detachMu like every close does.
func shutdownWithin867(t *testing.T, rig *stallRig704, release func()) {
	t.Helper()
	release()
	done := make(chan struct{})
	go func() {
		defer close(done)
		rig.w.shutdown()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Worker.shutdown did not return within 10s")
	}
}

func TestShutdownDeliversTheParkedRecvEndError867(t *testing.T) {
	for _, tc := range []struct {
		name string
		errs []error
		want []error
	}{
		{"one-error", []error{errFirst867}, []error{errFirst867}},
		{"second-error-does-not-replace-the-first", []error{errFirst867, errSecond867}, []error{errFirst867}},
		{"no-error-parked-nothing-delivered", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig, release := parkRecvEnd867(t, false, tc.errs...)
			shutdownWithin867(t, rig, release)
			got := rig.notified
			t.Logf("celeris867 case=%s OnError calls=%v closeErr_after=%v", tc.name, got, rig.cs.closeErr)
			if len(got) != len(tc.want) {
				t.Fatalf("OnError was called %d times with %v, want %d times with %v", len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("OnError call %d got %v, want %v", i, got[i], tc.want[i])
				}
			}
			if rig.cs.closeErr != nil {
				t.Errorf("closeErr = %v after shutdown, want it consumed", rig.cs.closeErr)
			}
		})
	}
}

// TestCloseConnDeliversTheParkedRecvEndError867 is the control the shutdown
// path must match: the dispatch goroutine's exit hands the conn back and
// closeConn delivers the parked error, once, first error kept.
func TestCloseConnDeliversTheParkedRecvEndError867(t *testing.T) {
	rig, release := parkRecvEnd867(t, true, errFirst867, errSecond867)
	release()
	rig.w.closeConn(rig.local)
	if len(rig.notified) != 1 || rig.notified[0] != errFirst867 {
		t.Fatalf("closeConn delivered %v, want exactly [%v]", rig.notified, errFirst867)
	}
}
