package celeris

import (
	"context"
	"io"
	"log/slog"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
)

// TestShutdownBeforeTheWatcherLoadsRunsHooksOnce pins the window celeris#728
// named in #692's review: a direct Server.Shutdown that lands after doPrepare
// has published the engine but before StartWithContext's watcher has taken
// its view of Shutdown, followed by a cancel of the Start context. At #692's
// head 5a05c2e the watcher snapshotted a Shutdown call counter at that point
// (callsBefore), so such a Shutdown counted as "before", the cancel found the
// counter unchanged, and the watcher ran Shutdown, and every OnShutdown hook,
// a second time. The design that merged decides from a flag a direct
// Shutdown sets before it runs anything (directShutdown), under lifecycleMu,
// so there is no snapshot and no window.
//
// The order is forced: the direct Shutdown has latched the shut-down state
// before the Start path begins; the engine's Listen, like a native engine's,
// keeps running after its context is cancelled until the test releases it, so
// the cancel reaches a watcher that is still waiting; and the hook count is
// read after the Start path, which waits for its watcher, has returned.
func TestShutdownBeforeTheWatcherLoadsRunsHooksOnce(t *testing.T) {
	s := New(Config{Engine: Std, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	var hookRuns atomic.Int32
	s.OnShutdown(func(context.Context) { hookRuns.Add(1) })

	eng := &window728Engine{listening: make(chan struct{}), release: make(chan struct{})}
	s.publishEngine(eng) // what doPrepare does: the engine is published

	// The direct Shutdown, in the window: after the engine is published,
	// before the Start path has begun. It runs on its own goroutine because
	// since celeris#703 it waits for Listen to return before its hooks.
	direct := make(chan error, 1)
	go func() { direct <- s.Shutdown(context.Background()) }()
	for {
		s.lifecycleMu.Lock()
		latched := s.shutdownCalled
		s.lifecycleMu.Unlock()
		if latched {
			break
		}
		time.Sleep(time.Millisecond)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.listenUntilCancelled(ctx, eng) }()
	<-eng.listening
	// Listen is held in its teardown, so the watcher is still waiting when
	// the caller cancels.
	cancel()
	// Let the watcher act on the cancel before Listen returns. The count
	// below does not depend on this being long enough: it is read after the
	// Start path returned, and that waits for the watcher.
	time.Sleep(50 * time.Millisecond)
	close(eng.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("listenUntilCancelled: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("listenUntilCancelled did not return after Listen did")
	}
	select {
	case err := <-direct:
		if err != nil {
			t.Fatalf("the direct Shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the direct Shutdown did not return after Listen did")
	}
	if n := hookRuns.Load(); n != 1 {
		t.Errorf("a direct Shutdown before the watcher's view, then a cancel: the OnShutdown hook ran %d times, want 1 (celeris#728)", n)
	}
}

// window728Engine's Listen, once its context is cancelled, waits for release
// before it returns, the way a native engine's Listen runs its teardown.
type window728Engine struct {
	listening chan struct{}
	release   chan struct{}
}

func (e *window728Engine) Listen(ctx context.Context) error {
	close(e.listening)
	<-ctx.Done()
	<-e.release
	return nil
}
func (e *window728Engine) Shutdown(context.Context) error { return nil }
func (e *window728Engine) Metrics() engine.EngineMetrics  { return engine.EngineMetrics{} }
func (e *window728Engine) Type() engine.EngineType        { return engine.Epoll }
func (e *window728Engine) Addr() net.Addr                 { return nil }
