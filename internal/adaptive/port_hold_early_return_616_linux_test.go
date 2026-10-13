//go:build linux

package adaptive

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
)

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
//     (listenGapHook), so Listen takes its "stopped" return and reports no
//     error;
//   - "start engine fails": the start engine's Listen returns an error without
//     ever publishing an address, as an io_uring engine whose worker init
//     failed does, and Listen takes its "sub-engine startup failed" return.
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
			inject: func(_ *Engine, cancel context.CancelFunc) {
				listenGapHook = func(string) { cancel() }
			}},
		{name: "context cancelled/iouring", env: "iouring", want: engine.IOUring,
			inject: func(_ *Engine, cancel context.CancelFunc) {
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
				listenBindDeadline = 200 * time.Millisecond
				var silent engine.Engine = &lateBindingStandby{mockEngine: newMockEngine(engine.Epoll), listenEnded: make(chan struct{})}
				e.active.Store(&silent)
			},
			wantErr: "failed to initialize within"},
	}
	savedDeadline := listenBindDeadline
	defer func() { listenBindDeadline = savedDeadline }()
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
			defer func() { listenGapHook = nil; listenBindDeadline = savedDeadline }()

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
