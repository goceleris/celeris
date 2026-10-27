//go:build linux

package epoll

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/engine"
)

// Tests for h2ServerOwes, checkTimeouts' look at whether a conn that is due for
// ReadTimeout or IdleTimeout is an HTTP/2 conn whose open streams all have a whole
// request (celeris#889).

func noStreamH2State889() *conn.H2State {
	return conn.NewH2State(nil, conn.H2Config{}, func([]byte) {}, nil)
}

// TestH2ServerOwesAnswers889: the answer for the conns the sweep meets, with
// and without a detachMu, unlocked.
func TestH2ServerOwesAnswers889(t *testing.T) {
	for _, async := range []bool{false, true} {
		for _, tc := range []struct {
			name string
			set  func(cs *connState)
			open bool
		}{
			{"http1", func(cs *connState) { cs.protocol = engine.HTTP1; cs.h1State = conn.NewH1State() }, false},
			{"h2-no-stream", func(cs *connState) { cs.protocol = engine.H2C; cs.h2State = noStreamH2State889() }, false},
			{"h2-request-open", func(cs *connState) { cs.protocol = engine.H2C; cs.h2State = h2OpenStream889(t, false) }, false},
			{"h2-request-whole", func(cs *connState) { cs.protocol = engine.H2C; cs.h2State = h2OpenStream889(t, true) }, true},
		} {
			name := tc.name
			if async {
				name += "/async"
			}
			t.Run(name, func(t *testing.T) {
				cs := acquireConnState(context.Background(), 3, 64, async)
				defer releaseConnState(cs)
				tc.set(cs)
				open, ok := h2ServerOwes(cs)
				if !ok || open != tc.open {
					t.Fatalf("h2ServerOwes = (open %v, ok %v), want (%v, true)", open, ok, tc.open)
				}
			})
		}
	}
}

// TestH2ServerOwesWhileTheLockIsHeld889: the dispatch goroutine holds detachMu
// across a handler. Not switched to HTTP/2 yet: h2State is not read, and the
// answer is snapshotH1Deadlines' (a running handler: the timeouts apply, ok;
// any other holder: skip the sweep). Switched: h2State was written before the
// flag, which is read under asyncInMu, so it is read without the lock.
func TestH2ServerOwesWhileTheLockIsHeld889(t *testing.T) {
	newCS := func() *connState {
		cs := acquireConnState(context.Background(), 3, 64, true)
		cs.protocol = engine.H2C
		cs.h2State = h2OpenStream889(t, true)
		cs.detachMu.Lock()
		return cs
	}
	t.Run("handler-running-http1", func(t *testing.T) {
		cs := newCS()
		defer releaseConnState(cs)
		defer cs.detachMu.Unlock()
		cs.asyncInMu.Lock()
		cs.asyncRun, cs.asyncParked = true, false
		cs.asyncInMu.Unlock()
		if open, ok := h2ServerOwes(cs); open || !ok {
			t.Fatalf("(open %v, ok %v), want (false, true): a handler overrunning the timeouts on an HTTP/1 conn is closed as before", open, ok)
		}
	})
	t.Run("other-holder", func(t *testing.T) {
		cs := newCS()
		defer releaseConnState(cs)
		defer cs.detachMu.Unlock()
		if open, ok := h2ServerOwes(cs); open || ok {
			t.Fatalf("(open %v, ok %v), want (false, false): the sweep skips a conn whose lock a bounded holder has", open, ok)
		}
	})
	t.Run("switched-to-h2", func(t *testing.T) {
		cs := newCS()
		defer releaseConnState(cs)
		defer cs.detachMu.Unlock()
		cs.asyncInMu.Lock()
		cs.asyncRun, cs.asyncParked = true, false
		cs.h2Switched = true
		cs.asyncInMu.Unlock()
		if open, ok := h2ServerOwes(cs); !open || !ok {
			t.Fatalf("(open %v, ok %v), want (true, true): an upgraded h2c conn whose stream runs under the goroutine's lock is exempt", open, ok)
		}
	})
}

// TestH2ServerOwesReadsUnderTheLock889: the h1-to-h2c upgrade writes cs.h2State
// off the loop thread, under detachMu. Run with -race: a read outside the lock
// is reported.
func TestH2ServerOwesReadsUnderTheLock889(t *testing.T) {
	cs := acquireConnState(context.Background(), 3, 64, true)
	defer releaseConnState(cs)
	st := h2OpenStream889(t, true)
	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			cs.detachMu.Lock()
			cs.h2State = st
			cs.detachMu.Unlock()
			cs.detachMu.Lock()
			cs.h2State = nil
			cs.detachMu.Unlock()
		}
	}()
	for range 20000 {
		h2ServerOwes(cs)
	}
	stop.Store(true)
	wg.Wait()
}
