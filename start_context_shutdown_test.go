package celeris

import (
	"context"
	"io"
	"log/slog"
	"net"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
)

// TestStartWithContextShutsDownWhenListenReturnsFirst pins celeris#673.
//
// StartWithContext and StartWithListenerAndContext start a watcher goroutine
// that is meant to call Server.Shutdown when the caller's context is
// cancelled. It used to wait in one select on ctx.Done() and on listenDone
// (closed once Listen returns) and to return WITHOUT shutting down on
// listenDone. But the context handed to Listen is derived from ctx, so a
// cancel also makes Listen return and close listenDone. When the watcher has
// not yet reached its select by then, both cases are ready, select picks one
// at random, and half the time Shutdown is skipped: no OnShutdown hooks, the
// CPU monitor's file descriptor left open, and the settle re-opener
// (celeris#592) left running for the life of the process. That last one is
// the goroutine TestRouteAdaptive_NoReopenerWhenEngineCreationFails found
// alive on a CI runner.
//
// The order is FORCED rather than waited for. The context is cancelled
// before Start is called, so Listen returns at once, and GOMAXPROCS is 1, so
// the watcher (a new goroutine on the only P) cannot run until the Start
// goroutine blocks. On the unfixed code Start never blocks after spawning it:
// listenDone is closed and Start returns before the watcher has looked at
// either channel, so every iteration presents the watcher with both cases
// ready. The test then asks, per iteration:
//
//   - did Shutdown finish before Start returned (the fix's contract: Start
//     returns once the Shutdown the cancel asked for is done)?
//   - if not, did it run at all within a generous wait (the skip itself)?
//
// With 16 iterations the unfixed code skips Shutdown in about half of them;
// the chance that it skips none is 2^-16.
func TestStartWithContextShutsDownWhenListenReturnsFirst(t *testing.T) {
	const iterations = 16
	// How long an iteration waits, after Start returned without having shut
	// down, for a late Shutdown before calling it skipped. With GOMAXPROCS=1
	// the watcher runs as soon as this goroutine blocks, so a Shutdown that is
	// going to happen at all happens within microseconds; a second is slack
	// for a loaded runner under -race, not a timing assumption.
	const lateWait = time.Second

	entries := []struct {
		name  string
		start func(t *testing.T, s *Server, ctx context.Context) error
	}{
		{"StartWithContext", func(_ *testing.T, s *Server, ctx context.Context) error {
			return s.StartWithContext(ctx)
		}},
		{"StartWithListenerAndContext", func(t *testing.T, s *Server, ctx context.Context) error {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			return s.StartWithListenerAndContext(ctx, ln)
		}},
	}
	for _, entry := range entries {
		t.Run(entry.name, func(t *testing.T) {
			prev := runtime.GOMAXPROCS(1)
			defer runtime.GOMAXPROCS(prev)

			var beforeReturn, late, skipped int
			for range iterations {
				s := New(Config{
					Engine:          Std,
					Addr:            "127.0.0.1:0",
					AsyncHandlers:   true,
					ShutdownTimeout: 5 * time.Second,
					// No log write between spawning the watcher and closing
					// listenDone: a slow stderr write is a syscall the
					// scheduler can hand the P off during, which would let
					// the watcher run early and blur the forced order.
					Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
				})
				// An adaptive route, so the settle re-opener starts and the
				// test can also check the goroutine #673 was filed about.
				s.GET("/s", noopHandler)
				if !s.router.adaptiveRoutes["/s"] {
					t.Fatal("precondition: /s must be adaptive, or the re-opener never starts")
				}
				hookRan := make(chan struct{})
				s.OnShutdown(func(context.Context) { close(hookRan) })

				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				if err := entry.start(t, s, ctx); err != nil {
					t.Fatalf("%s returned %v, want nil after a cancel", entry.name, err)
				}

				select {
				case <-hookRan:
					beforeReturn++
					if reopenerRunning(s.router) {
						t.Errorf("Shutdown ran but the settle re-opener is still running")
					}
					continue
				default:
				}
				select {
				case <-hookRan:
					late++
				case <-time.After(lateWait):
					skipped++
					if !reopenerRunning(s.router) {
						t.Errorf("OnShutdown hook never ran but the re-opener was stopped: something other than Shutdown stopped it, so the hook is the wrong witness")
					}
					// Release what the skipped Shutdown left behind, so the
					// iterations do not accumulate goroutines.
					_ = s.Shutdown(context.Background())
				}
			}
			t.Logf("%s: %d/%d iterations shut down before Start returned, %d shut down after it returned, %d never shut down",
				entry.name, beforeReturn, iterations, late, skipped)
			if skipped > 0 {
				t.Errorf("%s: the context was cancelled but Shutdown never ran in %d of %d iterations (celeris#673: the watcher's select took listenDone)",
					entry.name, skipped, iterations)
			}
			if late > 0 {
				t.Errorf("%s: Shutdown ran only after Start had returned in %d of %d iterations: a caller that exits when Start returns loses its OnShutdown hooks",
					entry.name, late, iterations)
			}
		})
	}
}

// reopenerRunning reports whether the settle re-opener is started, reading
// reopenStop under the lock that guards it.
func reopenerRunning(rt *router) bool {
	rt.reopenMu.Lock()
	defer rt.reopenMu.Unlock()
	return rt.reopenStop != nil
}

// TestStartContextWatcherDoesNotRepeatADirectShutdown is the control for the
// guard the celeris#673 fix adds. The watcher now shuts down whenever ctx is
// done, whichever channel woke it, so it must not do so when the caller has
// already called Server.Shutdown during the run: that would run every
// OnShutdown hook a second time.
//
// The order is forced with an engine whose Listen, like the native engines',
// keeps running for a while after its context is cancelled (their worker
// teardown). The caller shuts down directly, and cancels ctx while Listen is
// still tearing down, so the watcher wakes on ctx.Done() with listenDone still
// open: the exact case where only the guard stands between it and a second
// Shutdown. Without the guard the hook runs twice in every run.
//
// Since celeris#703 the direct Shutdown waits for Listen to return before it
// runs the hooks, so it is made on its own goroutine and is still in progress,
// its hooks not yet run, when ctx is cancelled.
func TestStartContextWatcherDoesNotRepeatADirectShutdown(t *testing.T) {
	s := New(Config{Engine: Std, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	var hookRuns atomic.Int32
	s.OnShutdown(func(context.Context) { hookRuns.Add(1) })

	eng := newTeardownEngine()
	s.publishEngine(eng)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.listenUntilCancelled(ctx, eng) }()
	<-eng.listening

	direct := make(chan error, 1)
	go func() { direct <- s.Shutdown(context.Background()) }()
	// The direct Shutdown has claimed the shutdown once it has cancelled
	// Listen's context: it marks itself before it runs anything.
	<-eng.cancelled
	if n := hookRuns.Load(); n != 0 {
		t.Fatalf("the hook ran %d times while Listen was still tearing down, want 0 (celeris#703)", n)
	}
	// Listen is now tearing down and has not returned, so listenDone is
	// still open when ctx is cancelled.
	cancel()
	// Let the watcher act on the cancel before Listen returns. Its decision
	// needs no I/O, and the assertion below does not depend on this sleep
	// being long enough: without the guard a late watcher still sees ctx done
	// and repeats the Shutdown (the hook count is read after Start returns,
	// and Start waits for the watcher).
	time.Sleep(50 * time.Millisecond)
	close(eng.release)
	select {
	case err := <-direct:
		if err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the direct Shutdown did not return after Listen did")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("listenUntilCancelled: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("listenUntilCancelled did not return after Listen did")
	}
	if n := hookRuns.Load(); n != 1 {
		t.Errorf("OnShutdown hook ran %d times, want 1: the watcher repeated a Shutdown the caller had already made", n)
	}
}

// teardownEngine is an engine.Engine whose Listen, once its context is
// cancelled, waits for release before returning, the way a native engine's
// Listen keeps running through its worker teardown. listening is closed when
// Listen starts, cancelled when it has seen its context cancelled, and
// returned is set just before it returns.
type teardownEngine struct {
	listening chan struct{}
	cancelled chan struct{}
	release   chan struct{}
	returned  atomic.Bool
}

func newTeardownEngine() *teardownEngine {
	return &teardownEngine{
		listening: make(chan struct{}),
		cancelled: make(chan struct{}),
		release:   make(chan struct{}),
	}
}

func (e *teardownEngine) Listen(ctx context.Context) error {
	close(e.listening)
	<-ctx.Done()
	close(e.cancelled)
	<-e.release
	e.returned.Store(true)
	return nil
}
func (e *teardownEngine) Shutdown(context.Context) error { return nil }
func (e *teardownEngine) Metrics() engine.EngineMetrics  { return engine.EngineMetrics{} }
func (e *teardownEngine) Type() engine.EngineType        { return engine.Epoll }
func (e *teardownEngine) Addr() net.Addr                 { return nil }

// TestADirectShutdownAfterTheWatchersDoesNotRepeatIt is the other order: the
// cancel comes first, and the direct Shutdown after it (review of #692). A
// caller whose signal handler cancels the Start context and then calls
// Server.Shutdown, or whose main calls Shutdown once Start has returned, did
// exactly this. The watcher's guard read a counter of Shutdown calls, and a
// direct call checked nothing, so the direct call ran every OnShutdown hook a
// second time: during the watcher's Shutdown, and after it had returned.
//
// The direct call now waits for the Shutdown the watcher claimed and returns
// its result. The first hook run holds the watcher's Shutdown open while the
// direct call is made. The result does not depend on when the direct call
// gets to run: the watcher claims the shutdown before it runs the hooks, and
// the call runs any hook it is going to run before it returns.
func TestADirectShutdownAfterTheWatchersDoesNotRepeatIt(t *testing.T) {
	s := New(Config{Engine: Std, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	var hookRuns atomic.Int32
	entered := make(chan struct{})
	releaseHook := make(chan struct{})
	s.OnShutdown(func(context.Context) {
		if hookRuns.Add(1) == 1 {
			close(entered)
			<-releaseHook
		}
	})

	eng := newTeardownEngine()
	s.publishEngine(eng)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.listenUntilCancelled(ctx, eng) }()
	<-eng.listening

	cancel()
	// Since celeris#703 the watcher's Shutdown runs the hooks only once
	// Listen has returned, so let Listen finish its teardown.
	<-eng.cancelled
	close(eng.release)
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		close(releaseHook)
		t.Fatal("the cancel never reached the OnShutdown hook: the watcher did not shut down")
	}
	direct := make(chan error, 1)
	go func() { direct <- s.Shutdown(context.Background()) }()
	close(releaseHook)
	select {
	case err := <-direct:
		if err != nil {
			t.Errorf("the direct Shutdown during the watcher's: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the direct Shutdown did not return after the watcher's Shutdown did")
	}
	if n := hookRuns.Load(); n != 1 {
		t.Errorf("a direct Shutdown during the one the cancel started: the hook ran %d times, want 1", n)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("listenUntilCancelled: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("listenUntilCancelled did not return after Listen did")
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Errorf("a Shutdown after Start returned: %v", err)
	}
	if n := hookRuns.Load(); n != 1 {
		t.Errorf("a Shutdown after Start returned: the hook ran %d times in all, want 1", n)
	}
}
