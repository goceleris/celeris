package std

// celeris#855, review round 1: what a detached stream's writes cost, and the
// watchdog that bounds them. Everything here uses only what the std bridge
// exposed before the watchdog (stdResponseWriter{w, eng}, StreamDetached,
// Write, Flush, streamEnded), so the same file builds against the per-write
// deadline design and the watchdog.

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/net/http2"

	"github.com/goceleris/celeris/internal/protocol/h2/stream"
)

// A detached stream's Write and Flush set no deadline: over h2c each
// SetWriteDeadline is a message to the connection's one serve goroutine, shared
// by every stream on it (the round-1 bench: +171% per event at 64 streams
// with four sets per event). The deadline is the watchdog's, set once when a
// write is found parked for WriteTimeout.
func TestDetachedWritesSetNoDeadlinePerWrite855(t *testing.T) {
	e, cancel := engine855(time.Second)
	defer cancel()
	w := newDLWriter855()
	rw := &stdResponseWriter{w: w, eng: e}
	rw.StreamDetached()
	w.take()
	const n = 100
	for range n {
		if err := rw.Write(nil, []byte("data: x\n\n")); err != nil {
			t.Fatal(err)
		}
		if err := rw.Flush(nil); err != nil {
			t.Fatal(err)
		}
	}
	got := w.take()
	if len(got) != 2*n {
		t.Fatalf("%d events of Write+Flush made %d calls on the writer, want %d (only the writes themselves): first ones %v",
			n, len(got), 2*n, got[:min(len(got), 8)])
	}
	for _, ev := range got {
		if strings.HasPrefix(ev, "w:") || strings.HasPrefix(ev, "r:") {
			t.Fatalf("a detached write set a deadline (%s) among its %d events", ev, len(got))
		}
	}
	rw.streamEnded()
}

// What every request pays for the detach support is nothing: the response
// writer is the size it was on main ({w, flushed}, the 24-byte class), so a
// request that never detaches allocates no more than it did (the round-2
// review found it had grown to the 48-byte class). The engine pointer takes the
// place of the flushed flag, which is kept on the stream (headersSent).
func TestStdResponseWriterSize855(t *testing.T) {
	type mainWriter struct { // stdResponseWriter on main, before celeris#855: {w, flushed}
		_ http.ResponseWriter
		_ bool
	}
	if got, want := unsafe.Sizeof(stdResponseWriter{}), unsafe.Sizeof(mainWriter{}); got != want {
		t.Fatalf("stdResponseWriter is %d bytes, want %d as on main (every request allocates one)", got, want)
	}
}

// The bridge answers for a handler that wrote nothing (200) or failed before
// it wrote anything (500), and leaves a response alone once its headers are
// out: the "flushed" state that decides it lives on the stream.
func TestBridgeFlushedState855(t *testing.T) {
	for _, tc := range []struct {
		name     string
		h        func(s *stream.Stream) error
		wantCode int
		wantBody string
	}{
		{"nothing written, handler ok", func(*stream.Stream) error { return nil }, 200, ""},
		{"nothing written, handler failed", func(*stream.Stream) error { return errors.New("boom") }, 500, "handler error\n"},
		{"headers written, handler failed", func(s *stream.Stream) error {
			_ = s.ResponseWriter.(stream.Streamer).WriteHeader(s, 202, nil)
			return errors.New("boom")
		}, 202, ""},
		{"response written, handler failed", func(s *stream.Stream) error {
			_ = s.ResponseWriter.WriteResponse(s, 201, nil, []byte("hi"))
			return errors.New("boom")
		}, 201, "hi"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, cancel := engine855(time.Second)
			defer cancel()
			br := &Bridge{engine: e, handler: streamFunc855(tc.h)}
			rec := httptest.NewRecorder()
			br.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
			if rec.Code != tc.wantCode || rec.Body.String() != tc.wantBody {
				t.Fatalf("answered %d %q, want %d %q", rec.Code, rec.Body.String(), tc.wantCode, tc.wantBody)
			}
		})
	}
}

type streamFunc855 func(s *stream.Stream) error

func (f streamFunc855) HandleStream(_ context.Context, s *stream.Stream) error { return f(s) }

// parkedWrite855 starts a write that the fake parks until a deadline in the
// past is set on it, and returns when it has been cut, with how long it was
// parked.
func parkedWrite855(rw *stdResponseWriter, w *dlWriter855) (parked time.Duration, err error) {
	w.park.Store(true)
	defer w.park.Store(false)
	start := time.Now()
	err = rw.Write(nil, []byte("x"))
	return time.Since(start), err
}

// A write parked for WriteTimeout is cut at WriteTimeout, not before and not
// long after; an idle stream is never cut, whatever the gap between events;
// and a stream that was idle (its watchdog stood down) is bounded again by its
// next write.
func TestDetachedParkedWriteIsCutAtWriteTimeout855(t *testing.T) {
	const wt = 150 * time.Millisecond
	e, cancel := engine855(wt)
	defer cancel()
	w := newDLWriter855()
	rw := &stdResponseWriter{w: w, eng: e}
	rw.StreamDetached()
	defer rw.streamEnded()
	w.take()

	if err := rw.Write(nil, []byte("x")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * wt) // idle for three WriteTimeouts
	if got := w.take(); !eq855(got, []string{"write"}) {
		t.Fatalf("an idle detached stream touched the connection: %v", got)
	}
	parked, err := parkedWrite855(rw, w)
	if err == nil {
		t.Fatal("a write parked for WriteTimeout was not cut")
	}
	if parked < wt || parked > 2*wt {
		t.Fatalf("the write was cut after %v, want about %v", parked, wt)
	}
	if got := w.take(); !eq855(got, []string{"write", "w:past"}) {
		t.Fatalf("events %v, want [write w:past]: the only deadline is the cut", got)
	}
	// The stream is cut for good: later writes fail at once.
	if err := rw.Write(nil, []byte("x")); err == nil {
		t.Fatal("a write after the cut succeeded")
	}
}

// The watchdog stands down when no write is in flight and starts again with
// the next one, many times over (the transition is the one race-prone step).
func TestDetachedWatchdogRestartsAfterIdle855(t *testing.T) {
	const wt = 60 * time.Millisecond
	e, cancel := engine855(wt)
	defer cancel()
	w := newDLWriter855()
	rw := &stdResponseWriter{w: w, eng: e}
	rw.StreamDetached()
	defer rw.streamEnded()
	for range 3 {
		if err := rw.Write(nil, []byte("x")); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2*wt + 10*time.Millisecond) // the watchdog fires on an idle stream and stands down
	}
	w.take()
	parked, err := parkedWrite855(rw, w)
	if err == nil || parked < wt || parked > 3*wt {
		t.Fatalf("after idling, a parked write ended after %v with %v, want a cut at about %v", parked, err, wt)
	}
}

// A write that is quick but whose stream is busy is never cut: events sent
// back to back for many WriteTimeouts.
func TestDetachedBusyStreamIsNotCut855(t *testing.T) {
	const wt = 40 * time.Millisecond
	e, cancel := engine855(wt)
	defer cancel()
	w := newDLWriter855()
	rw := &stdResponseWriter{w: w, eng: e}
	rw.StreamDetached()
	defer rw.streamEnded()
	w.take()
	for end := time.Now().Add(10 * wt); time.Now().Before(end); {
		if err := rw.Write(nil, []byte("x")); err != nil {
			t.Fatalf("a busy stream's write failed: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	for _, ev := range w.take() {
		if ev != "write" {
			t.Fatalf("a busy stream with no write parked got %q", ev)
		}
	}
}

// A poke (the drain budget) that comes while a write is in flight leaves the
// stream's write deadline in the past for good, however the write ends: the
// last bytes net/http writes are then bounded too (they fail).
func TestDetachedPokeDuringAWriteSticks855(t *testing.T) {
	e, cancel := engine855(time.Second)
	defer cancel()
	w := newDLWriter855()
	rw := &stdResponseWriter{w: w, eng: e}
	rw.StreamDetached()
	w.take()
	w.inWrite = func() {
		cancel() // the budget runs out in the middle of the write
		waitFor855(t, func() bool {
			w.mu.Lock()
			defer w.mu.Unlock()
			return kind855(w.writeDL) == "past"
		})
	}
	_ = rw.Write(nil, []byte("x"))
	w.inWrite = nil
	_ = rw.Flush(nil)
	rw.streamEnded()
	w.mu.Lock()
	last := kind855(w.writeDL)
	w.mu.Unlock()
	if last != "past" {
		t.Fatalf("the stream ended with a %s write deadline after a poke, want past", last)
	}
}

// testStream855 is the stream the Streamer methods are given; the bridge reads
// from it whether the response has begun.
func testStream855(tb testing.TB) *stream.Stream {
	s := stream.NewH1Stream(1)
	tb.Cleanup(s.Release)
	return s
}

// pastSets855 counts the deadlines in the past that were set on w, from its
// events (they are consumed).
func pastSets855(w *dlWriter855) int {
	n := 0
	for _, ev := range w.take() {
		if ev == "w:past" {
			n++
		}
	}
	return n
}

// A stream is cut once: when the watchdog has cut it, Shutdown's budget running
// out, the writes that follow and the end of the request set no deadline
// again. On h2c each deadline in the past is a message to the connection's
// serve goroutine that queues an RST_STREAM for the stream, so a second one
// is a second reset of a stream that is already closing.
func TestDetachedCutIsOnce855(t *testing.T) {
	const wt = 60 * time.Millisecond
	e, cancel := engine855(wt)
	defer cancel()
	w := newDLWriter855()
	rw := &stdResponseWriter{w: w, eng: e}
	rw.StreamDetached()
	w.take()
	if _, err := parkedWrite855(rw, w); err == nil {
		t.Fatal("a write parked for WriteTimeout was not cut")
	}
	cancel() // the budget runs out after the cut; the poke runs on its own goroutine
	time.Sleep(100 * time.Millisecond)
	for range 20 {
		if err := rw.Write(nil, []byte("x")); err == nil {
			t.Fatal("a write after the cut succeeded")
		}
		_ = rw.Flush(nil)
	}
	rw.streamEnded()
	if n := pastSets855(w); n != 1 {
		t.Fatalf("%d deadlines in the past were set on a stream cut once, want 1", n)
	}
}

// A poke is no more than one cut either, however many pokes there are.
func TestDetachedPokeIsOnce855(t *testing.T) {
	e, cancel := engine855(time.Second)
	defer cancel()
	w := newDLWriter855()
	rw := &stdResponseWriter{w: w, eng: e}
	rw.StreamDetached()
	w.take()
	cancel()
	waitFor855(t, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return kind855(w.writeDL) == "past"
	})
	d := rw.w.(*detachedWriter).d
	d.poke()
	d.poke()
	if err := rw.Write(nil, []byte("x")); err == nil {
		t.Fatal("a write after the poke succeeded")
	}
	rw.streamEnded()
	if n := pastSets855(w); n != 1 {
		t.Fatalf("%d deadlines in the past were set after one budget, want 1", n)
	}
}

// Context.Detach is public: a user may write to the stream from two
// goroutines. The bookkeeping holds (run under -race): two writers that are
// busy are never cut, and when both are parked they are both cut, by one
// deadline in the past, and the writes after fail without setting another.
func TestDetachedTwoWritersShareOneBound855(t *testing.T) {
	const wt = 80 * time.Millisecond
	e, cancel := engine855(wt)
	defer cancel()
	w := newDLWriter855()
	rw := &stdResponseWriter{w: w, eng: e}
	rw.StreamDetached()
	defer rw.streamEnded()
	w.take()
	var stop atomic.Bool
	var wg sync.WaitGroup
	var failed atomic.Int32
	writer := func() {
		defer wg.Done()
		for !stop.Load() {
			if err := rw.Write(nil, []byte("x")); err != nil {
				failed.Add(1)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}
	wg.Add(2)
	go writer()
	go writer()
	time.Sleep(8 * wt) // busy for eight WriteTimeouts
	if n := failed.Load(); n != 0 {
		stop.Store(true)
		wg.Wait()
		t.Fatalf("%d of two busy writers failed", n)
	}
	w.park.Store(true) // the client stops reading: both writers park
	start := time.Now()
	wg.Wait()
	if took := time.Since(start); took > 3*wt {
		t.Fatalf("two parked writers were cut after %v, want about %v", took, wt)
	}
	if n := failed.Load(); n != 2 {
		t.Fatalf("%d of two parked writers were cut", n)
	}
	if err := rw.Write(nil, []byte("x")); err == nil {
		t.Fatal("a write after the cut succeeded")
	}
	if n := pastSets855(w); n != 1 {
		t.Fatalf("%d deadlines in the past were set for two parked writers, want 1", n)
	}
}

// hijackWriter855 is a dlWriter855 that can hand over its connection.
type hijackWriter855 struct{ *dlWriter855 }

func (w hijackWriter855) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, _ := net.Pipe()
	return c, nil, nil
}

// A detached stream that hijacks goes on working: the connection is the
// caller's, and neither the watchdog nor the poke nor the end of the request
// sets a deadline on it. A writer that cannot hijack says so as it did before.
func TestDetachedHijack855(t *testing.T) {
	e, cancel := engine855(30 * time.Millisecond)
	defer cancel()
	w := newDLWriter855()
	rw := &stdResponseWriter{w: hijackWriter855{w}, eng: e}
	rw.StreamDetached()
	w.take()
	c, err := rw.Hijack(testStream855(t))
	if err != nil || c == nil {
		t.Fatalf("Hijack after Detach: %v, %v", c, err)
	}
	defer func() { _ = c.Close() }()
	cancel() // the budget runs out: nothing to poke
	time.Sleep(60 * time.Millisecond)
	rw.streamEnded()
	if got := w.take(); len(got) != 0 {
		t.Fatalf("a hijacked stream's connection got %v", got)
	}

	e2, cancel2 := engine855(time.Second)
	defer cancel2()
	rw = &stdResponseWriter{w: newDLWriter855(), eng: e2}
	rw.StreamDetached()
	if _, err := rw.Hijack(testStream855(t)); !errors.Is(err, stream.ErrHijackNotSupported) {
		t.Fatalf("Hijack on a writer that cannot: %v, want ErrHijackNotSupported", err)
	}
	rw.streamEnded()
}

// celeris#889 item 35 (from the #998 round-3 review): Context.Hijack followed
// by Context.Detach. Hijack goes through net/http's own writer, so
// detachedWriter.Hijack and abandon() never run; StreamDetached then wrapped the
// already-hijacked writer, and end() set a write deadline of WriteTimeout on the
// hijacked connection when ServeHTTP returned. The connection is the caller's:
// nothing of ours may touch it.
func TestHijackThenDetachSetsNoDeadline855(t *testing.T) {
	e, cancel := engine855(30 * time.Millisecond)
	defer cancel()
	w := newDLWriter855()
	rw := &stdResponseWriter{w: hijackWriter855{w}, eng: e}
	c, err := rw.Hijack(testStream855(t))
	if err != nil || c == nil {
		t.Fatalf("Hijack: %v, %v", c, err)
	}
	defer func() { _ = c.Close() }()
	w.take()
	rw.StreamDetached()
	if got := w.take(); len(got) != 0 {
		t.Fatalf("StreamDetached on a hijacked writer touched the connection: %v", got)
	}
	if name := fmt.Sprintf("%T", rw.w); !strings.HasSuffix(name, ".hijackedWriter") {
		t.Fatalf("a detach after a hijack wrapped the hijacked writer in %s, want it left as the hijackedWriter marker", name)
	}
	cancel() // the budget runs out: nothing to poke
	time.Sleep(60 * time.Millisecond)
	rw.streamEnded() // ServeHTTP returns: end() must not arm a write deadline
	if got := w.take(); len(got) != 0 {
		t.Fatalf("the end of a hijacked, detached request touched the connection: %v", got)
	}
}

// The same on a real connection: the hijacked connection is still writable
// well past WriteTimeout after the handler returned.
func TestHijackThenDetachKeepsTheConnectionPastWriteTimeout855(t *testing.T) {
	const wt = 150 * time.Millisecond
	e, cancel := engine855(wt)
	defer cancel()
	late := make(chan error, 1)
	again := make(chan error, 1)
	srv := &http.Server{WriteTimeout: wt, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		rw := &stdResponseWriter{w: w, eng: e}
		defer rw.streamEnded() // as Bridge.ServeHTTP does
		s := testStream855(t)
		conn, err := rw.Hijack(s)
		if err != nil {
			late <- err
			again <- err
			return
		}
		rw.StreamDetached()
		_, err2 := rw.Hijack(s)
		again <- err2
		go func() {
			defer func() { _ = conn.Close() }()
			time.Sleep(3 * wt) // past the deadline end() would have armed
			_, err := conn.Write([]byte("late"))
			late <- err
		}()
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(c, "GET / HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(c)
	if err := <-late; err != nil {
		t.Fatalf("a write to the hijacked connection %v after the handler returned failed: %v (read %q)", 3*wt, err, got)
	}
	if string(got) != "late" {
		t.Fatalf("the client read %q from the hijacked connection, want %q", got, "late")
	}
	if err := <-again; !errors.Is(err, http.ErrHijacked) {
		t.Fatalf("a second Hijack: %v, want http.ErrHijacked", err)
	}
}

// h2cServer855 serves h on an in-process h2c server (the x/net http2 server the std
// engine runs on) and returns a client for it.
func h2cServer855(tb testing.TB, h http.Handler) (cl *http.Client, url string) {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	h2s := &http2.Server{} //nolint:staticcheck // SA1019: the server the std engine's h2c runs on; see the import comment in engine.go.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				h2s.ServeConn(c, &http2.ServeConnOpts{Handler: h}) //nolint:staticcheck // SA1019: as above.
			}()
		}
	}()
	tr := &http2.Transport{AllowHTTP: true, //nolint:staticcheck // SA1019: an h2c client for the server above.
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) { //nolint:staticcheck // SA1019: as above.
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}}
	tb.Cleanup(func() {
		tr.CloseIdleConnections() //nolint:staticcheck // SA1019: as above.
		_ = ln.Close()
		wg.Wait()
	})
	return &http.Client{Transport: tr}, "http://" + ln.Addr().String() + "/"
}

// The watchdog's cut, the drain budget's poke and the handler's return race
// each other on a real connection: on HTTP/1 and on h2c, where a deadline is a
// message to the connection's serve goroutine (a deadline set under a lock the
// serve goroutine could be waiting for would deadlock here). Run under -race.
func TestDetachedCutPokeAndReturnRace855(t *testing.T) {
	big := make([]byte, 32<<20) // more than any window or socket buffer: the client does not read
	for _, proto := range []string{"h1", "h2c"} {
		t.Run(proto, func(t *testing.T) {
			var cutErrs atomic.Int32
			const iters = 24
			var wg sync.WaitGroup
			for i := range iters {
				e, cancel := engine855(40 * time.Millisecond)
				handlerDone := make(chan struct{})
				h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					defer close(handlerDone)
					rw := &stdResponseWriter{w: w, eng: e}
					rw.StreamDetached()
					defer rw.streamEnded()
					_ = rw.WriteHeader(testStream855(t), 200, nil)
					_ = rw.Flush(nil)
					// The budget runs out somewhere around the cut.
					time.AfterFunc(time.Duration(i%8)*10*time.Millisecond, cancel)
					if err := rw.Write(nil, big); err != nil {
						cutErrs.Add(1)
					}
				})
				var get func() (*http.Response, error)
				if proto == "h2c" {
					cl, url := h2cServer855(t, h)
					get = func() (*http.Response, error) { return cl.Get(url) }
				} else {
					get = newLiveServer855(t, h)
				}
				wg.Go(func() {
					defer cancel()
					// A budget that runs out before the response headers have
					// reached the client resets the stream under its Get: that
					// is the poke, not a failure. Either way the client reads
					// nothing, and the handler must return.
					if resp, err := get(); err == nil {
						defer func() { _ = resp.Body.Close() }()
					}
					// Do not read: the server's write is parked until it is cut.
					select {
					case <-handlerDone:
					case <-time.After(30 * time.Second):
						t.Errorf("%s #%d: the handler did not return", proto, i)
					}
				})
			}
			wg.Wait()
			if got := cutErrs.Load(); got != iters {
				t.Fatalf("%s: %d of %d parked writes were cut (errored)", proto, got, iters)
			}
		})
	}
}

// newLiveServer855 serves h on a real HTTP/1 server and returns a getter for it.
func newLiveServer855(tb testing.TB, h http.Handler) func() (*http.Response, error) {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(ln) }()
	cl := &http.Client{Transport: &http.Transport{}}
	tb.Cleanup(func() {
		cl.CloseIdleConnections()
		_ = srv.Close()
	})
	return func() (*http.Response, error) { return cl.Get("http://" + ln.Addr().String() + "/") }
}

var errBenchStream855 = errors.New("stream write failed")

// streamBench855 is one stream's (or 64 streams') Write+Flush of a small event
// on a live connection, h2c or HTTP/1. mode is "plain" (before the detach: no
// watchdog), "plain2" (the same again, the A/A floor) or "detached". One
// process per mode and round, the order rotated, under the timing lock: see
// scripts/bench-stream.sh.
func streamBench855(b *testing.B, proto, mode string, streams int) {
	e, cancel := engine855(time.Minute)
	defer cancel()
	payload := []byte("data: 12345678901234567890123456789012345678901234567890\n\n")
	ready := make(chan struct{}, streams)
	start := make(chan struct{})
	var wg sync.WaitGroup
	per := max(b.N/streams, 1)
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		defer wg.Done()
		rw := &stdResponseWriter{w: w, eng: e}
		if mode == "detached" {
			rw.StreamDetached()
		}
		ready <- struct{}{}
		<-start
		for range per {
			if err := rw.Write(nil, payload); err != nil {
				b.Error(errBenchStream855, err)
				return
			}
			if err := rw.Flush(nil); err != nil {
				b.Error(errBenchStream855, err)
				return
			}
		}
		rw.streamEnded()
	})
	var get func() (*http.Response, error)
	if proto == "h2c" {
		cl, url := h2cServer855(b, h)
		get = func() (*http.Response, error) { return cl.Get(url) }
	} else {
		get = newLiveServer855(b, h)
	}
	wg.Add(streams)
	var cwg sync.WaitGroup
	for range streams {
		cwg.Go(func() {
			resp, err := get()
			if err != nil {
				b.Error(err)
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		})
	}
	for range streams {
		<-ready
	}
	b.ReportAllocs()
	b.ResetTimer()
	close(start)
	wg.Wait()
	b.StopTimer()
	cwg.Wait()
}

func BenchmarkStreamWriteProto855(b *testing.B) {
	for _, proto := range []string{"h1", "h2c"} {
		for _, streams := range []int{1, 64} {
			for _, mode := range []string{"plain", "plain2", "detached"} {
				b.Run(fmt.Sprintf("%s/s%d/%s", proto, streams, mode), func(b *testing.B) { streamBench855(b, proto, mode, streams) })
			}
		}
	}
}
