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

// parkLimit855 is how long the fake holds a parked write that nothing cuts.
const parkLimit855 = 3 * time.Second

var errParkedUncut855 = errors.New("the write was parked and nothing cut it")

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
	if w.deadlinePassed() {
		return 0, os.ErrDeadlineExceeded
	}
	// A write nothing cuts must end the test, not hang it: a defect that
	// leaves a stalled client uncut fails after parkLimit855.
	for start := time.Now(); w.park.Load(); time.Sleep(time.Millisecond) {
		if w.deadlinePassed() {
			return 0, os.ErrDeadlineExceeded
		}
		if time.Since(start) > parkLimit855 {
			return 0, errParkedUncut855
		}
	}
	return len(p), nil
}

// deadlinePassed is whether a socket write would fail now: the write deadline
// is set and has passed.
func (w *dlWriter855) deadlinePassed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return !w.writeDL.IsZero() && !w.writeDL.After(time.Now())
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
// stream starts. It clears the write deadline net/http armed, and leaves the
// read deadline alone: on go1.27 it has been cleared before the handler runs
// (HTTP/1) or only closes a request body that has been read whole (h2), so
// clearing it would be a call with no effect (the round-2 review found no test
// could tell).
func TestStreamDetachedClearsTheWriteDeadline855(t *testing.T) {
	w := newDLWriter855()
	rw := &stdResponseWriter{w: w}
	rw.StreamDetached()
	if got, want := w.take(), []string{"w:zero"}; !eq855(got, want) {
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

// A detached stream's writes touch nothing but the writer; the last bytes of
// the response (the end of a chunked body) are bounded by one deadline at the
// end of the request. Before the stream detaches nothing is set, and with a
// WriteTimeout of -1 nothing is set at all.
func TestDetachedWriteEvents855(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
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
		if got, want := w.take(), []string{"write", "flush"}; !eq855(got, want) {
			t.Fatalf("a detached Write then Flush: %v, want %v", got, want)
		}
		rw.streamEnded()
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
// after it, whether or not the stream has a WriteTimeout. The deadline is in
// the past and stays there: no write sets another.
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
		if got := w.take(); !eq855(got, []string{"w:past"}) {
			t.Fatalf("WriteTimeout %v: the poke set %v", wt, got)
		}
		_ = rw.Write(nil, []byte("x"))
		_ = rw.Flush(nil)
		if got, want := w.take(), []string{"write", "flush"}; !eq855(got, want) {
			t.Fatalf("WriteTimeout %v: after the poke %v, want %v", wt, got, want)
		}
		rw.streamEnded()
		if got := w.take(); len(got) != 0 {
			t.Fatalf("WriteTimeout %v: a poked stream's end set %v", wt, got)
		}
		w.mu.Lock()
		last := kind855(w.writeDL)
		w.mu.Unlock()
		if last != "past" {
			t.Fatalf("WriteTimeout %v: the poked stream's write deadline is %s, want past", wt, last)
		}
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
// connection (ResponseController, the write deadline, then the poke's
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

// BenchmarkDetachedBookkeeping855 is what a detached stream's Write or Flush
// pays for the stall bound, apart from the write itself: "steady" is begin and
// done with the watchdog scheduled (the common event), "wake" is the first
// write after the watchdog stood down, which takes the mutex and resets the
// timer (once per WriteTimeout of idleness at most), "off" is a stream with
// WriteTimeout -1 (none).
func BenchmarkDetachedBookkeeping855(b *testing.B) {
	for _, mode := range []string{"off", "steady", "wake"} {
		b.Run(mode, func(b *testing.B) {
			wt := time.Hour
			if mode == "off" {
				wt = -1
			}
			e, cancel := engine855(wt)
			defer cancel()
			rw := &stdResponseWriter{w: &discardResponseWriter{h: http.Header{}}, eng: e}
			rw.StreamDetached()
			defer rw.streamEnded()
			d := rw.w.(*detachedWriter).d
			d.begin() // schedules the watchdog
			d.done()
			b.ReportAllocs()
			for b.Loop() {
				if mode == "wake" {
					d.watching.Store(false)
				}
				d.begin()
				d.done()
			}
		})
	}
}
