//go:build linux

package adaptive

import (
	"context"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
)

// addrHidden616 is a real start engine whose published address Listen cannot
// see: everything is the wrapped engine's but Addr, which stays nil. The
// "context cancelled" arms use it so that the early return they are about is
// taken by construction: adaptive.Listen waits for the start engine's address
// while its context is still alive, and with the context already cancelled at
// the gap hook it sees the end of the context first. With the bare engine it was
// a race between that wait and the engine's own Listen, which started with a
// cancelled context: a start engine that published before the wait looked at it
// (epoll or io_uring, on a starved runner: celeris#1328, 1 run in 500 with the
// test pinned to 2 CPUs beside 3 busy loops) took Listen down the return of an
// engine that did start, which TestAdaptivePortHeldUntilListenReturns616
// covers, and the arm failed on its own premise. The engine still starts and is
// still stopped by the cancel, with its sockets beside the hold, so the checks
// after Listen are as strict as before.
type addrHidden616 struct{ engine.Engine }

func (addrHidden616) Addr() net.Addr { return nil }

// hideStartAddr616 puts the start engine behind addrHidden616.
func hideStartAddr616(e *Engine) {
	var hidden engine.Engine = addrHidden616{Engine: *e.active.Load()}
	e.active.Store(&hidden)
}

// TestAdaptiveListenEarlyReturnReleasesHold616: a Listen that returns before
// the start engine has published its address gives the port hold back with no
// Shutdown to follow (celeris#886 item 4). celeris.Server never calls Shutdown
// after a failed Start, so the deferred release in Listen is the only thing
// that frees the port for the next socket; a hold kept would stay bound until
// the garbage collector finalized the descriptor.
//
// Three ways to return early are reached, each by an injection that sends Listen
// down its own exit:
//
//   - "context cancelled": the context ends at the last moment the hold covers
//     (listenGapHook) and the start engine's address is kept from Listen
//     (addrHidden616), so Listen takes its "stopped" return and reports no
//     error;
//   - "start engine fails": the start engine's Listen returns an error without
//     ever publishing an address, as an io_uring engine whose worker init
//     failed does, and Listen takes its "sub-engine startup failed" return;
//   - "bind deadline passes": the start engine runs but publishes no address
//     within listenBindDeadline (cut to 200 ms), and Listen takes its
//     "failed to initialize within" return.
//
// Each arm checks that the injection made Listen return early (no address
// published; the explicit return of a started engine is not the one under
// test), that the hold is gone, and that a bare socket can bind the port
// within 2 s, with the engine still referenced so that its finalizer cannot be
// the reason. TestAdaptivePortHeldUntilListenReturns616 covers the return of
// an engine that did start, and fails for a release placed only there no more
// than for none: this one fails for it.
func TestAdaptiveListenEarlyReturnReleasesHold616(t *testing.T) {
	type arm struct {
		name    string
		env     string
		want    engine.EngineType
		inject  func(e *Engine, cancel context.CancelFunc)
		wantErr string // "" = Listen reports no error
	}
	arms := []arm{
		{name: "context cancelled/epoll", env: "epoll", want: engine.Epoll,
			inject: func(e *Engine, cancel context.CancelFunc) {
				hideStartAddr616(e)
				listenGapHook = func(string) { cancel() }
			}},
		{name: "context cancelled/iouring", env: "iouring", want: engine.IOUring,
			inject: func(e *Engine, cancel context.CancelFunc) {
				hideStartAddr616(e)
				listenGapHook = func(string) { cancel() }
			}},
		{name: "start engine fails/epoll", env: "epoll", want: engine.Epoll,
			inject: func(e *Engine, _ context.CancelFunc) {
				var failing engine.Engine = &failingListenStandby{mockEngine: newMockEngine(engine.Epoll)}
				e.active.Store(&failing)
			},
			wantErr: "injected standby Listen failure"},
		{name: "bind deadline passes/epoll", env: "epoll", want: engine.Epoll,
			inject: func(e *Engine, _ context.CancelFunc) {
				listenBindDeadline.Store(int64(200 * time.Millisecond))
				var silent engine.Engine = &lateBindingStandby{mockEngine: newMockEngine(engine.Epoll), listenEnded: make(chan struct{})}
				e.active.Store(&silent)
			},
			wantErr: "failed to initialize within"},
	}
	savedDeadline := listenBindDeadline.Load()
	defer func() { listenBindDeadline.Store(savedDeadline) }()
	for _, a := range arms {
		t.Run(a.name, func(t *testing.T) {
			e, addr, _ := newGapEngine(t, a.env, a.want)
			if e.portHold.Load() == nil {
				t.Fatal("New returned without holding the port")
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			hooked := false
			a.inject(e, cancel)
			if listenGapHook != nil {
				inner := listenGapHook
				listenGapHook = func(s string) { hooked = true; inner(s) }
			} else {
				listenGapHook = func(string) { hooked = true }
			}
			defer func() { listenGapHook = nil; listenBindDeadline.Store(savedDeadline) }()

			lerr := e.Listen(ctx)
			listenGapHook = nil
			if !hooked {
				t.Fatal("injection not made: the Listen hook never ran")
			}
			// The injection must have sent Listen down an early return: an
			// engine that published an address is the return the other test
			// covers.
			if e.Addr() != nil {
				t.Fatalf("injection not made: Listen published %v", e.Addr())
			}
			if (a.wantErr == "") != (lerr == nil) || (lerr != nil && !strings.Contains(lerr.Error(), a.wantErr)) {
				t.Fatalf("Listen returned %v, want an error containing %q (\"\" = none)", lerr, a.wantErr)
			}
			held := e.portHold.Load() != nil
			var waited time.Duration
			var failed []string
			for _, th := range portThieves {
				took, err := stealWithin(th, addr, 2*time.Second)
				waited = max(waited, took)
				if err != nil {
					failed = append(failed, th.name+": "+err.Error())
				}
			}
			t.Logf("celeris616 early return: %s listen_err=%v hold_after=%v thieves_failed=%d waited=%v",
				a.name, lerr != nil, held, len(failed), waited)
			if held {
				t.Errorf("Listen returned early (%v) and still holds the port with no Shutdown", lerr)
			}
			for _, f := range failed {
				t.Errorf("Listen returned early (%v) and, with no Shutdown, %s on %s after 2s: the hold was kept", lerr, f, addr)
			}
			runtime.KeepAlive(e) // the finalizer must not free the port for the wrong reason
			_ = e.Shutdown(context.Background())
		})
	}
}
