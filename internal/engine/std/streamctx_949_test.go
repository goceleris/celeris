package std

import (
	"context"
	"runtime"
	"sync"
	"testing"

	"github.com/goceleris/celeris/internal/protocol/h2/stream"
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
