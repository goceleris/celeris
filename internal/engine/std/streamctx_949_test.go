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

// The binding of a stream's context to the request's (celeris#949), made when
// the handler first asks for the context (celeris#889): Stream.BindParentContext.
// The counting parent is parentCtx889 (bridge_889_test.go).

// TestBindParentContextNeverReachesAReleasedStream949: the parent context ends
// as the request finishes, which is when net/http cancels r.Context(). A cancel
// that lands after the stream went back to its pool must not cancel the next
// request that gets the object: the registration cancels the context of its
// own use, and ends with it.
func TestBindParentContextNeverReachesAReleasedStream949(t *testing.T) {
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
				s.BindParentContext(ctx)
				_ = s.Context() // the handler asks: bound
				cancel()        // the parent ends as the handler returns
				s.Release()
				// The next request's stream: from the same pool, on this P.
				next := stream.NewStream(1)
				if next.IsCancelled() || next.Context().Err() != nil {
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

// TestBindParentContextEndsTheStreamContext949: the binding does what it is for.
func TestBindParentContextEndsTheStreamContext949(t *testing.T) {
	s := stream.NewStream(1)
	defer s.Release()
	ctx, cancel := context.WithCancel(context.Background())
	s.BindParentContext(ctx)
	if s.Context().Err() != nil {
		t.Fatal("the stream's context ended before the parent's")
	}
	cancel()
	select {
	case <-s.Context().Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the stream's context did not end with the parent's")
	}
	if err := s.Context().Err(); err != context.Canceled {
		t.Fatalf("stream context Err = %v, want context.Canceled", err)
	}
}

// TestBindParentContextRegistersOnFirstAskOnly949: nothing is registered with
// the parent until the context is asked for, once however often it is, and
// the registration is removed when the use ends; the parent cancelled after
// that reaches nothing.
func TestBindParentContextRegistersOnFirstAskOnly949(t *testing.T) {
	parent, cancel := newParent889()
	defer cancel()
	s := stream.NewStream(1)
	s.BindParentContext(parent)
	if n := parent.registered.Load(); n != 0 {
		t.Fatalf("BindParentContext registered %d times, want 0", n)
	}
	c := s.Context()
	for range 3 {
		if s.Context() != c {
			t.Fatal("Context returned a different context for the same use")
		}
	}
	if n := parent.registered.Load(); n != 1 {
		t.Fatalf("registered %d times after four asks, want 1", n)
	}
	s.Release()
	if n := parent.stopped.Load(); n != 1 {
		t.Fatalf("the registration was removed %d times at Release, want 1", n)
	}
	if c.Err() == nil {
		t.Fatal("the context of a released use is not cancelled")
	}
}

// A stream that was bound and not asked leaves nothing for its next use: the
// parent is dropped at the end of the use.
func TestBindParentContextIsDroppedAtTheEndOfTheUse949(t *testing.T) {
	parent, cancel := newParent889()
	defer cancel()
	s := stream.NewStream(1)
	s.BindParentContext(parent)
	s.Release() // never asked
	for range 50 {
		next := stream.NewStream(1) // very likely the same object, from the pool
		_ = next.Context()
		cancel()
		if next.Context().Err() != nil {
			t.Fatal("a stream's next use was bound to the previous request's context")
		}
		next.Release()
	}
	if n := parent.registered.Load(); n != 0 {
		t.Fatalf("the next use registered %d times with the previous request's context", n)
	}
}

// A parent that has ended already makes the context cancelled as it is made,
// with no goroutine and no registration (the rapid-reset shape: the client
// reset the stream before the handler started).
func TestBindParentContextAlreadyEndedIsSynchronous949(t *testing.T) {
	for i := range 2000 {
		parent, cancel := newParent889()
		cancel()
		s := stream.NewStream(1)
		s.BindParentContext(parent)
		select {
		case <-s.Context().Done():
		default:
			t.Fatalf("iteration %d: the stream's context has not ended on first ask, for a parent that had ended", i)
		}
		if err := s.Context().Err(); err != context.Canceled {
			t.Fatalf("iteration %d: stream context Err = %v, want context.Canceled", i, err)
		}
		if n := parent.registered.Load(); n != 0 {
			t.Fatalf("iteration %d: registered %d times with a parent that had ended", i, n)
		}
		s.Release()
	}
}

// A parent that can never end (context.Background) binds nothing: asking for
// the context costs what it does for a stream with no parent.
func TestBindParentContextBackgroundAddsNoAllocations949(t *testing.T) {
	run := func(bind bool) float64 {
		return testing.AllocsPerRun(200, func() {
			s := stream.NewStream(1)
			if bind {
				s.BindParentContext(context.Background())
			}
			_ = s.Context()
			s.Release()
		})
	}
	run(false)
	if a, b := run(false), run(true); b > a+0.5 {
		t.Fatalf("binding context.Background() added allocations: %.2f per use, %.2f without", b, a)
	}
}

// cancelInHandler949 is a stream handler that asks for its context and then
// cancels the request's context (a function it finds in ctx) as it returns:
// net/http cancels r.Context() around the end of ServeHTTP, and
// context.AfterFunc runs the context's cancel in a goroutine of its own.
type cancelInHandler949 struct{}

type cancelKey949 struct{}

func (cancelInHandler949) HandleStream(ctx context.Context, s *stream.Stream) error {
	_ = s.Context() // bound now
	err := s.ResponseWriter.WriteResponse(s, 200, [][2]string{{"content-type", "text/plain"}}, []byte("ok"))
	ctx.Value(cancelKey949{}).(context.CancelFunc)()
	return err
}

// TestBridgeEndsTheBindingBeforeItReleasesTheStream949: a cancel of the
// request's context that arrives as the request ends reaches nothing that
// outlives it: the next request's stream, from the pool the native engines
// draw from too, is not born cancelled.
func TestBridgeEndsTheBindingBeforeItReleasesTheStream949(t *testing.T) {
	e, err := New(resource.Config{Addr: "127.0.0.1:0", Engine: engine.Std, Protocol: engine.HTTP1}, cancelInHandler949{})
	if err != nil {
		t.Fatal(err)
	}
	br := &Bridge{engine: e, handler: cancelInHandler949{}}
	procs := max(runtime.GOMAXPROCS(0), 4)
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
				if next.IsCancelled() || next.Context().Err() != nil {
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
		t.Fatalf("%d of %d fresh streams were already cancelled by the request that used the object before", total, procs*perWorker)
	}
}
