package conn

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/goceleris/celeris/internal/protocol/h2/stream"
)

// celeris#889 (items 12, 32 and 47): what an HTTP/2 stream's peer sees when
// the handler is over without having finished its response. net/http ends the
// response when a handler returns; the processor does the same: END_STREAM for
// a response that is complete (headers sent and, for a declared
// content-length, that many bytes written; or a response with no declared
// length that simply was not closed), RST_STREAM(INTERNAL_ERROR) for one that
// is not (a body short of its declared length, nothing sent at all, a handler
// that panicked or called runtime.Goexit), and a log record through
// H2Config.Logger. The tests run in memory (NewH2State, ProcessH2), on the
// event loop's path (inline) and on the worker pool.

// h2Return889 drives one connection: the first request opens it.
type h2Return889 struct {
	*flushConn
	opened bool
}

func newH2Return889(t *testing.T, h stream.Handler, cfg H2Config) *h2Return889 {
	t.Helper()
	return &h2Return889{flushConn: newFlushConn(t, h, cfg)}
}

// req sends a request with method on each of ids (END_STREAM on the HEADERS),
// preceded by the preface and SETTINGS the first time.
func (c *h2Return889) req(method string, settings []http2.Setting, ids ...uint32) {
	c.t.Helper()
	var in []byte
	first := !c.opened
	if first {
		c.opened = true
		in = append(in, http2.ClientPreface...)
	}
	in = append(in, frames893(c.t, func(fr *http2.Framer, enc *hpack.Encoder, hb *bytes.Buffer) {
		if first {
			_ = fr.WriteSettings(settings...)
		}
		for _, id := range ids {
			hb.Reset()
			for _, hf := range []hpack.HeaderField{{Name: ":method", Value: method}, {Name: ":scheme", Value: "http"}, {Name: ":authority", Value: "x"}, {Name: ":path", Value: "/b"}} {
				_ = enc.WriteField(hf)
			}
			_ = fr.WriteHeaders(http2.HeadersFrameParam{StreamID: id, BlockFragment: hb.Bytes(), EndStream: true, EndHeaders: true})
		}
	})...)
	c.process(in)
}

// settle waits for the pool handlers to be over and drains the write queue.
func (c *h2Return889) settle() {
	c.t.Helper()
	waitFlush(c.t, "the pool handlers to return", func() bool { return !c.st.processor.PoolHandlersRunning() })
	c.drain()
}

// pathArm889 is one of the two places a stream's handler runs.
type pathArm889 struct {
	name string
	h    func(run func(context.Context, *stream.Stream) error) stream.Handler
}

func arms889() []pathArm889 {
	return []pathArm889{
		{"inline", func(run func(context.Context, *stream.Stream) error) stream.Handler { return &syncHandler893{run: run} }},
		{"pool", func(run func(context.Context, *stream.Stream) error) stream.Handler {
			return &asyncHandler893{run: run}
		}},
	}
}

func logger889(sink *logBuf889) H2Config {
	return H2Config{Logger: slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug}))}
}

// streamed889 is a handler that writes a StreamWriter response and returns
// without closing it: the headers (with contentLength, if >= 0) and n bytes.
func streamed889(contentLength int, n int) func(context.Context, *stream.Stream) error {
	return func(_ context.Context, s *stream.Stream) error {
		sw := s.ResponseWriter.(stream.Streamer)
		var hdrs [][2]string
		if contentLength >= 0 {
			hdrs = [][2]string{{"content-length", fmt.Sprint(contentLength)}}
		}
		if err := sw.WriteHeader(s, 200, hdrs); err != nil {
			return err
		}
		if n > 0 {
			return sw.Write(s, pattern904(n))
		}
		return nil // never closed
	}
}

// (a) A handler that returns without closing its StreamWriter after a
// complete response ends the stream with END_STREAM.
func TestHandlerReturnWithoutCloseEndsTheStream889(t *testing.T) {
	cases := []struct {
		name          string
		contentLength int // < 0: none declared
		n             int
	}{
		{"no content-length, body written", -1, 5},
		{"content-length met", 5, 5},
		{"no content-length, no body", -1, 0},
		{"content-length 0, no body", 0, 0},
	}
	for _, arm := range arms889() {
		for _, tc := range cases {
			t.Run(arm.name+"/"+tc.name, func(t *testing.T) {
				var sink logBuf889
				c := newH2Return889(t, arm.h(streamed889(tc.contentLength, tc.n)), logger889(&sink))
				c.req("GET", nil, 1)
				c.settle()
				w := c.stream(1)
				if w.hdrIdx != 0 {
					t.Fatalf("stream 1: no HEADERS first: %s", w.summary())
				}
				if len(w.data) != tc.n {
					t.Errorf("%d DATA bytes on the wire, want %d (%s)", len(w.data), tc.n, w.summary())
				}
				if !w.ended || w.after != 0 {
					t.Errorf("END_STREAM %v with %d frames after it: the handler returned without closing a complete response, and the client is left waiting (%s)", w.ended, w.after, w.summary())
				}
				if len(w.rst) != 0 {
					t.Errorf("a complete response was reset: %v", w.rst)
				}
				if got := c.st.processor.GetManager().StreamCount(); got != 0 {
					t.Errorf("the manager still has %d stream(s)", got)
				}
				for _, want := range []string{"level=DEBUG", "h2 handler returned before finishing its response", "stream=1", "action=end_stream"} {
					if !strings.Contains(sink.String(), want) {
						t.Errorf("the log lacks %q:\n%s", want, sink.String())
					}
				}
			})
		}
	}
}

// A response with nothing declared and a HEAD request: the body a HEAD's
// handler writes is dropped, so a declared length is never met, and the
// response is still complete.
func TestHandlerReturnWithoutCloseOnHEADEndsTheStream889(t *testing.T) {
	for _, arm := range arms889() {
		t.Run(arm.name, func(t *testing.T) {
			c := newH2Return889(t, arm.h(streamed889(5, 5)), H2Config{})
			c.req("HEAD", nil, 1)
			c.settle()
			w := c.stream(1)
			if !w.ended || len(w.rst) != 0 || len(w.data) != 0 {
				t.Errorf("a HEAD response with a declared content-length and no closed writer: ended=%v rst=%v data=%d (%s), want ended, no RST_STREAM, no body", w.ended, w.rst, len(w.data), w.summary())
			}
		})
	}
}

// (a) The inline handler's buffered rest: it is sent as the peer grants
// window, and END_STREAM rides on the last of it.
func TestUnclosedInlineWriterWithBufferedDataEndsWithItsLastData889(t *testing.T) {
	for _, tc := range []struct {
		name          string
		contentLength int
	}{{"no content-length", -1}, {"content-length met", 5000}} {
		t.Run(tc.name, func(t *testing.T) {
			c := newH2Return889(t, &syncHandler893{run: streamed889(tc.contentLength, 5000)}, H2Config{})
			c.req("GET", []http2.Setting{{ID: http2.SettingInitialWindowSize, Val: 1000}}, 1)
			c.drain()
			m := c.st.processor.GetManager()
			if held := m.OutboundHeld(); held != 4000 {
				t.Fatalf("the connection holds %d buffered bytes, want 4000", held)
			}
			if w := c.stream(1); w.ended {
				t.Fatalf("END_STREAM before the buffered DATA was sent: %s", w.summary())
			}
			c.grant(1<<20, true, 1)
			c.drain()
			checkWholeBody(t, "unclosed writer", c.stream(1), pattern904(5000))
			if w := c.stream(1); len(w.rst) != 0 {
				t.Errorf("a complete response was reset: %v", w.rst)
			}
			if got := m.StreamCount(); got != 0 {
				t.Errorf("the manager still has %d stream(s)", got)
			}
			if held := m.OutboundHeld(); held != 0 {
				t.Errorf("the connection still holds %d buffered bytes", held)
			}
		})
	}
}

// (b) A body short of its declared content-length is reset, never ended: a
// truncated body must not look complete to the client (#965).
func TestHandlerReturnShortOfContentLengthResetsTheStream889(t *testing.T) {
	cases := []struct {
		name          string
		contentLength int
		n             int
	}{
		{"40 of 100", 100, 40},
		{"headers only, 100 declared", 100, 0},
		{"99 of 100", 100, 99},
	}
	for _, arm := range arms889() {
		for _, tc := range cases {
			t.Run(arm.name+"/"+tc.name, func(t *testing.T) {
				var sink logBuf889
				c := newH2Return889(t, arm.h(streamed889(tc.contentLength, tc.n)), logger889(&sink))
				c.req("GET", nil, 1)
				c.settle()
				w := c.stream(1)
				if w.ended {
					t.Errorf("END_STREAM for a body of %d bytes against a declared %d: it looks complete to the client (%s)", tc.n, tc.contentLength, w.summary())
				}
				if len(w.rst) != 1 || w.rst[0] != http2.ErrCodeInternal {
					t.Errorf("RST_STREAM codes %v, want one INTERNAL_ERROR (%s)", w.rst, w.summary())
				}
				if len(w.seq) == 0 || w.seq[len(w.seq)-1] != "RST_STREAM" {
					t.Errorf("the RST_STREAM is not the stream's last frame: %s", w.summary())
				}
				if got := c.st.processor.GetManager().StreamCount(); got != 0 {
					t.Errorf("the manager still has %d stream(s)", got)
				}
				for _, want := range []string{"level=WARN", "h2 handler returned before finishing its response", "stream=1", "action=rst_stream"} {
					if !strings.Contains(sink.String(), want) {
						t.Errorf("the log lacks %q:\n%s", want, sink.String())
					}
				}
			})
		}
	}
}

// (b) The same for the inline handler's buffered DATA: the stream is reset at
// once, what it still buffers is dropped and its share of the outbound budget
// refunded, and nothing follows the RST_STREAM when the peer grants window.
func TestShortInlineWriterWithBufferedDataIsResetAndReleased889(t *testing.T) {
	c := newH2Return889(t, &syncHandler893{run: streamed889(6000, 5000)}, H2Config{})
	c.req("GET", []http2.Setting{{ID: http2.SettingInitialWindowSize, Val: 1000}}, 1)
	c.drain()
	m := c.st.processor.GetManager()
	if held := m.OutboundHeld(); held != 0 {
		t.Errorf("the connection holds %d buffered bytes of a reset stream, want 0", held)
	}
	if got := m.StreamCount(); got != 0 {
		t.Errorf("the manager has %d stream(s), want 0", got)
	}
	c.grant(1<<20, true, 1)
	c.drain()
	w := c.stream(1)
	if w.ended {
		t.Errorf("END_STREAM for a body short of its declared length: %s", w.summary())
	}
	if len(w.rst) != 1 || w.rst[0] != http2.ErrCodeInternal || w.seq[len(w.seq)-1] != "RST_STREAM" {
		t.Errorf("want one INTERNAL_ERROR RST_STREAM as the stream's last frame: %v %s", w.rst, w.summary())
	}
}

// A handler that returns an error with nothing sent leaves the client waiting
// for its own timeout: the stream is reset. An error after a complete response
// changes nothing.
func TestHandlerErrorReturnResetsAnUnansweredStream889(t *testing.T) {
	boom := errors.New("boom")
	for _, arm := range arms889() {
		t.Run(arm.name+"/unanswered", func(t *testing.T) {
			c := newH2Return889(t, arm.h(func(context.Context, *stream.Stream) error { return boom }), H2Config{})
			c.req("GET", nil, 1)
			c.settle()
			w := c.stream(1)
			if w.ended || len(w.rst) != 1 || w.rst[0] != http2.ErrCodeInternal {
				t.Errorf("a handler that returned an error and sent nothing: ended=%v rst=%v (%s), want one INTERNAL_ERROR RST_STREAM", w.ended, w.rst, w.summary())
			}
		})
		t.Run(arm.name+"/answered", func(t *testing.T) {
			c := newH2Return889(t, arm.h(func(_ context.Context, s *stream.Stream) error {
				_ = s.ResponseWriter.WriteResponse(s, 200, nil, []byte("ok"))
				return boom
			}), H2Config{})
			c.req("GET", nil, 1)
			c.settle()
			w := c.stream(1)
			if !w.ended || len(w.rst) != 0 {
				t.Errorf("a handler that answered and then returned an error: ended=%v rst=%v (%s), want the response kept", w.ended, w.rst, w.summary())
			}
		})
	}
}

// (c) runtime.Goexit and a panic out of the handler: the stream is reset, one
// log record says so with the stack, and the connection goes on with its other
// streams. A Goexit on the worker pool.
func TestPoolHandlerGoexitResetsTheStreamAndKeepsTheConnection889(t *testing.T) {
	for _, tc := range []struct {
		name string
		fn   func(context.Context, *stream.Stream) error
	}{
		{"before any write", func(context.Context, *stream.Stream) error { runtime.Goexit(); return nil }},
		{"after headers", func(_ context.Context, s *stream.Stream) error {
			_ = s.ResponseWriter.(stream.Streamer).WriteHeader(s, 200, nil)
			runtime.Goexit()
			return nil
		}},
		{"panic", func(context.Context, *stream.Stream) error { panic("boom889") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sink logBuf889
			var calls atomic.Int32
			h := &asyncHandler893{run: func(ctx context.Context, s *stream.Stream) error {
				if calls.Add(1) == 1 {
					return tc.fn(ctx, s)
				}
				return s.ResponseWriter.WriteResponse(s, 200, nil, []byte("ok"))
			}}
			c := newH2Return889(t, h, logger889(&sink))
			c.req("GET", nil, 1)
			c.settle()
			w := c.stream(1)
			if w.ended || len(w.rst) != 1 || w.rst[0] != http2.ErrCodeInternal {
				t.Errorf("stream 1: ended=%v rst=%v (%s), want one INTERNAL_ERROR RST_STREAM and no END_STREAM", w.ended, w.rst, w.summary())
			}
			out := sink.String()
			if tc.name == "panic" {
				if !strings.Contains(out, "h2 handler panicked") {
					t.Errorf("no panic record:\n%s", out)
				}
			} else {
				if n := strings.Count(out, "level=ERROR"); n != 1 {
					t.Errorf("%d error records, want 1:\n%s", n, out)
				}
				for _, want := range []string{"h2 handler called runtime.Goexit", "stream=1", "TestPoolHandlerGoexit"} {
					if !strings.Contains(out, want) {
						t.Errorf("the log lacks %q:\n%s", want, out)
					}
				}
			}
			// The connection serves its next stream.
			c.req("GET", nil, 3)
			c.settle()
			w3 := c.stream(3)
			if !w3.ended || len(w3.rst) != 0 {
				t.Errorf("stream 3 after the fault: ended=%v rst=%v (%s), want it answered", w3.ended, w3.rst, w3.summary())
			}
		})
	}
}

// (c) A panic and a Goexit in an inline handler. The Goexit ends the goroutine
// that runs the connection (the engine's event loop, which the engine's loop
// report covers, celeris#848), so the test runs the connection on a goroutine
// of its own: the stream must still be reset (the frame is queued; the loop
// that would send it is gone only in this case) and the record written.
func TestInlineHandlerGoexitAndPanicAreReported889(t *testing.T) {
	for _, mode := range []string{"goexit", "panic"} {
		t.Run(mode, func(t *testing.T) {
			var sink logBuf889
			h := &syncHandler893{run: func(_ context.Context, s *stream.Stream) error {
				_ = s.ResponseWriter.(stream.Streamer).WriteHeader(s, 200, nil)
				if mode == "panic" {
					panic("boom889")
				}
				runtime.Goexit()
				return nil
			}}
			c := newH2Return889(t, h, logger889(&sink))
			done := make(chan struct{})
			go func() {
				defer close(done)
				c.req("GET", nil, 1)
			}()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("the connection's goroutine did not end")
			}
			c.drain()
			w := c.stream(1)
			if w.ended || len(w.rst) != 1 || w.rst[0] != http2.ErrCodeInternal {
				t.Errorf("ended=%v rst=%v (%s), want one INTERNAL_ERROR RST_STREAM and no END_STREAM", w.ended, w.rst, w.summary())
			}
			want := "h2 handler called runtime.Goexit"
			if mode == "panic" {
				want = "h2 handler panicked"
			}
			if out := sink.String(); !strings.Contains(out, want) || strings.Count(out, "level=ERROR") != 1 {
				t.Errorf("want exactly one error record %q:\n%s", want, out)
			}
		})
	}
}

// Concurrent pool handlers that return unclosed, some complete and some short
// of their declared length, while the loop grants window and drains the
// write queue: each stream ends with exactly one of END_STREAM (complete) and
// RST_STREAM (short), with nothing after it, and the connection keeps no stream
// and no buffered byte. Run with -race: the settle takes the stream's lock, the
// shard's and the manager's, in the orders the loop's flushes take them
// (celeris#889, RULE 10).
func TestUnfinishedPoolHandlersWhileTheLoopFlushes889(t *testing.T) {
	const streams, size = 40, 3000
	var started atomic.Int32
	h := &asyncHandler893{run: func(ctx context.Context, s *stream.Stream) error {
		started.Add(1)
		cl := size
		if s.ID%4 == 3 { // every other stream is short of what it declares
			cl = size + 1000
		}
		return streamed889(cl, size)(ctx, s)
	}}
	c := newH2Return889(t, h, H2Config{WriteTimeout: 20 * time.Second})
	ids := make([]uint32, 0, streams)
	for i := range streams {
		ids = append(ids, uint32(2*i+1))
	}
	c.req("GET", []http2.Setting{{ID: http2.SettingInitialWindowSize, Val: 1000}}, ids...)
	deadline := time.Now().Add(30 * time.Second)
	for c.st.processor.PoolHandlersRunning() || started.Load() < streams {
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d handlers started and the pool is still running: a deadlock", started.Load(), streams)
		}
		c.drain()
		c.grant(1000, true, ids...)
		time.Sleep(time.Millisecond)
	}
	c.drain()
	for _, id := range ids {
		w := c.stream(id)
		short := id%4 == 3
		switch {
		case short && (w.ended || len(w.rst) != 1 || w.seq[len(w.seq)-1] != "RST_STREAM"):
			t.Errorf("stream %d (short): ended=%v rst=%v %s, want one RST_STREAM as the last frame", id, w.ended, w.rst, w.summary())
		case !short && (!w.ended || w.after != 0 || len(w.rst) != 0 || len(w.data) != size):
			t.Errorf("stream %d (complete): ended=%v after=%d rst=%v data=%d %s, want END_STREAM last with %d bytes", id, w.ended, w.after, w.rst, len(w.data), w.summary(), size)
		}
	}
	m := c.st.processor.GetManager()
	if held := m.OutboundHeld(); held != 0 {
		t.Errorf("the connection holds %d buffered bytes", held)
	}
	c.grant(1<<20, true, ids...)
	c.drain()
	if got := m.StreamCount(); got != 0 {
		t.Errorf("the manager has %d stream(s)", got)
	}
}
