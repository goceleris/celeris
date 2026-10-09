package std

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/resource"
)

// holdDrain boots an HTTP/1 std engine whose one request is held by a handler
// that ignores its context, so a drain cannot complete until the test
// releases it, and returns once Shutdown's first call holds the drain: it has
// been started with longCtx and the listener refuses connections (the first
// thing http.Server.Shutdown does). The returned channel carries that first
// call's result.
func holdDrain(longCtx context.Context, t *testing.T) (e *Engine, first <-chan error) {
	t.Helper()
	h := &heldHandler{started: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(h.release) }) })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	e, err = New(resource.Config{
		Listener:     ln,
		Engine:       engine.Std,
		Protocol:     engine.HTTP1,
		ReadTimeout:  -1,
		WriteTimeout: -1,
	}, h)
	if err != nil {
		_ = ln.Close()
		t.Fatalf("New: %v", err)
	}
	listenCtx, cancelListen := context.WithCancel(context.Background())
	t.Cleanup(cancelListen)
	go func() { _ = e.Listen(listenCtx) }()

	client := &http.Client{Timeout: 30 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	go func() {
		for deadline := time.Now().Add(5 * time.Second); ; {
			resp, err := client.Get("http://" + addr + "/held")
			if err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				return
			}
			if time.Now().After(deadline) {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	select {
	case <-h.started:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not start within 5s")
	}

	ch := make(chan error, 1)
	go func() { ch <- e.Shutdown(longCtx) }()
	for deadline := time.Now().Add(5 * time.Second); ; {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err != nil {
			break
		}
		_ = c.Close()
		if time.Now().After(deadline) {
			t.Fatal("the listener still accepted 5s after Shutdown began")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return e, ch
}

// TestOverlappingShutdownLiveCallerIsToldWhoseBudgetEndedTheDrain pins
// celeris#879.
//
// The drain is shared, and any Shutdown call whose ctx expires ends it (the
// shortest budget governs; that is also what lets a second call with a done
// ctx force a stop). The call whose own ctx is still live then used to get the
// drain's internal context.Canceled back: its own ctx.Err() is nil, so the
// "this call's budget ran out" branch is skipped and the internal error
// passes through, naming no budget. It has to be an error that says whose
// budget ended the drain, and never nil, as handlers may still be running and
// the OnShutdown hooks are about to run.
func TestOverlappingShutdownLiveCallerIsToldWhoseBudgetEndedTheDrain(t *testing.T) {
	const bound = 3 * time.Second
	for _, tc := range []struct {
		name string
		// end ends the drain through the second call; want is the error
		// that call returns and the live first call must report too.
		end  func() (context.Context, context.CancelFunc)
		want error
	}{
		{
			name: "deadline",
			end: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 200*time.Millisecond)
			},
			want: context.DeadlineExceeded,
		},
		{
			// The second signal that means "stop now": a ctx already done.
			name: "forced",
			end: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, cancel
			},
			want: context.Canceled,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			liveCtx, cancelLive := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancelLive()
			e, first := holdDrain(liveCtx, t)

			endCtx, cancelEnd := tc.end()
			defer cancelEnd()
			if got := e.Shutdown(endCtx); !errors.Is(got, tc.want) {
				t.Fatalf("the call whose budget ended the drain returned %v, want %v", got, tc.want)
			}

			select {
			case err := <-first:
				if liveCtx.Err() != nil {
					t.Fatalf("the first call's own ctx expired (%v): the test did not hold what it means to", liveCtx.Err())
				}
				if err == nil {
					t.Fatal("the live first call returned nil, although another call's budget ended the drain with its handler still running")
				}
				if err == context.Canceled { //nolint:errorlint // the point: not the bare internal error
					t.Errorf("the live first call got the bare internal context.Canceled, which names no budget (celeris#879)")
				}
				if !errors.Is(err, tc.want) {
					t.Errorf("the live first call returned %v, want an error wrapping %v, the ending call's ctx error", err, tc.want)
				}
			case <-time.After(bound):
				t.Fatalf("the live first call (30s budget) had not returned %v after the second call's budget ended the drain", bound)
			}

			// A call that arrives once the drain has ended is told the same.
			late := e.Shutdown(liveCtx)
			if late == nil || !errors.Is(late, tc.want) || late == tc.want { //nolint:errorlint // see above
				t.Errorf("a call after the drain ended returned %v, want a wrapped %v", late, tc.want)
			}
		})
	}
}
