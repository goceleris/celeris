//go:build linux

package iouring

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#1173: the write clock of an HTTP/2 response a handler's goroutine
// writes after a quiet gap. flushSend stamps lastActivity, from which the sweep
// measures WriteTimeout for a conn with a SEND in flight, with w.cachedNow: the
// clock the loop read for its last batch of completions or its last sweep. A
// worker that is woken for the handler's write without a completion (the
// 100 us poll of an HTTP/2 conn, a timed wait that ran out) stamped a clock as
// old as that last read, so a stream quiet for most of WriteTimeout had a
// write clock that was already most of WriteTimeout old when its SEND went out
// (measured 70 ms of 100 in CI-like runs), and a sweep that found the SEND
// still in flight (the worker descheduled between the submit and the reaping of
// its completion) closed the conn mid-stream.

const writeTimeout1173 = 100 * time.Millisecond

type gate1173 struct {
	started chan struct{}
	gate    chan struct{}
}

func (g *gate1173) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	for _, hh := range s.Headers {
		if hh[0] == ":path" && hh[1] == "/gate" {
			g.started <- struct{}{}
			<-g.gate
		}
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "text/plain"}, {"content-length", "2"}}, []byte("ok"))
}
func (g *gate1173) RouteAsync(_, path string) bool { return path == "/gate" }
func (g *gate1173) HasAsyncRoutes() bool           { return true }

func TestWriteClockStampAfterAQuietGapIsFresh1173(t *testing.T) {
	g := &gate1173{started: make(chan struct{}, 1), gate: make(chan struct{})}
	addr := freeAddr639(t)
	var run *engineRun896
	_, _, err := retryRingENOMEM(func() error {
		e, err := New(resource.Config{
			Addr:          addr,
			Engine:        engine.IOUring,
			Protocol:      engine.Auto,
			AsyncHandlers: true,
			// The shape of TestQuietH2StreamIsNotCutByWriteTimeout889: the only
			// timeout on is WriteTimeout, and it is short.
			ReadTimeout:  -1,
			IdleTimeout:  -1,
			WriteTimeout: writeTimeout1173,
			Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
			Resources:    resource.Resources{Workers: 2},
		}, g)
		if err != nil {
			skipOrFail656(t, "New: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- e.Listen(ctx) }()
		for dl := time.Now().Add(5 * time.Second); time.Now().Before(dl); {
			if e.Addr() != nil {
				run = &engineRun896{e: e, cancel: cancel, done: done}
				return nil
			}
			select {
			case lerr := <-done:
				cancel()
				return lerr
			case <-time.After(time.Millisecond):
			}
		}
		cancel()
		<-done
		t.Fatalf("engine on %s never published an address", addr)
		return nil
	})
	if err != nil {
		if ioUringUnavailable639(err) {
			skipUnlessIOUringUnusable(t, err)
		}
		t.Fatalf("engine start on %s: %v", addr, err)
	}
	defer func() { _, _ = run.stop() }()

	// The seam sees every drain of a handler's write; the test arms it for the
	// one that follows its release of the gate and keeps the first age it sees.
	var armed atomic.Bool
	var age atomic.Int64
	h2WriteDrainSeam = func(w *Worker, _ int) {
		if armed.CompareAndSwap(true, false) {
			age.Store(time.Now().UnixNano() - w.cachedNow)
		}
	}
	t.Cleanup(func() { h2WriteDrainSeam = nil })

	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(60 * time.Second))
	if _, err := c.Write([]byte(http2.ClientPreface)); err != nil {
		t.Fatal(err)
	}
	fr := http2.NewFramer(c, c)
	if err := fr.WriteSettings(); err != nil {
		t.Fatal(err)
	}
	ended := make(chan uint32, 8)
	go func() {
		for {
			f, err := fr.ReadFrame()
			if err != nil {
				return
			}
			if df, ok := f.(*http2.DataFrame); ok && df.StreamEnded() {
				ended <- df.StreamID
			}
		}
	}()

	const n = 60
	ages := make([]time.Duration, 0, n)
	for i := range n {
		id := uint32(2*i + 1)
		var hb bytes.Buffer
		enc := hpack.NewEncoder(&hb)
		for _, f := range [][2]string{{":method", "GET"}, {":scheme", "http"}, {":path", "/gate"}, {":authority", "x"}} {
			_ = enc.WriteField(hpack.HeaderField{Name: f[0], Value: f[1]})
		}
		if err := fr.WriteHeaders(http2.HeadersFrameParam{StreamID: id, BlockFragment: hb.Bytes(), EndStream: true, EndHeaders: true}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-g.started:
		case <-time.After(5 * time.Second):
			t.Fatalf("stream %d: the handler never ran", id)
		}
		// Quiet: the request has been read, and nothing arrives for a gap
		// that differs from one stream to the next, so that the write falls
		// at every phase of the sweep.
		time.Sleep(time.Duration(70+(i*17)%70) * time.Millisecond)
		armed.Store(true)
		g.gate <- struct{}{}
		select {
		case got := <-ended:
			if got != id {
				t.Fatalf("response for stream %d, want %d", got, id)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("stream %d: no response", id)
		}
		if armed.Load() {
			t.Fatalf("stream %d: the response did not go through drainH2Writes", id)
		}
		ages = append(ages, time.Duration(age.Load()))
	}
	slices.Sort(ages)
	median, maxAge := ages[n/2], ages[n-1]
	t.Logf("age of the clock a handler's write is stamped with, after a quiet gap (%d streams, WriteTimeout %v): median %v, max %v", n, writeTimeout1173, median, maxAge)
	// Fresh is microseconds: the loop reads the clock when it wakes, whatever
	// woke it. A stamp half of WriteTimeout old is a write clock that has run
	// half of it before the SEND exists. (The measure includes the scheduler's
	// gap between the loop's read and the seam, microseconds on a worker that
	// runs; 50 ms is not reached by that.)
	if maxAge > writeTimeout1173/2 {
		t.Fatalf("the write clock of a response written after a quiet gap starts %v in the past (median %v) with WriteTimeout %v: "+
			"a sweep that finds the SEND in flight closes the conn mid-stream", maxAge, median, writeTimeout1173)
	}
}
