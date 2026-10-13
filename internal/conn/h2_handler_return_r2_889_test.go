package conn

import (
	"context"
	"errors"
	"strings"
	"testing"

	"golang.org/x/net/http2"

	"github.com/goceleris/celeris/internal/protocol/h2/stream"
)

// celeris#889, review round 2.

// streamedHeaders889 is streamed889 with the headers given whole: a handler
// that writes n bytes and returns without closing.
func streamedHeaders889(hdrs [][2]string, n int) func(context.Context, *stream.Stream) error {
	return func(_ context.Context, s *stream.Stream) error {
		sw := s.ResponseWriter.(stream.Streamer)
		if err := sw.WriteHeader(s, 200, hdrs); err != nil {
			return err
		}
		if n > 0 {
			return sw.Write(s, pattern904(n))
		}
		return nil
	}
}

// A body that is not exactly as long as its content-length is reset, never
// ended, whichever side of the declared length it falls on; and so is a body
// whose content-length cannot be held to (not a plain run of digits, or two
// that differ), since it can be neither short nor whole. A list of identical
// values stands for that one value.
func TestUnclosedWriterAgainstItsContentLength889(t *testing.T) {
	cl := func(v ...string) [][2]string {
		var h [][2]string
		for _, x := range v {
			h = append(h, [2]string{"content-length", x})
		}
		return h
	}
	cases := []struct {
		name  string
		hdrs  [][2]string
		n     int
		reset bool
	}{
		{"over the declared length", cl("3"), 5, true},
		{"exact", cl("5"), 5, false},
		{"identical list", cl("5, 5"), 5, false},
		{"identical list on two lines", cl("5", "5"), 5, false},
		{"differing list", cl("5, 6"), 5, true},
		{"differing lines", cl("5", "6"), 5, true},
		{"plus sign", cl("+5"), 5, true},
		{"not a number", cl("abc"), 5, true},
		{"empty", cl(""), 5, true},
		{"negative", cl("-1"), 5, true},
		{"trailing comma", cl("5,"), 5, true},
		{"too many digits", cl(strings.Repeat("9", 40)), 5, true},
		{"leading zeros", cl("005"), 5, false},
	}
	for _, arm := range arms889() {
		for _, tc := range cases {
			t.Run(arm.name+"/"+tc.name, func(t *testing.T) {
				var sink logBuf889
				c := newH2Return889(t, arm.h(streamedHeaders889(tc.hdrs, tc.n)), logger889(&sink))
				c.req("GET", nil, 1)
				c.settle()
				w := c.stream(1)
				if tc.reset {
					if w.ended || len(w.rst) != 1 || w.rst[0] != http2.ErrCodeInternal {
						t.Errorf("content-length %q, %d bytes: ended=%v rst=%v (%s), want one INTERNAL_ERROR RST_STREAM and no END_STREAM", tc.hdrs, tc.n, w.ended, w.rst, w.summary())
					}
					if out := sink.String(); !strings.Contains(out, "action=rst_stream") {
						t.Errorf("no Warn for the reset:\n%s", out)
					}
					return
				}
				if !w.ended || len(w.rst) != 0 {
					t.Errorf("content-length %q, %d bytes: ended=%v rst=%v (%s), want END_STREAM and no reset", tc.hdrs, tc.n, w.ended, w.rst, w.summary())
				}
			})
		}
	}
}

// The log says why: a content-length that cannot be held to is flagged.
func TestInvalidContentLengthIsLogged889(t *testing.T) {
	var sink logBuf889
	h := &syncHandler893{run: streamedHeaders889([][2]string{{"content-length", "+5"}}, 5)}
	c := newH2Return889(t, h, logger889(&sink))
	c.req("GET", nil, 1)
	c.settle()
	if out := sink.String(); !strings.Contains(out, "content_length_invalid=true") || strings.Contains(out, "content_length=") {
		t.Errorf("want content_length_invalid=true and no content_length:\n%s", out)
	}
}

// A handler that closes its StreamWriter while the tail is still buffered for
// the peer's window (the response is whole: its END_STREAM is queued behind
// that DATA) and then returns an error or panics leaves
// the stream standing until the tail and its END_STREAM are out: they are not
// dropped with the stream, and no RST_STREAM is sent for a whole response.
// (A runtime.Goexit is not a case: it ends the event loop that would send the
// tail. A raw stream.Handler only: the celeris router returns nil and recovers
// panics.)
func TestClosedInlineWriterWithBufferedTailThenFailureKeepsItsTail889(t *testing.T) {
	for _, mode := range []string{"error", "panic"} {
		t.Run(mode, func(t *testing.T) {
			h := &syncHandler893{run: func(_ context.Context, s *stream.Stream) error {
				sw := s.ResponseWriter.(stream.Streamer)
				if err := sw.WriteHeader(s, 200, nil); err != nil {
					return err
				}
				if err := sw.Write(s, pattern904(5000)); err != nil {
					return err
				}
				if err := sw.Close(s); err != nil {
					return err
				}
				switch mode {
				case "panic":
					panic("boom889")
				}
				return errors.New("boom889")
			}}
			c := newH2Return889(t, h, H2Config{})
			done := make(chan struct{})
			go func() {
				defer close(done)
				c.req("GET", []http2.Setting{{ID: http2.SettingInitialWindowSize, Val: 1000}}, 1)
			}()
			<-done
			c.drain()
			m := c.st.processor.GetManager()
			if held := m.OutboundHeld(); held != 4000 {
				t.Fatalf("the connection holds %d buffered bytes, want 4000 (the tail waits for the peer's window)", held)
			}
			if got := m.StreamCount(); got != 1 {
				t.Fatalf("the manager has %d stream(s), want 1: the tail needs its stream", got)
			}
			c.grant(1<<20, true, 1)
			c.drain()
			checkWholeBody(t, "closed writer, then "+mode, c.stream(1), pattern904(5000))
			if w := c.stream(1); len(w.rst) != 0 {
				t.Errorf("a whole response was reset: %v", w.rst)
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
