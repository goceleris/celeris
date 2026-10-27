//go:build linux

package iouring

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/engine"
)

// Tests for h2StreamOpen, checkTimeouts' look at whether a conn that is due for
// ReadTimeout or IdleTimeout is an HTTP/2 conn with a stream open (celeris#889).

// TestH2StreamOpenAnswers889: the answer for the conns the sweep meets, with
// and without a detachMu.
func TestH2StreamOpenAnswers889(t *testing.T) {
	for _, withMu := range []bool{false, true} {
		for _, tc := range []struct {
			name string
			set  func(cs *connState)
			open bool
		}{
			{"http1", func(cs *connState) { cs.protocol.Store(int32(engine.HTTP1)); cs.h1State = conn.NewH1State() }, false},
			{"h2-no-stream", func(cs *connState) {
				cs.protocol.Store(int32(engine.H2C))
				cs.h2State = conn.NewH2State(nil, conn.H2Config{}, func([]byte) {}, nil)
			}, false},
			{"h2-stream-open", func(cs *connState) { cs.protocol.Store(int32(engine.H2C)); cs.h2State = h2OpenStream889(t) }, true},
		} {
			name := tc.name
			if withMu {
				name += "/detachMu"
			}
			t.Run(name, func(t *testing.T) {
				cs := &connState{fd: 3, detected: true}
				if withMu {
					cs.detachMu = &sync.Mutex{}
				}
				tc.set(cs)
				open, known := h2StreamOpen(cs)
				if !known || open != tc.open {
					t.Fatalf("h2StreamOpen = (open %v, known %v), want (%v, true)", open, known, tc.open)
				}
			})
		}
	}
}

// TestH2StreamOpenSkipsAHeldLock889: the dispatch goroutine holds detachMu
// across a handler; nothing is read and the sweep leaves the conn to the next.
func TestH2StreamOpenSkipsAHeldLock889(t *testing.T) {
	cs := &connState{fd: 3, detected: true, detachMu: &sync.Mutex{}}
	cs.protocol.Store(int32(engine.H2C))
	cs.h2State = h2OpenStream889(t)
	cs.detachMu.Lock()
	defer cs.detachMu.Unlock()
	if open, known := h2StreamOpen(cs); open || known {
		t.Fatalf("(open %v, known %v), want (false, false)", open, known)
	}
}

// TestH2StreamOpenReadsUnderTheLock889: the h1-to-h2c upgrade writes cs.h2State
// off the worker, under detachMu. Run with -race: a read outside the lock is
// reported.
func TestH2StreamOpenReadsUnderTheLock889(t *testing.T) {
	cs := &connState{fd: 3, detected: true, detachMu: &sync.Mutex{}}
	st := h2OpenStream889(t)
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
		h2StreamOpen(cs)
	}
	stop.Store(true)
	wg.Wait()
}
