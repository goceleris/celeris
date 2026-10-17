//go:build linux

package epoll

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/conn"
)

// celeris#867, epoll half (Refs #885 item 34). A read-end error (peer FIN, read
// failure) that meets a running async handler is parked in cs.closeErr and
// delivered to the detached middleware (OnError) by closeConn when the close
// actually runs. Loop.shutdown finishes a deferred close itself when it gets
// there first, and never delivered it: OnError was not called. And a second
// read-end error for the same conn replaced the first (closeOnReadEnd stored
// unconditionally). The io_uring twin is shutdown_closeerr_867_linux_test.go
// in that package.

var (
	errFirst867  = errors.New("first read-end error")
	errSecond867 = errors.New("second read-end error")
)

// parkReadEnd867 leaves a conn as a read-end error that met a running
// handler leaves it: the handler holds detachMu, closeOnReadEnd parks the
// error and asks for the close (closeConn leaves it to the dispatch
// goroutine: closeOwed). The returned release is the handler returning. The
// conn is a bare async connState in a shutdownLoop863 (no dispatch goroutine
// to join); the returned notified slice is OnError's record.
func parkReadEnd867(t *testing.T, errs ...error) (l *Loop, cs *connState, fd int, release func(), notified func() []error) {
	t.Helper()
	l = shutdownLoop863(t, nil)
	fd = socketpairFD(t, l)
	cs = &connState{fd: fd, liveIdx: -1}
	cs.detachMu = &sync.Mutex{}
	cs.asyncCond.L = &cs.asyncInMu
	cs.asyncPromoted = true
	cs.h1State = conn.NewH1State()
	var mu sync.Mutex
	var got []error
	cs.h1State.OnError = func(err error) {
		mu.Lock()
		got = append(got, err)
		mu.Unlock()
	}
	l.conns[fd] = cs
	l.addLiveConn(cs)
	l.connCount++
	l.activeConns.Add(1)

	release = holdAsHandler(t, cs, true)
	for _, err := range errs {
		l.closeOnReadEnd(fd, cs, err)
	}
	if l.conns[fd] != cs {
		t.Fatal("setup: the conn closed while its handler was running")
	}
	if len(errs) > 0 && cs.closeErr == nil {
		t.Fatal("setup: the read-end error was not parked")
	}
	return l, cs, fd, release, func() []error {
		mu.Lock()
		defer mu.Unlock()
		return append([]error(nil), got...)
	}
}

// shutdownWithin867 runs shutdown on its own goroutine (RULE 10: a change that
// takes a lock the handler holds is shown not to wedge). The handler is
// released first, as shutdown waits for detachMu like every close does.
func shutdownWithin867(t *testing.T, l *Loop, release func()) {
	t.Helper()
	release()
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.shutdown()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Loop.shutdown did not return within 10s")
	}
}

func TestShutdownDeliversTheParkedReadEndError867(t *testing.T) {
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
			l, cs, _, release, notified := parkReadEnd867(t, tc.errs...)
			shutdownWithin867(t, l, release)
			got := notified()
			t.Logf("celeris867 case=%s OnError calls=%v closeErr_after=%v", tc.name, got, cs.closeErr)
			if len(got) != len(tc.want) {
				t.Fatalf("OnError was called %d times with %v, want %d times with %v", len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("OnError call %d got %v, want %v", i, got[i], tc.want[i])
				}
			}
			if cs.closeErr != nil {
				t.Errorf("closeErr = %v after shutdown, want it consumed", cs.closeErr)
			}
		})
	}
}

// TestCloseConnDeliversTheParkedReadEndError867 is the control the shutdown
// path must match: closeConn, run once the handler has returned, delivers the
// parked error, once, first error kept.
func TestCloseConnDeliversTheParkedReadEndError867(t *testing.T) {
	l, _, fd, release, notified := parkReadEnd867(t, errFirst867, errSecond867)
	release()
	l.closeConn(fd)
	if got := notified(); len(got) != 1 || got[0] != errFirst867 {
		t.Fatalf("closeConn delivered %v, want exactly [%v]", got, errFirst867)
	}
}

// TestSecondReadEndErrorAfterTheHandlerReturned867 is the non-busy twin of the
// parked-first-error rule: a second read-end error that arrives once the
// handler has released detachMu, but before its hand-back (closeOwed) has been
// drained, takes closeOnReadEnd's non-busy path. That path used to call
// OnError(second) itself and then closeConn delivered the parked first error,
// so the middleware was told [second, first]: the later death, then the
// original one (a last-wins handler such as the WebSocket writeErr kept the
// first). The middleware must be told the first error, once, and nothing else.
func TestSecondReadEndErrorAfterTheHandlerReturned867(t *testing.T) {
	l, cs, fd, release, notified := parkReadEnd867(t, errFirst867)
	release() // the handler returned; its hand-back has not been drained yet
	l.closeOnReadEnd(fd, cs, errSecond867)
	got := notified()
	t.Logf("celeris867 second-after-release OnError calls=%v closeErr_after=%v", got, cs.closeErr)
	if len(got) != 1 || got[0] != errFirst867 {
		t.Fatalf("OnError was called with %v, want exactly [%v]", got, errFirst867)
	}
	if cs.closeErr != nil {
		t.Errorf("closeErr = %v after the close, want it consumed", cs.closeErr)
	}
}

// A conn with nothing parked still tells the middleware the error it met.
func TestReadEndErrorOnAFreeConnIsDelivered867(t *testing.T) {
	l, cs, fd, release, notified := parkReadEnd867(t)
	release()
	l.closeOnReadEnd(fd, cs, errSecond867)
	if got := notified(); len(got) != 1 || got[0] != errSecond867 {
		t.Fatalf("OnError was called with %v, want exactly [%v]", got, errSecond867)
	}
}
