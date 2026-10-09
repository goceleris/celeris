package celeris

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/goceleris/celeris/internal/protocol/h2/stream"
)

// recordingStreamer835 is a ResponseWriter that is also a Streamer, as every
// engine's is, and records what reached it: whole responses (WriteResponse)
// and streamed calls.
type recordingStreamer835 struct {
	responses []string // "status body"
	streamed  []string // "WriteHeader 200", "Write part", "Flush", "Close"
}

func (r *recordingStreamer835) WriteResponse(_ *stream.Stream, status int, _ [][2]string, body []byte) error {
	r.responses = append(r.responses, itoa835(status)+" "+string(body))
	return nil
}

func (r *recordingStreamer835) WriteHeader(_ *stream.Stream, status int, _ [][2]string) error {
	r.streamed = append(r.streamed, "WriteHeader "+itoa835(status))
	return nil
}

func (r *recordingStreamer835) Write(_ *stream.Stream, data []byte) error {
	r.streamed = append(r.streamed, "Write "+string(data))
	return nil
}

func (r *recordingStreamer835) Flush(*stream.Stream) error {
	r.streamed = append(r.streamed, "Flush")
	return nil
}

func (r *recordingStreamer835) Close(*stream.Stream) error {
	r.streamed = append(r.streamed, "Close")
	return nil
}

var _ stream.Streamer = (*recordingStreamer835)(nil)

func itoa835(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// TestErrorBeforeStreamWriterUseIsAnswered835 is celeris#835 at the router.
// Context.StreamWriter marks the response written the moment the writer is
// taken, so an error (or a panic) the handler returns before it has sent
// anything through the writer was dropped: handleError and handlePanic saw a
// written response and wrote nothing. sse.New takes the writer before running
// OnConnect, so an OnConnect rejection never reached the client (no response at
// all on epoll/io_uring/adaptive HTTP/1, an empty 200 on std and on HTTP/2).
//
// Each case drives HandleStream with a Streamer that records what reached it.
// The last three are the other side of the rule: once the writer has been used,
// or the response was written before the writer was taken, or the connection
// was detached, nothing more may be written.
func TestErrorBeforeStreamWriterUseIsAnswered835(t *testing.T) {
	type want struct {
		responses string // the WriteResponse calls, joined by "|"
		streamed  string // the streamed calls, joined by "|"
	}
	cases := []struct {
		name         string
		errorHandler func(*Context, error)
		handler      HandlerFunc
		want         want
	}{
		{
			name: "error-before-use",
			handler: func(c *Context) error {
				if c.StreamWriter() == nil {
					return errors.New("no StreamWriter")
				}
				return NewHTTPError(403, "denied835")
			},
			want: want{responses: "403 denied835"},
		},
		{
			name: "plain-error-before-use",
			handler: func(c *Context) error {
				_ = c.StreamWriter()
				return errors.New("boom")
			},
			want: want{responses: "500 Internal Server Error"},
		},
		{
			name: "panic-before-use",
			handler: func(c *Context) error {
				_ = c.StreamWriter()
				panic("boom835")
			},
			want: want{responses: "500 Internal Server Error"},
		},
		{
			name: "custom-error-handler-before-use",
			errorHandler: func(c *Context, err error) {
				_ = c.String(418, "custom:%s", err.Error())
			},
			handler: func(c *Context) error {
				_ = c.StreamWriter()
				return errors.New("nope")
			},
			want: want{responses: "418 custom:nope"},
		},
		{
			name: "taken-twice-before-use",
			handler: func(c *Context) error {
				_ = c.StreamWriter()
				_ = c.StreamWriter()
				return NewHTTPError(401, "twice")
			},
			want: want{responses: "401 twice"},
		},
		{
			name: "taken-twice-first-used",
			handler: func(c *Context) error {
				first := c.StreamWriter()
				_ = first.WriteHeader(200, nil)
				_ = c.StreamWriter()
				return NewHTTPError(403, "the first writer already sent its header")
			},
			want: want{streamed: "WriteHeader 200"},
		},
		{
			name: "error-after-write-header",
			handler: func(c *Context) error {
				sw := c.StreamWriter()
				_ = sw.WriteHeader(200, nil)
				return NewHTTPError(403, "too late")
			},
			want: want{streamed: "WriteHeader 200"},
		},
		{
			name: "error-after-stream",
			handler: func(c *Context) error {
				sw := c.StreamWriter()
				_ = sw.WriteHeader(200, nil)
				_, _ = sw.Write([]byte("part"))
				_ = sw.Flush()
				_ = sw.Close()
				return errors.New("after the stream")
			},
			want: want{streamed: "WriteHeader 200|Write part|Flush|Close"},
		},
		{
			name: "error-after-flush-only",
			handler: func(c *Context) error {
				sw := c.StreamWriter()
				_ = sw.Flush()
				return errors.New("after a flush")
			},
			want: want{streamed: "Flush"},
		},
		{
			name: "panic-after-write",
			handler: func(c *Context) error {
				sw := c.StreamWriter()
				_, _ = sw.Write([]byte("x"))
				panic("late panic")
			},
			want: want{streamed: "Write x"},
		},
		{
			name: "written-before-taken",
			handler: func(c *Context) error {
				_ = c.String(200, "whole")
				_ = c.StreamWriter()
				return errors.New("after a whole response")
			},
			want: want{responses: "200 whole"},
		},
		{
			name: "detached-before-use",
			handler: func(c *Context) error {
				_ = c.StreamWriter()
				done := c.Detach()
				defer done()
				return NewHTTPError(403, "detached")
			},
			want: want{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(Config{})
			s.GET("/x", tc.handler)
			adapter := &routerAdapter{server: s, errorHandler: tc.errorHandler}
			st := stream.NewStream(1)
			defer st.Release()
			st.Headers = [][2]string{{":method", "GET"}, {":path", "/x"}, {":scheme", "http"}, {":authority", "localhost"}}
			rw := &recordingStreamer835{}
			st.ResponseWriter = rw
			if err := adapter.HandleStream(context.Background(), st); err != nil {
				t.Fatalf("HandleStream: %v", err)
			}
			got := want{responses: strings.Join(rw.responses, "|"), streamed: strings.Join(rw.streamed, "|")}
			if got != tc.want {
				t.Errorf("%s: responses %q, streamed %q; want responses %q, streamed %q",
					tc.name, got.responses, got.streamed, tc.want.responses, tc.want.streamed)
			}
		})
	}
}

// TestKeepAliveContextForgetsStreamWriter835: the StreamWriter state of one
// request must not reach the next request served by the same Context, as on
// an HTTP/1.1 keep-alive connection (the Context is cached on the stream).
// StreamWriter did not mark the Context extended, and reset clears its writer
// only on the extended path, so a writer used without Detach stayed on the
// Context: the next request's BytesWritten reported the old stream's bytes,
// and with the celeris#835 state carried over, an error before its own
// writer's first use was dropped again.
func TestKeepAliveContextForgetsStreamWriter835(t *testing.T) {
	var bytesAtStart []int
	s := New(Config{})
	s.GET("/stream", func(c *Context) error {
		bytesAtStart = append(bytesAtStart, c.BytesWritten())
		sw := c.StreamWriter()
		_ = sw.WriteHeader(200, nil)
		_, _ = sw.Write([]byte("12345"))
		return sw.Close()
	})
	s.GET("/reject", func(c *Context) error {
		bytesAtStart = append(bytesAtStart, c.BytesWritten())
		_ = c.StreamWriter()
		return NewHTTPError(403, "denied835")
	})
	adapter := &routerAdapter{server: s}
	st := stream.NewStream(1)
	defer st.Release()
	st.CachedCtx = acquireContext(st) // as on an H1 stream: one Context per connection
	releaseContext(st.CachedCtx.(*Context))
	var responses []string
	for _, path := range []string{"/stream", "/reject"} {
		st.Headers = [][2]string{{":method", "GET"}, {":path", path}, {":scheme", "http"}, {":authority", "localhost"}}
		rw := &recordingStreamer835{}
		st.ResponseWriter = rw
		if err := adapter.HandleStream(context.Background(), st); err != nil {
			t.Fatalf("%s: HandleStream: %v", path, err)
		}
		responses = append(responses, path+":"+strings.Join(rw.responses, "|"))
	}
	if len(bytesAtStart) != 2 || bytesAtStart[0] != 0 || bytesAtStart[1] != 0 {
		t.Errorf("BytesWritten at the start of each request = %v, want [0 0] (the first request's stream leaked into the second)", bytesAtStart)
	}
	if want := []string{"/stream:", "/reject:403 denied835"}; strings.Join(responses, " ") != strings.Join(want, " ") {
		t.Errorf("responses %q, want %q", responses, want)
	}
}
