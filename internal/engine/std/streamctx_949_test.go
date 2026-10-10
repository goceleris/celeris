package std

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// TestBindStreamCancelNeverReachesAReleasedStream949: the parent context ends
// as the request finishes, which is when net/http cancels r.Context(). A
// cancel that lands after the stream went back to its pool would cancel the
// next request that gets the object. unbind must wait for a cancel that has
// started, so that none lands after it returns.
func TestBindStreamCancelNeverReachesAReleasedStream949(t *testing.T) {
	// At least 4 workers, whatever GOMAXPROCS is: the AfterFunc goroutine
	// races Release on one P too (a goroutine switch is enough).
	procs := max(runtime.GOMAXPROCS(0), 4)
	const perWorker = 20000
	var wg sync.WaitGroup
	bad := make(chan int, procs)
	for range procs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n := 0
			for range perWorker {
				s := stream.NewStream(1)
				ctx, cancel := context.WithCancel(context.Background())
				unbind := BindStreamCancel(ctx, s)
				cancel() // the parent ends as the handler returns
				unbind()
				s.Release()
				// The next request's stream: from the same pool, on this P.
				next := stream.NewStream(1)
				if next.IsCancelled() {
					n++
				}
				next.Release()
			}
			bad <- n
		}()
	}
	wg.Wait()
	close(bad)
	total := 0
	for n := range bad {
		total += n
	}
	if total != 0 {
		t.Fatalf("%d of %d fresh streams were already cancelled: a cancel from the previous request landed after its stream was released", total, procs*perWorker)
	}
}

// TestBindStreamCancelEndsTheStreamContext949: the binding does what it is for.
func TestBindStreamCancelEndsTheStreamContext949(t *testing.T) {
	s := stream.NewStream(1)
	defer s.Release()
	ctx, cancel := context.WithCancel(context.Background())
	unbind := BindStreamCancel(ctx, s)
	defer unbind()
	if s.Context().Err() != nil {
		t.Fatal("the stream's context ended before the parent's")
	}
	cancel()
	<-s.Context().Done()
	if err := s.Context().Err(); err != context.Canceled {
		t.Fatalf("stream context Err = %v, want context.Canceled", err)
	}
}

// After unbind the parent no longer reaches the stream.
func TestBindStreamCancelUnbindDetaches949(t *testing.T) {
	s := stream.NewStream(1)
	defer s.Release()
	ctx, cancel := context.WithCancel(context.Background())
	BindStreamCancel(ctx, s)()
	cancel()
	if s.IsCancelled() {
		t.Fatal("an unbound stream was cancelled by its former parent")
	}
}

// unbind is documented to be called once, but a second call (a deferred one
// and an explicit one, say) must not hang: with the AfterFunc stopped before
// it ran, the first call left nothing for the second to wait on.
func TestBindStreamCancelUnbindTwice949(t *testing.T) {
	for _, tc := range []struct {
		name string
		mk   func() (context.Context, func())
	}{
		{"cancelable", func() (context.Context, func()) { return context.WithCancel(context.Background()) }},
		{"cancelled-after-first-unbind", func() (context.Context, func()) { return context.WithCancel(context.Background()) }},
		{"background", func() (context.Context, func()) { return context.Background(), func() {} }},
		{"pre-cancelled", func() (context.Context, func()) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx, cancel
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := stream.NewStream(1)
			defer s.Release()
			ctx, cancel := tc.mk()
			defer cancel()
			unbind := BindStreamCancel(ctx, s)
			done := make(chan struct{})
			go func() {
				defer close(done)
				unbind()
				if tc.name == "cancelled-after-first-unbind" {
					cancel()
				}
				unbind()
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("a second unbind() did not return")
			}
		})
	}
}

// A context that is over already (the rapid-reset shape: the client reset the
// stream before the handler started) cancels the stream before
// BindStreamCancel returns, with no goroutine; a context that can never end
// (context.Background) binds nothing and allocates nothing.
func TestBindStreamCancelAlreadyEndedIsSynchronous949(t *testing.T) {
	for i := range 2000 {
		s := stream.NewStream(1)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		unbind := BindStreamCancel(ctx, s)
		if !s.IsCancelled() {
			t.Fatalf("iteration %d: the stream is not cancelled when BindStreamCancel returns for a context that has ended", i)
		}
		// What a handler sees: its context ended (the context is made on
		// the first look, after the cancel).
		select {
		case <-s.Context().Done():
		default:
			t.Fatalf("iteration %d: the stream's context has not ended when BindStreamCancel returns for a context that has ended", i)
		}
		if err := s.Context().Err(); err != context.Canceled {
			t.Fatalf("iteration %d: stream context Err = %v, want context.Canceled", i, err)
		}
		unbind()
		s.Release()
	}
}

func TestBindStreamCancelBackgroundAllocatesNothing949(t *testing.T) {
	s := stream.NewStream(1)
	defer s.Release()
	if n := testing.AllocsPerRun(100, func() { BindStreamCancel(context.Background(), s)() }); n != 0 {
		t.Fatalf("binding context.Background() allocated %v times per call, want 0", n)
	}
	if s.IsCancelled() {
		t.Fatal("binding context.Background() cancelled the stream")
	}
}

// cancelInHandler949 is a stream handler that cancels the request's context
// (a function it finds in ctx) as it returns: net/http cancels r.Context()
// around the end of ServeHTTP, and context.AfterFunc runs the stream's cancel
// in a goroutine of its own.
type cancelInHandler949 struct{}

type cancelKey949 struct{}

func (cancelInHandler949) HandleStream(ctx context.Context, s *stream.Stream) error {
	err := s.ResponseWriter.WriteResponse(s, 200, [][2]string{{"content-type", "text/plain"}}, []byte("ok"))
	ctx.Value(cancelKey949{}).(context.CancelFunc)()
	return err
}

// TestBridgeUnbindsBeforeItReleasesTheStream949 pins the order of the two
// defers in Bridge.ServeHTTP (and with it that Bridge uses BindStreamCancel's
// unbind at all): the cancel must be unbound, and a cancel that has started
// awaited, before the stream goes back to the pool the native engines draw
// from too. With the order swapped, the late cancel lands on a pooled stream
// and the next request's stream is born cancelled.
func TestBridgeUnbindsBeforeItReleasesTheStream949(t *testing.T) {
	e, err := New(resource.Config{Addr: "127.0.0.1:0", Engine: engine.Std, Protocol: engine.HTTP1}, cancelInHandler949{})
	if err != nil {
		t.Fatal(err)
	}
	br := &Bridge{engine: e, handler: cancelInHandler949{}}
	procs := max(runtime.GOMAXPROCS(0), 4)
	// With the order swapped nearly every iteration lands the cancel on the
	// pooled stream (159773 of 160000 at 20000 per worker), so a few hundred
	// are plenty; the root package under -race and -cover is near its budget.
	const perWorker = 1000
	var wg sync.WaitGroup
	bad := make(chan int, procs)
	for range procs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n := 0
			w := &discardResponseWriter{h: http.Header{}}
			base := httptest.NewRequest(http.MethodGet, "/", nil)
			base.ProtoMajor = 2 // an h2c stream
			for range perWorker {
				ctx, cancel := context.WithCancel(context.Background())
				ctx = context.WithValue(ctx, cancelKey949{}, cancel)
				clear(w.h)
				br.ServeHTTP(w, base.WithContext(ctx))
				next := stream.NewStream(1) // the next request's, from the same pool
				if next.IsCancelled() {
					n++
				}
				next.Release()
			}
			bad <- n
		}()
	}
	wg.Wait()
	close(bad)
	total := 0
	for n := range bad {
		total += n
	}
	if total != 0 {
		t.Fatalf("%d of %d fresh streams were already cancelled: Bridge released its stream before it unbound the cancel", total, procs*perWorker)
	}
}
