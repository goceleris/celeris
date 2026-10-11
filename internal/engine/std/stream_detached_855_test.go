package std

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// dlWriter855 is an http.ResponseWriter that can set deadlines (what
// http.ResponseController asks for) and records them, and the writes between.
type dlWriter855 struct {
	mu     sync.Mutex
	h      http.Header
	events []string // "r:<zero|future|past>", "w:<...>", "write", "flush"
	// writeDL is the write deadline last set.
	writeDL time.Time
	// beforeSet, if set, runs on the caller's goroutine at the start of
	// SetWriteDeadline, before it is recorded.
	beforeSet func(time.Time)
	// park makes Write wait, as a socket write to a client that reads nothing
	// does, until a write deadline in the past is set: it then fails.
	park atomic.Bool
	// inWrite, if set, runs in the middle of Write.
	inWrite func()
}

func newDLWriter855() *dlWriter855 { return &dlWriter855{h: http.Header{}} }

func kind855(t time.Time) string {
	switch {
	case t.IsZero():
		return "zero"
	case t.After(time.Now()):
		return "future"
	}
	return "past"
}

func (w *dlWriter855) Header() http.Header { return w.h }
func (w *dlWriter855) WriteHeader(int)     {}
func (w *dlWriter855) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.events = append(w.events, "write")
	w.mu.Unlock()
	if f := w.inWrite; f != nil {
		f()
	}
	for w.park.Load() {
		w.mu.Lock()
		dl := w.writeDL
		w.mu.Unlock()
		if !dl.IsZero() && !dl.After(time.Now()) {
			return 0, os.ErrDeadlineExceeded
		}
		time.Sleep(time.Millisecond)
	}
	return len(p), nil
}
func (w *dlWriter855) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.events = append(w.events, "flush")
}
func (w *dlWriter855) SetReadDeadline(t time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.events = append(w.events, "r:"+kind855(t))
	return nil
}
func (w *dlWriter855) SetWriteDeadline(t time.Time) error {
	if f := w.beforeSet; f != nil {
		f(t)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.events = append(w.events, "w:"+kind855(t))
	w.writeDL = t
	return nil
}
func (w *dlWriter855) take() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	ev := w.events
	w.events = nil
	return ev
}

func eq855(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// engine855 is an engine with just what a detached stream reads: the write
// timeout of its server and the base context. cancel is the drain budget
// running out.
func engine855(wt time.Duration) (e *Engine, cancel context.CancelFunc) {
	e = &Engine{server: &http.Server{WriteTimeout: wt}}
	e.baseCtx, cancel = context.WithCancel(context.Background())
	return e, cancel
}

// celeris#855: StreamDetached is what Context.Detach calls on std, once when a
// stream starts. It clears both deadlines net/http armed (the read one is
// pinned here: no end-to-end test can fail without it on go1.27, where
// net/http has cleared it before the handler runs).
func TestStreamDetachedClearsBothDeadlines855(t *testing.T) {
	w := newDLWriter855()
	rw := &stdResponseWriter{w: w}
	rw.StreamDetached()
	if got, want := w.take(), []string{"r:zero", "w:zero"}; !eq855(got, want) {
		t.Fatalf("StreamDetached set %v, want %v", got, want)
	}
	rw.StreamDetached() // a second call changes nothing
	if got := w.take(); len(got) != 0 {
		t.Fatalf("a second StreamDetached set %v", got)
	}
}

// A writer that cannot set deadlines (a recorder, a wrapper that hides the
// connection) has none armed to clear, and still writes.
func TestStreamDetachedOnAWriterWithoutDeadlines855(t *testing.T) {
	rec := httptest.NewRecorder()
	e, cancel := engine855(time.Second)
	defer cancel()
	rw := &stdResponseWriter{w: rec, eng: e}
	rw.StreamDetached()
	defer rw.streamEnded()
	if err := rw.Write(nil, []byte("data: 1\n\n")); err != nil {
		t.Fatal(err)
	}
	if err := rw.Flush(nil); err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != "data: 1\n\n" {
		t.Fatalf("body %q", got)
	}
	cancel() // the budget runs out: poke finds nothing to set, and does not panic
	time.Sleep(20 * time.Millisecond)
}

// Each write of a detached stream is armed with a deadline and disarmed when it
// returns; before the stream detaches, and with a WriteTimeout of -1, nothing
// is set.
func TestDetachedWriteArmsAndDisarms855(t *testing.T) {
	t.Run("armed", func(t *testing.T) {
		e, cancel := engine855(time.Second)
		defer cancel()
		w := newDLWriter855()
		rw := &stdResponseWriter{w: w, eng: e}
		_ = rw.Write(nil, []byte("x"))
		if got := w.take(); !eq855(got, []string{"write"}) {
			t.Fatalf("before the detach: %v", got)
		}
		rw.StreamDetached()
		w.take()
		_ = rw.Write(nil, []byte("x"))
		_ = rw.Flush(nil)
		want := []string{"w:future", "write", "w:zero", "w:future", "flush", "w:zero"}
		if got := w.take(); !eq855(got, want) {
			t.Fatalf("a detached Write then Flush: %v, want %v", got, want)
		}
		rw.streamEnded()
		// The last bytes of the response (end of a chunked body) are bounded too.
		if got := w.take(); !eq855(got, []string{"w:future"}) {
			t.Fatalf("at the end of the request: %v", got)
		}
	})
	t.Run("no-timeout", func(t *testing.T) {
		e, cancel := engine855(-1) // stdTimeout of a disabled WriteTimeout
		defer cancel()
		w := newDLWriter855()
		rw := &stdResponseWriter{w: w, eng: e}
		rw.StreamDetached()
		w.take()
		_ = rw.Write(nil, []byte("x"))
		_ = rw.Flush(nil)
		rw.streamEnded()
		if got, want := w.take(), []string{"write", "flush"}; !eq855(got, want) {
			t.Fatalf("with no WriteTimeout: %v, want %v", got, want)
		}
	})
}

// The drain budget running out fails the stream's writes, and every write
// after it, whether or not the stream has a WriteTimeout.
func TestDetachedPokeFailsWrites855(t *testing.T) {
	for _, wt := range []time.Duration{time.Second, -1} {
		e, cancel := engine855(wt)
		w := newDLWriter855()
		rw := &stdResponseWriter{w: w, eng: e}
		rw.StreamDetached()
		w.take()
		cancel()
		waitFor855(t, func() bool {
			w.mu.Lock()
			defer w.mu.Unlock()
			return !w.writeDL.IsZero() && kind855(w.writeDL) == "past"
		})
		w.take()
		_ = rw.Write(nil, []byte("x"))
		_ = rw.Flush(nil)
		got := w.take()
		for _, ev := range got {
			if ev == "w:future" || ev == "w:zero" {
				t.Fatalf("WriteTimeout %v: after the poke a write was armed %v", wt, got)
			}
		}
		if len(got) == 0 || got[0] != "w:past" {
			t.Fatalf("WriteTimeout %v: after the poke the first write was not failed: %v", wt, got)
		}
		rw.streamEnded()
		if got := w.take(); len(got) != 0 {
			t.Fatalf("WriteTimeout %v: a poked stream's end set %v", wt, got)
		}
	}
}

// A poke that lands between arm's look at closing and its set would be undone
// by the set: arm looks again once the deadline is in place. The poke is made
// to land exactly there.
func TestDetachedArmDoesNotUndoAPoke855(t *testing.T) {
	e, cancel := engine855(time.Second)
	defer cancel()
	w := newDLWriter855()
	rw := &stdResponseWriter{w: w, eng: e}
	rw.StreamDetached()
	w.take()
	fired := false
	w.beforeSet = func(dl time.Time) {
		if fired || kind855(dl) != "future" {
			return
		}
		fired = true
		cancel() // the budget runs out, here
		waitFor855(t, func() bool {
			w.mu.Lock()
			defer w.mu.Unlock()
			return kind855(w.writeDL) == "past"
		})
	}
	_ = rw.Write(nil, []byte("x"))
	w.mu.Lock()
	last := kind855(w.writeDL)
	w.mu.Unlock()
	if !fired {
		t.Fatal("the poke was not made")
	}
	if last != "past" {
		t.Fatalf("a Write racing the poke left a %s write deadline, want past", last)
	}
}

// Nothing touches the response once the request is over: a poke that comes
// after it would set a deadline for the next request on a kept-alive
// connection.
func TestDetachedPokeAfterEndIsNothing855(t *testing.T) {
	e, cancel := engine855(time.Second)
	w := newDLWriter855()
	rw := &stdResponseWriter{w: w, eng: e}
	rw.StreamDetached()
	rw.streamEnded()
	w.take()
	cancel()
	time.Sleep(50 * time.Millisecond)
	if got := w.take(); len(got) != 0 {
		t.Fatalf("a poke after the end of the request set %v", got)
	}
	// And a poke the end raced with: poke has run, end follows.
	d := &detachedWrites{rc: http.NewResponseController(w)}
	d.end()
	d.poke()
	if got := w.take(); len(got) != 0 {
		t.Fatalf("poke after end set %v", got)
	}
}

// Writes, pokes and ends racing (run under -race): no data race, and no
// deadlock (the test ends).
func TestDetachedWritesRace855(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for range 64 {
			wg.Go(func() {
				e, cancel := engine855(time.Millisecond)
				w := newDLWriter855()
				rw := &stdResponseWriter{w: w, eng: e}
				rw.StreamDetached()
				var inner sync.WaitGroup
				inner.Go(func() {
					for range 50 {
						_ = rw.Write(nil, []byte("x"))
						_ = rw.Flush(nil)
					}
				})
				inner.Go(cancel)
				inner.Wait()
				rw.streamEnded()
				cancel()
			})
		}
		wg.Wait()
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("detached writes, pokes and ends deadlocked")
	}
}

func waitFor855(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("condition not reached in 5 s")
}

// liveWriter855 runs fn with the http.ResponseWriter of a live request on a
// real connection, whose client reads and discards the stream, on the
// calling goroutine (the handler is held open for the call).
func liveWriter855(tb testing.TB, fn func(w http.ResponseWriter)) {
	tb.Helper()
	ch := make(chan http.ResponseWriter)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		ch <- w
		<-release
	}))
	defer srv.Close()
	errc := make(chan error, 1)
	go func() {
		resp, err := http.Get(srv.URL)
		if err == nil {
			_, err = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		errc <- err
	}()
	w := <-ch
	fn(w)
	close(release)
	if err := <-errc; err != nil && !errors.Is(err, io.EOF) {
		tb.Fatal(err)
	}
}

// BenchmarkStreamDetached855 is the cost of the call itself on a live
// connection (ResponseController, then both deadlines, then the poke's
// registration): what a stream pays once, when it starts. Requests that do not
// detach never reach it.
func BenchmarkStreamDetached855(b *testing.B) {
	e, cancel := engine855(time.Minute)
	defer cancel()
	liveWriter855(b, func(w http.ResponseWriter) {
		b.ReportAllocs()
		for b.Loop() {
			rw := &stdResponseWriter{w: w, eng: e}
			rw.StreamDetached()
			rw.streamEnded()
		}
	})
}

// BenchmarkStreamWrite855 is a stream's Write and Flush of one small event on
// a live connection, as the SSE middleware sends them: "plain" as before the
// detach (net/http's one deadline), "detached" with a deadline armed and
// cleared around each (the cost of the per-write bound).
func BenchmarkStreamWrite855(b *testing.B) {
	payload := []byte("data: 12345678901234567890123456789012345678901234567890\n\n")
	for _, mode := range []string{"plain", "detached"} {
		b.Run(mode, func(b *testing.B) {
			e, cancel := engine855(time.Minute)
			defer cancel()
			liveWriter855(b, func(w http.ResponseWriter) {
				rw := &stdResponseWriter{w: w, eng: e}
				if mode == "detached" {
					rw.StreamDetached()
				}
				b.ReportAllocs()
				for b.Loop() {
					if err := rw.Write(nil, payload); err != nil {
						b.Fatal(err)
					}
					if err := rw.Flush(nil); err != nil {
						b.Fatal(err)
					}
				}
				rw.streamEnded()
			})
		})
	}
}
