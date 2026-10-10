package stream

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// celeris#904, round 2 of the review of celeris#965: a detached Context takes
// the stream's ResponseWriter at call time (Context.StreamWriter), possibly
// after the handler returned. UseResponseWriter hands it over only for the use
// it was detached from, and a detached stream never goes back to the pool.

type rw904 struct{}

func (rw904) WriteResponse(*Stream, int, [][2]string, []byte) error { return nil }

func TestUseResponseWriterRefusedOnceTheUseEnds904(t *testing.T) {
	s := NewStream(1)
	s.ResponseWriter = rw904{}
	gen := s.Gen()
	if rw, ok := s.UseResponseWriter(gen); !ok || rw == nil {
		t.Fatalf("UseResponseWriter during the use = %v, %v; want the writer, true", rw, ok)
	}
	s.EndUse() // the handler returned
	if rw, ok := s.UseResponseWriter(gen); ok || rw != nil {
		t.Fatalf("UseResponseWriter after EndUse = %v, %v; want nil, false", rw, ok)
	}
	s.Release()
	if rw, ok := s.UseResponseWriter(gen); ok || rw != nil {
		t.Fatalf("UseResponseWriter after Release = %v, %v; want nil, false", rw, ok)
	}
}

// TestUseResponseWriterVersusRelease904: a goroutine asks for the writer in a
// loop while the stream is released. Under -race this finds a release that
// clears the field without the lock the read takes; and it must never see a
// writer once the use is over.
func TestUseResponseWriterVersusRelease904(t *testing.T) {
	for i := 0; i < 200; i++ {
		s := NewStream(1)
		s.ResponseWriter = rw904{}
		gen := s.Gen()
		var wg sync.WaitGroup
		var ended atomic.Bool // set after Release returned
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				over := ended.Load() // before the call: a writer returned after Release returned is a leak
				_, ok := s.UseResponseWriter(gen)
				if !ok {
					return
				}
				if over {
					t.Errorf("round %d: UseResponseWriter returned the writer after Release", i)
					return
				}
			}
		}()
		time.Sleep(time.Duration(i%5) * 20 * time.Microsecond)
		s.EndUse()
		s.Release()
		ended.Store(true)
		wg.Wait()
	}
}

// TestDetachedStreamIsNotPooled904: a stream a Context was detached from is
// reset when its use ends but is never handed to another request, whatever
// the pool's state; an undetached one still is.
func TestDetachedStreamIsNotPooled904(t *testing.T) {
	// One P so that sync.Pool's per-P private slot makes Put-then-Get return
	// the same object (the race detector drops a quarter of the Puts, hence the
	// repeats).
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	same := func(detach bool) bool {
		s := NewStream(1)
		s.ResponseWriter = rw904{}
		if detach {
			s.MarkDetached()
		}
		s.Release()
		n := NewStream(3)
		defer n.Release()
		return n == s
	}
	for i := 0; i < 50; i++ {
		if same(true) {
			t.Fatal("a detached stream was handed to the next request")
		}
	}
	pooled := false
	for i := 0; i < 50 && !pooled; i++ {
		pooled = same(false)
	}
	if !pooled {
		t.Error("an undetached stream was never reused: the pool is bypassed for every stream")
	}
	// Reset, either way: nothing of the use stays readable through the old pointer.
	s := NewStream(1)
	s.ResponseWriter = rw904{}
	s.Headers = append(s.Headers, [2]string{"x-b-secret", "v"})
	s.RemoteAddr = "10.0.0.1:1"
	s.MarkDetached()
	s.Release()
	if s.ResponseWriter != nil || len(s.Headers) != 0 || s.RemoteAddr != "" {
		t.Errorf("a released detached stream keeps the use's state: writer=%v headers=%v remote=%q", s.ResponseWriter, s.Headers, s.RemoteAddr)
	}
}
