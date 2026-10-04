package stream

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// celeris#836: an HTTP/2 stream's context was a view of the pooled Stream
// (its flags and done channel), so once the stream was reset for its next use
// the context of the use before it said "not cancelled" again. A context
// derived from it (context.WithCancel, WithTimeout, AfterFunc) starts a
// propagation goroutine that reads the parent's Err after it sees the
// parent's Done closed; when the reset came in between, Err was nil and the
// context package panicked ("context: internal error: missing cancel error"),
// which killed the process. Each use of a stream now has a context of its own
// that nothing resets.

// TestDerivedContextAfterReleaseDoesNotPanic836 is the triage probe: derive a
// context from a stream's, cancel it, release the stream, many times. Before
// the fix the test binary died within 0.2 s.
func TestDerivedContextAfterReleaseDoesNotPanic836(t *testing.T) {
	for _, tc := range []struct {
		name   string
		derive func(context.Context) (context.Context, context.CancelFunc)
	}{
		{"WithCancel", context.WithCancel},
		// The soak's frames: timerCtx.cancel <- propagateCancel.func2.
		{"WithTimeout", func(p context.Context) (context.Context, context.CancelFunc) {
			return context.WithTimeout(p, time.Hour)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for i := range 200000 {
				s := NewStream(uint32(i*2 + 1))
				ctx, cancel := tc.derive(s.Context())
				cancel()
				_ = ctx
				s.Release()
			}
		})
	}
}

// TestStreamContextStaysCancelledAfterRelease836: once a use of a stream is
// over, its context is cancelled for good. Done stays closed and Err stays
// context.Canceled, whether or not Done was asked for before the release.
func TestStreamContextStaysCancelledAfterRelease836(t *testing.T) {
	for _, watched := range []bool{true, false} {
		s := NewStream(1)
		ctx := s.Context()
		if watched {
			_ = ctx.Done() // what a derived context does at once
		}
		s.Release()
		select {
		case <-ctx.Done():
		default:
			t.Errorf("watched=%v: Done is not closed after Release", watched)
		}
		if err := ctx.Err(); !errors.Is(err, context.Canceled) {
			t.Errorf("watched=%v: Err after Release = %v, want context.Canceled", watched, err)
		}
	}
}

// TestStreamContextOfOneUseIsNotTheNext836 reuses one Stream object for a
// second use, as the pool and the inline path do. The first use's context
// must stay cancelled and must not watch the second use: a context derived
// from it later is cancelled at once, and the second use's context is a
// different, live one.
func TestStreamContextOfOneUseIsNotTheNext836(t *testing.T) {
	check := func(t *testing.T, ctx1 context.Context, s *Stream) {
		t.Helper()
		if err := ctx1.Err(); !errors.Is(err, context.Canceled) {
			t.Fatalf("the first use's context: Err = %v after the stream's next use began, want context.Canceled", err)
		}
		select {
		case <-ctx1.Done():
		default:
			t.Fatal("the first use's context: Done is open again after the stream's next use began")
		}
		child, cancel := context.WithCancel(ctx1)
		defer cancel()
		if child.Err() == nil {
			t.Fatal("a context derived from the first use's context is live, parented on the next use")
		}
		ctx2 := s.Context()
		if ctx2 == ctx1 {
			t.Fatal("the next use of the stream got the first use's context")
		}
		if err := ctx2.Err(); err != nil {
			t.Fatalf("the next use's context: Err = %v before it is cancelled, want nil", err)
		}
		s.Cancel()
		if !errors.Is(ctx2.Err(), context.Canceled) {
			t.Fatalf("the next use's context: Err = %v after Cancel, want context.Canceled", ctx2.Err())
		}
	}

	t.Run("inline-reset", func(t *testing.T) {
		s := NewStream(1)
		ctx1 := s.Context()
		_ = ctx1.Done()
		s.Cancel()
		ResetH2StreamInline(s, 3)
		check(t, ctx1, s)
		s.Release()
	})

	t.Run("inline-reset-without-cancel", func(t *testing.T) {
		// The inline reset ends a use without Cancel: the context it
		// handed out must end with it, not stay live forever.
		s := NewStream(1)
		ctx1 := s.Context()
		ResetH2StreamInline(s, 3)
		check(t, ctx1, s)
		s.Release()
	})

	t.Run("pool", func(t *testing.T) {
		// sync.Pool usually hands the object just Put back to the same
		// goroutine (not always: the race detector drops a quarter of
		// the Puts), so count the reuses and require some.
		reused := 0
		for i := 0; i < 200 && reused < 20; i++ {
			s := NewStream(1)
			ctx1 := s.Context()
			if i%2 == 0 {
				_ = ctx1.Done()
			}
			s.Release()
			s2 := NewStream(3)
			if s2 == s {
				reused++
				check(t, ctx1, s2)
			}
			s2.Release()
		}
		if reused == 0 {
			t.Fatal("the stream pool never handed back a released stream: this case checked nothing")
		}
	})
}

// TestStreamContextCancelRaces836 races a use's first Context() against
// Cancel and against Release (run it with -race). Whatever the order, the
// context ends cancelled and a derived context ends with the parent's Err.
func TestStreamContextCancelRaces836(t *testing.T) {
	for i := range 2000 {
		s := NewStream(uint32(i*2 + 1))
		var wg sync.WaitGroup
		ctxs := make([]context.Context, 4)
		for j := range ctxs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c := s.Context()
				if j%2 == 0 {
					_ = c.Done()
				}
				ctxs[j] = c
			}()
		}
		wg.Add(1)
		go func() { defer wg.Done(); s.Cancel() }()
		wg.Wait()
		children := make([]context.Context, len(ctxs))
		cancels := make([]context.CancelFunc, len(ctxs))
		for j, c := range ctxs {
			if c != ctxs[0] {
				t.Fatalf("iteration %d: one use of a stream handed out two contexts", i)
			}
			children[j], cancels[j] = context.WithTimeout(c, time.Hour)
		}
		s.Release()
		for j, child := range children {
			<-child.Done()
			if !errors.Is(child.Err(), context.Canceled) {
				t.Fatalf("iteration %d: derived context %d ended with %v, want context.Canceled", i, j, child.Err())
			}
			cancels[j]()
		}
	}
}

// TestStreamContextDoneIsOneChannel836: context.Context says successive calls
// to Done return the same value. Two first calls to Done racing a Cancel must
// get the same channel, and so must every call after them, and the context
// must say it has one (HasDoneCh).
func TestStreamContextDoneIsOneChannel836(t *testing.T) {
	for i := range 20000 {
		s := NewStream(uint32(i*2 + 1))
		c := s.Context()
		var wg sync.WaitGroup
		var got [2]<-chan struct{}
		start := make(chan struct{})
		for j := range got {
			wg.Add(1)
			go func() { defer wg.Done(); <-start; got[j] = c.Done() }()
		}
		wg.Add(1)
		go func() { defer wg.Done(); <-start; s.Cancel() }()
		close(start)
		wg.Wait()
		if got[0] != got[1] || c.Done() != got[0] {
			t.Fatalf("iteration %d: Done returned different channels to callers racing a Cancel", i)
		}
		if !s.HasDoneCh() {
			t.Fatalf("iteration %d: Done was asked for, and HasDoneCh says it was not", i)
		}
		<-got[0]
		s.Release()
	}
}

// TestH1StreamContextIsBackground836: an HTTP/1 stream's context is
// context.Background(), as before (it is never cancelled by the stream).
func TestH1StreamContextIsBackground836(t *testing.T) {
	s := NewH1Stream(1)
	defer s.Release()
	if s.Context() != context.Background() {
		t.Fatalf("H1 stream context = %T, want context.Background()", s.Context())
	}
}
