package std

// celeris#855, review round 1: what a detached stream's writes cost, and the
// watchdog that bounds them. Everything here uses only what the std bridge
// exposed before the watchdog (stdResponseWriter{w, eng}, StreamDetached,
// Write, Flush, streamEnded), so the same file builds against the per-write
// deadline design and the watchdog.

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/net/http2"
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

// What every request pays for the detach support is one pointer in the
// response writer: 32 bytes, the size class above main's 24, not 48.
func TestStdResponseWriterSize855(t *testing.T) {
	if sz := unsafe.Sizeof(stdResponseWriter{}); sz > 32 {
		t.Fatalf("stdResponseWriter is %d bytes, want at most 32 (every request allocates one)", sz)
	}
}

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

// h2cServer855 serves h on an in-process h2c server (the x/net http2 server the std
// engine runs on) and returns a client for it.
func h2cServer855(tb testing.TB, h http.Handler) (cl *http.Client, url string) {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	h2s := &http2.Server{}
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
				h2s.ServeConn(c, &http2.ServeConnOpts{Handler: h})
			}()
		}
	}()
	tr := &http2.Transport{AllowHTTP: true, DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}
	tb.Cleanup(func() {
		tr.CloseIdleConnections()
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
					_ = rw.WriteHeader(nil, 200, nil)
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
					resp, err := get()
					if err != nil {
						t.Error(err)
						return
					}
					defer resp.Body.Close()
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
