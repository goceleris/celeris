package sse

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

// fakeEngineHooks plays the native engine's part for the celeris#494
// regression tests: it records the detached-conn callbacks the SSE
// middleware installs through the exact Stream hooks that
// populateCachedStream wires on epoll/io_uring (OnWSSetError and
// OnWSDetachClose), and whether they were installed before or after
// Context.Detach ran (OnDetach). Every field is guarded by mu because the
// test fires the recorded callbacks from its own goroutine while the
// middleware's stream goroutine is still running.
type fakeEngineHooks struct {
	mu          sync.Mutex
	detached    atomic.Bool
	errFn       func(error)
	closeFn     func()
	afterDetach bool // a hook was installed AFTER OnDetach fired
}

func (h *fakeEngineHooks) snapshot() (errFn func(error), closeFn func(), afterDetach bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.errFn, h.closeFn, h.afterDetach
}

// newDetachedSSEContext builds a Context that looks like a native-engine
// H1 stream to the SSE middleware: OnDetach is set (so
// EngineSupportsAsyncDetach() is true and sse.New drives the stream on
// its own goroutine, exactly as on epoll/io_uring) and the two engine
// disconnect setters record what the middleware installs.
//
// It deliberately uses celeristest.NewContext rather than NewContextT and
// never calls ReleaseContext: with OnDetach set, the stream goroutine's
// trailing done() writes c.detachSnap AFTER the OnDisconnect signal the
// tests wait on, with no happens-before edge to a t.Cleanup-registered
// reset(), so a cleanup-registered release is a -race report waiting to
// happen. One pooled Context per subtest is left to the GC instead.
func newDetachedSSEContext(t *testing.T) (*celeris.Context, *mockStreamer, *fakeEngineHooks) {
	t.Helper()
	ctx, _ := celeristest.NewContext("GET", "/events")
	ms := &mockStreamer{}
	hooks := &fakeEngineHooks{}
	s := celeris.TestStream(ctx)
	s.ResponseWriter = ms
	s.OnDetach = func() { hooks.detached.Store(true) }
	s.OnWSSetError = func(fn func(error)) {
		hooks.mu.Lock()
		defer hooks.mu.Unlock()
		hooks.errFn = fn
		if hooks.detached.Load() {
			hooks.afterDetach = true
		}
	}
	s.OnWSDetachClose = func(fn func()) {
		hooks.mu.Lock()
		defer hooks.mu.Unlock()
		hooks.closeFn = fn
		if hooks.detached.Load() {
			hooks.afterDetach = true
		}
	}
	return ctx, ms, hooks
}

// TestEngineDisconnectCancelsClient is the celeris#494 regression pin.
//
// On epoll/io_uring the H1 StreamWriter never returns an error and
// c.Context() is context.Background(), so the only way the SSE middleware
// can learn the peer went away is the engine's H1State.OnError /
// H1State.OnDetachClose callbacks. The middleware must install both
// (after OnConnect, before Detach) and route either into the client's
// context cancel so a handler parked on client.Context().Done() — or on
// Send — returns and the stream tears down.
func TestEngineDisconnectCancelsClient(t *testing.T) {
	cases := []struct {
		name string
		fire func(errFn func(error), closeFn func())
	}{
		{"OnError", func(errFn func(error), _ func()) { errFn(io.EOF) }},
		{"OnDetachClose", func(_ func(error), closeFn func()) { closeFn() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, ms, hooks := newDetachedSSEContext(t)

			handlerCtx := make(chan context.Context, 1)
			sendErr := make(chan error, 1)
			disconnected := make(chan struct{})
			h := New(Config{
				// A live heartbeat goroutine reproduces the soak's
				// three-goroutines-per-stream signature (handler +
				// heartbeat + recoverAndRelease waiter).
				HeartbeatInterval: 5 * time.Millisecond,
				OnDisconnect:      func(*celeris.Context, *Client) { close(disconnected) },
				Handler: func(client *Client) {
					cctx := client.Context()
					handlerCtx <- cctx
					<-cctx.Done()
					sendErr <- client.Send(Event{Data: "late"})
				},
			})

			if err := h(ctx); err != nil {
				t.Fatalf("handler returned error: %v", err)
			}

			errFn, closeFn, afterDetach := hooks.snapshot()
			if errFn == nil || closeFn == nil {
				t.Fatalf("sse.New did not install the engine disconnect hooks (OnError=%v OnDetachClose=%v) — celeris#494",
					errFn != nil, closeFn != nil)
			}
			if !hooks.detached.Load() {
				t.Fatal("Context.Detach was not called")
			}
			if afterDetach {
				t.Fatal("hooks installed AFTER Detach — a peer RST in the Detach window would be lost")
			}

			var cctx context.Context
			select {
			case cctx = <-handlerCtx:
			case <-time.After(2 * time.Second):
				t.Fatal("SSE handler did not start within 2s")
			}
			if cctx.Err() != nil {
				t.Fatalf("client context done before any disconnect signal: %v", cctx.Err())
			}

			// The engine reports the peer went away.
			tc.fire(errFn, closeFn)

			select {
			case err := <-sendErr:
				if err == nil {
					t.Fatal("Send after engine disconnect returned nil")
				}
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("Send after engine disconnect: got %v, want context.Canceled", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("SSE handler still parked on client.Context().Done() 2s after %s fired — celeris#494", tc.name)
			}
			select {
			case <-disconnected:
			case <-time.After(2 * time.Second):
				t.Fatal("OnDisconnect did not run within 2s: the stream goroutine did not tear down")
			}
			ms.mu.Lock()
			closed := ms.closed
			ms.mu.Unlock()
			if !closed {
				t.Fatal("StreamWriter was not closed by the teardown defer chain")
			}

			// Stale-hook safety: the engine never clears OnError and can
			// fire OnDetachClose long after the stream finished (idle
			// sweep, shutdown). Both must be harmless no-ops on the
			// finished ctx — no panic, no touch of the pooled Client.
			errFn(io.ErrUnexpectedEOF)
			closeFn()
			errFn(nil)
			closeFn()
		})
	}
}

// TestEngineDisconnectHooksNotInstalledWhenRejected pins the install
// ordering from the other side: an OnConnect rejection returns before
// Detach and must leave no hook behind on a keep-alive connection that
// goes on serving ordinary requests.
func TestEngineDisconnectHooksNotInstalledWhenRejected(t *testing.T) {
	ctx, _, hooks := newDetachedSSEContext(t)
	reject := errors.New("nope")
	h := New(Config{
		HeartbeatInterval: -1,
		OnConnect:         func(*celeris.Context, *Client) error { return reject },
		Handler:           func(*Client) { t.Error("handler must not run for a rejected connection") },
	})
	if err := h(ctx); !errors.Is(err, reject) {
		t.Fatalf("got %v, want OnConnect error", err)
	}
	errFn, closeFn, _ := hooks.snapshot()
	if errFn != nil || closeFn != nil {
		t.Fatalf("rejected OnConnect left engine hooks installed (OnError=%v OnDetachClose=%v)",
			errFn != nil, closeFn != nil)
	}
	if hooks.detached.Load() {
		t.Fatal("rejected OnConnect must not Detach")
	}
}

// TestEngineDisconnectLateFireAfterNormalReturn covers the other
// ordering: the handler returns on its own, the stream tears down, and
// only then does the engine close the conn and fire the hooks. The cancel
// they hold is already spent; nothing may panic or resurrect the stream.
func TestEngineDisconnectLateFireAfterNormalReturn(t *testing.T) {
	ctx, ms, hooks := newDetachedSSEContext(t)
	disconnected := make(chan struct{})
	h := New(Config{
		HeartbeatInterval: -1,
		OnDisconnect:      func(*celeris.Context, *Client) { close(disconnected) },
		Handler: func(client *Client) {
			_ = client.Send(Event{Data: "only"})
		},
	})
	if err := h(ctx); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	select {
	case <-disconnected:
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not finish within 2s")
	}
	errFn, closeFn, afterDetach := hooks.snapshot()
	if errFn == nil || closeFn == nil {
		t.Fatal("engine disconnect hooks not installed — celeris#494")
	}
	if afterDetach {
		t.Fatal("hooks installed AFTER Detach")
	}
	errFn(io.EOF)
	closeFn()
	ms.mu.Lock()
	closed := ms.closed
	ms.mu.Unlock()
	if !closed {
		t.Fatal("StreamWriter not closed after a normal handler return")
	}
}
