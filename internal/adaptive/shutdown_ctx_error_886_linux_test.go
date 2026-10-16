//go:build linux

package adaptive

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
)

// celeris#886 item 15 (user decision 2026-10-13): Engine.Shutdown returns the
// context's error whenever ctx ended before it finished, on both paths:
//
//	path 1: ctx ends while Shutdown waits for e.mu behind a switch;
//	path 2: Shutdown holds e.mu and ctx ends while it waits for Listen to unwind.
//
// Path 2 used to fall through to the sub-engines' Shutdown, whose result is nil
// for epoll and io_uring, so a Shutdown that was cut short returned nil.

// errSub is a sub-engine whose Shutdown fails.
type errSub struct {
	*trackedSub
	err error
}

func (s *errSub) Shutdown(ctx context.Context) error {
	_ = s.trackedSub.Shutdown(ctx)
	return s.err
}

// openListenDone replaces the already-closed listenDone of the shutdown-window
// rig with one that stays open until release is called: a Listen that is still
// unwinding. release is idempotent and also runs at cleanup.
func openListenDone(t *testing.T, e *Engine) (release func()) {
	t.Helper()
	done := make(chan struct{})
	e.listenDone = done
	var closed bool
	release = func() {
		if !closed {
			closed = true
			close(done)
		}
	}
	t.Cleanup(release)
	return release
}

// TestShutdownReturnsCtxErrWhenListenUnwindsLate886 is path 2: Shutdown has
// the lock, Listen does not unwind within ctx. It must return ctx's error, and
// must still have stopped the sub-engines (Shutdown calls each sub twice: once
// to hand over the budget, once in stopSubEngines after the join).
func TestShutdownReturnsCtxErrWhenListenUnwindsLate886(t *testing.T) {
	e, active, _, _ := newShutdownWindowEngine(t, nil)
	standby := newTrackedSub(engine.IOUring)
	standby.listenCtx = active.listenCtx
	e.secondary = standby
	openListenDone(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := e.Shutdown(ctx)
	took := time.Since(start)
	t.Logf("path 2: deadline=100ms took=%v err=%v", took.Round(time.Millisecond), err)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Shutdown returned %v when ctx ended while Listen unwound, want context.DeadlineExceeded", err)
	}
	if took >= 2*time.Second {
		t.Errorf("Shutdown took %v, want about its 100ms deadline", took)
	}
	if n, _ := active.state(); n != 2 {
		t.Errorf("primary got %d Shutdown calls, want 2 (the sub-engines are stopped after ctx ended)", n)
	}
	if n, _ := standby.state(); n != 2 {
		t.Errorf("standby got %d Shutdown calls, want 2", n)
	}
	if e.listenCtx.Err() == nil {
		t.Error("the Listen context was not cancelled")
	}
}

// TestShutdownReturnsCanceledWhenListenUnwindsLate886: the error is ctx's own,
// not a deadline's: a cancel returns context.Canceled.
func TestShutdownReturnsCanceledWhenListenUnwindsLate886(t *testing.T) {
	e, _, _, _ := newShutdownWindowEngine(t, nil)
	openListenDone(t, e)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	if err := e.Shutdown(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Shutdown returned %v, want context.Canceled", err)
	}
}

// TestShutdownCtxErrKeepsSubEngineError886: a sub-engine's own failure is not
// lost behind ctx's error on path 2.
func TestShutdownCtxErrKeepsSubEngineError886(t *testing.T) {
	e, active, _, _ := newShutdownWindowEngine(t, nil)
	boom := errors.New("sub shutdown failed")
	bad := &errSub{trackedSub: active, err: boom}
	e.primary = bad
	openListenDone(t, e)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := e.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, boom) {
		t.Errorf("Shutdown returned %v, want both context.DeadlineExceeded and the sub-engine error", err)
	}
}

// TestShutdownNilWhenListenUnwindsInTime886 is the control for path 2: the same
// rig, Listen unwinding well inside ctx, returns nil (an ordinary shutdown).
func TestShutdownNilWhenListenUnwindsInTime886(t *testing.T) {
	e, active, _, _ := newShutdownWindowEngine(t, nil)
	release := openListenDone(t, e)
	time.AfterFunc(30*time.Millisecond, release)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := e.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown returned %v with Listen unwinding in time, want nil", err)
	}
	if n, _ := active.state(); n != 2 {
		t.Errorf("primary got %d Shutdown calls, want 2", n)
	}
}

// TestShutdownNilWhenCtxEndsAfterListenUnwound886: ctx is already done but
// Listen has already returned and e.mu is free: nothing was cut short, so the
// result is the sub-engines' (nil), though the join's select has two ready
// cases.
func TestShutdownNilWhenCtxEndsAfterListenUnwound886(t *testing.T) {
	for range 200 {
		e, _, _, _ := newShutdownWindowEngine(t, nil) // listenDone is closed
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := e.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown returned %v with Listen already unwound, want nil", err)
		}
	}
}

// TestShutdownReturnsCtxErrBehindSwitch886 is path 1 with a cancel instead of a
// deadline (the deadline form is TestShutdownHonoursCtxWhileSwitchBuildsStandby886):
// the error is ctx.Err() itself, identical to path 2's.
func TestShutdownReturnsCtxErrBehindSwitch886(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	e, _, _, _ := newShutdownWindowEngine(t, func() {
		close(entered)
		<-release
	})
	t.Cleanup(func() { close(release) })
	go e.ForceSwitch()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the switch never reached the standby build")
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	if err := e.Shutdown(ctx); err != context.Canceled {
		t.Errorf("Shutdown returned %v behind a switch, want context.Canceled", err)
	}
}
