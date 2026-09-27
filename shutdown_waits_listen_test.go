package celeris

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

// TestShutdownRunsHooksOnlyAfterListenReturns is the engine-independent half
// of celeris#703 (shutdown_drain_order_linux_test.go drives the real engines).
// On epoll and io_uring Engine.Shutdown is a no-op and the drain runs in
// Listen once its context is cancelled, so the drain is over only when Listen
// returns. Shutdown used to run the OnShutdown hooks, and return, right after
// cancelling that context. teardownEngine's Listen keeps running after the
// cancel until the test releases it, the way a native engine's does while its
// workers close their connections.
//
// The order is forced: the test releases Listen as soon as a hook starts, or
// after releaseAfter if none does. A Shutdown that does not wait reaches its
// hooks with Listen still held, every time; one that waits cannot reach them
// before the release.
func TestShutdownRunsHooksOnlyAfterListenReturns(t *testing.T) {
	const releaseAfter = 200 * time.Millisecond
	cases := []struct {
		name string
		// run starts the server's Listen the way the entry point does and
		// returns once it has.
		run func(s *Server, ctx context.Context, eng *teardownEngine) error
		// cancelStarts: the shutdown is started by cancelling ctx, and the
		// call that shuts the server down is run itself; otherwise by a
		// direct Server.Shutdown.
		cancelStarts bool
	}{
		{"Start+Shutdown", func(s *Server, _ context.Context, eng *teardownEngine) error {
			return s.listen(context.Background(), eng)
		}, false},
		{"StartWithContext+Shutdown", func(s *Server, ctx context.Context, eng *teardownEngine) error {
			return s.listenUntilCancelled(ctx, eng)
		}, false},
		{"StartWithContext+cancel", func(s *Server, ctx context.Context, eng *teardownEngine) error {
			return s.listenUntilCancelled(ctx, eng)
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(Config{Engine: Std, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
			eng := newTeardownEngine()
			var hookSawListenReturned atomic.Bool
			hookStarted := make(chan struct{})
			s.OnShutdown(func(context.Context) {
				hookSawListenReturned.Store(eng.returned.Load())
				close(hookStarted)
			})
			s.publishEngine(eng)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			runDone := make(chan error, 1)
			go func() { runDone <- tc.run(s, ctx, eng) }()
			<-eng.listening

			// The call that shuts the server down: Shutdown itself, or the
			// Start*Context call whose context is cancelled.
			callDone := runDone
			if tc.cancelStarts {
				cancel()
			} else {
				direct := make(chan error, 1)
				go func() { direct <- s.Shutdown(context.Background()) }()
				callDone = direct
			}
			<-eng.cancelled

			var callErr error
			returned := false
			select {
			case <-hookStarted:
			case callErr = <-callDone:
				returned = true
			case <-time.After(releaseAfter):
			}
			close(eng.release)
			if !returned {
				select {
				case callErr = <-callDone:
				case <-time.After(10 * time.Second):
					t.Fatal("the call that shut the server down did not return after Listen did")
				}
			}
			if callErr != nil {
				t.Errorf("the call that shut the server down returned %v, want nil", callErr)
			}
			select {
			case <-hookStarted:
			default:
				t.Fatal("the OnShutdown hook never ran")
			}
			if !hookSawListenReturned.Load() {
				t.Errorf("%s: the OnShutdown hook ran while Listen was still draining (celeris#703)", tc.name)
			}
			if returned {
				t.Errorf("%s: the call that shut the server down returned while Listen was still draining (celeris#703)", tc.name)
			}
			if !tc.cancelStarts {
				select {
				case err := <-runDone:
					if err != nil {
						t.Errorf("Listen's entry point returned %v, want nil", err)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("Listen's entry point did not return after Listen did")
				}
			}
		})
	}
}

// TestShutdownWaitForListenIsBoundedByCtx: the wait for the drain is bounded
// by Shutdown's ctx. When ctx is done first the hooks still run, with that
// ctx, and Shutdown returns ctx's error, as std's http.Server.Shutdown does
// when its drain outlives the deadline.
func TestShutdownWaitForListenIsBoundedByCtx(t *testing.T) {
	s := New(Config{Engine: Std, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	eng := newTeardownEngine()
	var hookCtxErr atomic.Value
	var hookRuns atomic.Int32
	s.OnShutdown(func(ctx context.Context) {
		hookRuns.Add(1)
		if err := ctx.Err(); err != nil {
			hookCtxErr.Store(err)
		}
	})
	s.publishEngine(eng)

	runDone := make(chan error, 1)
	go func() { runDone <- s.listen(context.Background(), eng) }()
	<-eng.listening

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := s.Shutdown(ctx)
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Shutdown with Listen still draining past ctx's deadline returned %v, want %v", err, context.DeadlineExceeded)
	}
	if elapsed < 100*time.Millisecond {
		t.Errorf("Shutdown returned after %v, before its ctx's 100ms deadline: it did not wait for the drain", elapsed)
	}
	if n := hookRuns.Load(); n != 1 {
		t.Errorf("the hook ran %d times, want 1: the hooks run even when the drain outlives ctx", n)
	}
	if e, _ := hookCtxErr.Load().(error); !errors.Is(e, context.DeadlineExceeded) {
		t.Errorf("the hook saw ctx error %v, want %v: it gets Shutdown's ctx", e, context.DeadlineExceeded)
	}
	if eng.returned.Load() {
		t.Fatal("precondition: Listen returned before the release")
	}
	close(eng.release)
	select {
	case err := <-runDone:
		if err != nil {
			t.Errorf("listen: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("listen did not return after Listen did")
	}
}
