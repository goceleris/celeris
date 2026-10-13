//go:build linux

package epoll

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
)

// A panic out of the OnDetach hook on a conn that is not on the promoted async
// path (sync mode, and async mode before promotion) does not end the conn:
// Context.Detach does not mark the Context detached, the router answers 500 and
// the conn goes on as an ordinary keep-alive conn. Whatever the hook had already
// installed must therefore be undone, so the conn is exactly as if Detach had
// never run: the guarded writeFn (a write through it locks the one non-reentrant
// cs.detachMu, which the event-loop thread, the H2C branch and serveAsync hold
// across the writes they make), the adapter's, RawWriteFn, pause/resume, the
// count and asyncDetachPending (#889 item 34, review of PR #1014).

type rig889 struct {
	l     *Loop
	cs    *connState
	h1    *conn.H1State
	mu    sync.Mutex
	wrote []string
}

func newRig889(async bool) *rig889 {
	r := &rig889{l: &Loop{async: async, epollFD: -1}}
	r.cs = &connState{fd: -1, protocol: engine.HTTP1}
	if async {
		// acquireConnState allocates it in async mode.
		r.cs.detachMu = &sync.Mutex{}
		r.cs.asyncCond.L = &r.cs.asyncInMu
	}
	r.cs.writeFn = func(b []byte) {
		r.mu.Lock()
		r.wrote = append(r.wrote, string(b))
		r.mu.Unlock()
	}
	r.l.initProtocol(r.cs)
	r.h1 = r.cs.h1State
	return r
}

func (r *rig889) out() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.wrote, "")
}

// Two points the fault is injected at, both after cs.writeFn = guarded: early
// (the state the hook publishes through is gone, so UpdateWriteFn faults) and
// late (everything is installed).
const (
	faultEarly889 = "early"
	faultLate889  = "late"
)

func (r *rig889) panicking(t *testing.T, where string, hook func()) {
	t.Helper()
	switch where {
	case faultEarly889:
		r.cs.h1State = nil
		defer func() { r.cs.h1State = r.h1 }()
	case faultLate889:
		f := func() { panic("engine fault after the install") }
		detachPostInstallHook.Store(&f)
		defer detachPostInstallHook.Store(nil)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("the injected fault did not panic")
		}
	}()
	hook()
}

func within889(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("DEADLOCK: %s", what)
	}
}

// lockedWrite writes through fn the way the engine's callers do: holding
// cs.detachMu when the conn has one.
func (r *rig889) lockedWrite(fn func([]byte), s string) {
	if mu := r.cs.detachMu; mu != nil {
		mu.Lock()
		defer mu.Unlock()
	}
	fn([]byte(s))
}

func TestOnDetachPanicLeavesTheConnAsIfNeverRan889(t *testing.T) {
	for _, async := range []bool{false, true} {
		for _, where := range []string{faultEarly889, faultLate889} {
			name := where + "/sync"
			if async {
				name = where + "/async-unpromoted"
			}
			t.Run(name, func(t *testing.T) {
				r := newRig889(async)
				r.panicking(t, where, r.h1.OnDetach)

				if r.l.detachedCount != 0 || r.cs.detachCounted {
					t.Errorf("detachedCount=%d counted=%v, want 0 false", r.l.detachedCount, r.cs.detachCounted)
				}
				if r.cs.asyncDetachPending {
					t.Error("asyncDetachPending is still set: the drain would count a conn that never detached")
				}
				if !async && r.cs.detachMu != nil {
					t.Error("cs.detachMu is still set on a conn that never detached")
				}
				if r.h1.RawWriteFn != nil || r.h1.PauseRecv != nil || r.h1.ResumeRecv != nil {
					t.Error("RawWriteFn / PauseRecv / ResumeRecv are still installed")
				}
				if r.h1.Detached.Load() {
					t.Error("Detached is set")
				}
				// cs.writeFn is the conn's own: a write through it with detachMu
				// held (as the H2C branch and serveAsync hold it) returns.
				within889(t, "cs.writeFn is still the guard", func() { r.lockedWrite(r.cs.writeFn, "x") })
				if got := r.out(); got != "x" {
					t.Errorf("the conn's write function saw %q, want x", got)
				}
			})
		}
	}
}

// handler889 detaches on /d (the hook faults, the handler recovers as the router
// does) and answers on every path.
type handler889 struct {
	r      *rig889
	where  string
	panics int
}

func (h *handler889) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.Path == "/d" {
		func() {
			defer func() {
				if recover() != nil {
					h.panics++
				}
			}()
			if h.where == faultLate889 {
				f := func() { panic("engine fault after the install") }
				detachPostInstallHook.Store(&f)
				defer detachPostInstallHook.Store(nil)
			} else {
				h.r.cs.h1State = nil
				defer func() { h.r.cs.h1State = h.r.h1 }()
			}
			s.OnDetach()
		}()
	}
	return s.ResponseWriter.WriteResponse(s, 200, [][2]string{{"content-length", "2"}}, []byte("ok"))
}

// The promoted async path after an unpromoted panic: serveAsync holds detachMu
// around ProcessH1 and the response goes out through the adapter, which the
// hook had pointed at the guard (the router recovers the panic and answers 500
// through it; here the handler answers).
func TestOnDetachPanicRecoveredRequestAnswersUnderDetachMu889(t *testing.T) {
	for _, where := range []string{faultEarly889, faultLate889} {
		t.Run(where, func(t *testing.T) {
			r := newRig889(true)
			h := &handler889{r: r, where: where}
			within889(t, "the adapter still writes through the guard, under detachMu", func() {
				for _, path := range []string{"/d", "/ok"} {
					r.cs.detachMu.Lock() // serveAsync
					err := conn.ProcessH1(context.Background(), []byte("GET "+path+" HTTP/1.1\r\nHost: x\r\n\r\n"), r.h1, h, r.cs.writeFn)
					r.cs.detachMu.Unlock()
					if err != nil {
						t.Errorf("ProcessH1 %s: %v", path, err)
					}
				}
			})
			if h.panics != 1 {
				t.Fatalf("the hook faulted %d times, want 1", h.panics)
			}
			if got := strings.Count(r.out(), "HTTP/1.1 200"); got != 2 {
				t.Errorf("%d answers, want 2: %q", got, r.out())
			}
		})
	}
}

// (b) The promoted request after the panic, the writes only.
func TestOnDetachPanicThenPromotedWrite889(t *testing.T) {
	r := newRig889(true)
	r.panicking(t, faultEarly889, r.h1.OnDetach)
	r.cs.asyncPromoted = true
	within889(t, "promoted serveAsync write under detachMu through the leftover guard", func() {
		r.lockedWrite(r.cs.writeFn, "HTTP/1.1 200 OK\r\n\r\n")
	})
}

// (c) The H2C branch: the H2 state is built over cs.writeFn, and the branch
// locks detachMu around ProcessH2 on the event-loop thread.
func TestOnDetachPanicThenH2CWrite889(t *testing.T) {
	r := newRig889(false)
	r.panicking(t, faultEarly889, r.h1.OnDetach)
	writeFn := r.cs.writeFn // drainRead: writeFn := cs.writeFn, then switchToH2(cs, writeFn)
	within889(t, "H2C branch (detachMu held) writing through the leftover guard on the loop thread", func() {
		r.lockedWrite(writeFn, "SETTINGS-ACK")
	})
}

// (a) A drain between the panicked run and the re-run, then the re-run: the
// conn is counted once.
func TestOnDetachPanicThenAsyncDrainCountsOnce889(t *testing.T) {
	r := newRig889(true)
	r.panicking(t, faultLate889, r.h1.OnDetach)
	r.l.enqueueDetach(r.cs) // any later enqueue of cs (a partial guarded write, a relink)
	r.l.drainDetachQueue()
	r.h1.OnDetach() // the second Detach, an inline request (not promoted)
	r.l.drainDetachQueue()
	if r.l.detachedCount != 1 {
		t.Errorf("detachedCount=%d after two runs on one conn, want 1 (closeConn decrements once)", r.l.detachedCount)
	}
}

// The drain counts a conn once whatever enqueued it: a conn already counted,
// with the flag set again, is not counted again.
func TestDrainDetachQueueDoesNotCountACountedConn889(t *testing.T) {
	l := &Loop{async: true, epollFD: -1}
	cs := &connState{fd: -1, protocol: engine.HTTP1, detachCounted: true, asyncDetachPending: true}
	l.detachedCount = 1
	l.enqueueDetach(cs)
	l.drainDetachQueue()
	if l.detachedCount != 1 {
		t.Errorf("detachedCount=%d, want 1", l.detachedCount)
	}
}
