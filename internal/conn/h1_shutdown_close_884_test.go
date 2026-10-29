package conn

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/goceleris/celeris/internal/protocol/h2/stream"
)

// The contract of celeris#884 / celeris#885: once the engine's shutdown flag is
// up (H1State.Shutdown), every HTTP/1 response carries exactly one
// Connection: close and ProcessH1 ends the connection after it
// (errConnectionClose), whatever the response is and whenever the flag went up
// relative to the request: before it, or while the handler was running.

type funcHandler884 func(context.Context, *stream.Stream) error

func (f funcHandler884) HandleStream(ctx context.Context, s *stream.Stream) error { return f(ctx, s) }

// sink884 collects what the engine's write hook is given.
type sink884 struct{ buf bytes.Buffer }

func (k *sink884) write(p []byte) { k.buf.Write(p) }

// heads884 returns the header blocks of the responses in out, in order (each up
// to and including its blank line), skipping bodies by Content-Length.
func heads884(t *testing.T, out string) []string {
	t.Helper()
	var heads []string
	for len(out) > 0 {
		i := strings.Index(out, "\r\n\r\n")
		if i < 0 {
			t.Fatalf("response without a blank line: %q", out)
		}
		head := out[:i+4]
		heads = append(heads, head)
		out = out[i+4:]
		low := strings.ToLower(head)
		if j := strings.Index(low, "\r\ncontent-length: "); j >= 0 {
			rest := low[j+len("\r\ncontent-length: "):]
			n := 0
			for _, c := range rest {
				if c < '0' || c > '9' {
					break
				}
				n = n*10 + int(c-'0')
			}
			// HEAD, 204 and 304 carry no body whatever they declare: the
			// callers of this helper pass only what they know has one.
			if !strings.HasPrefix(head, "HTTP/1.1 304") && !strings.HasPrefix(head, "HTTP/1.1 204") && !headOnly884 {
				if n > len(out) {
					n = len(out)
				}
				out = out[n:]
			}
		} else if strings.Contains(low, "transfer-encoding: chunked") {
			// A chunked body ends at the last chunk.
			if k := strings.Index(out, "0\r\n\r\n"); k >= 0 {
				out = out[k+5:]
			} else {
				out = ""
			}
		}
	}
	return heads
}

// headOnly884 is set by the tests that answer HEAD requests.
var headOnly884 bool

// connLines884 returns the Connection header lines of a head, any case.
func connLines884(head string) []string {
	var lines []string
	for _, l := range strings.Split(head, "\r\n") {
		if len(l) >= 11 && strings.EqualFold(l[:11], "connection:") {
			lines = append(lines, l)
		}
	}
	return lines
}

func wantOneClose884(t *testing.T, name, head string) {
	t.Helper()
	lines := connLines884(head)
	if len(lines) != 1 || !strings.EqualFold(strings.TrimSpace(lines[0][len("connection:"):]), "close") {
		t.Errorf("%s: Connection lines %q in\n%s\nwant exactly one \"connection: close\"", name, lines, head)
	}
}

func process884(st *H1State, h stream.Handler, k *sink884, reqs ...string) error {
	var err error
	for _, r := range reqs {
		if err = ProcessH1(context.Background(), []byte(r), st, h, k.write); err != nil {
			return err
		}
	}
	return err
}

// TestShutdownFlagAnswersCloseOnEveryShape884 pins the response shapes: the
// fused 200 fast path, the general path, HEAD, 204, 304, a chunked stream. Each
// is asked with the flag up before the request.
func TestShutdownFlagAnswersCloseOnEveryShape884(t *testing.T) {
	body := []byte("hello")
	shapes := []struct {
		name string
		req  string
		head bool
		h    funcHandler884
	}{
		{"fused-200", "GET /x HTTP/1.1\r\nHost: a\r\n\r\n", false, func(_ context.Context, s *stream.Stream) error {
			return s.ResponseWriter.WriteResponse(s, 200, [][2]string{{"content-type", "text/plain"}, {"content-length", "5"}}, body)
		}},
		{"general-200", "GET /x HTTP/1.1\r\nHost: a\r\n\r\n", false, func(_ context.Context, s *stream.Stream) error {
			return s.ResponseWriter.WriteResponse(s, 200, [][2]string{{"content-type", "text/plain"}, {"x-a", "1"}}, body)
		}},
		{"404-empty", "GET /x HTTP/1.1\r\nHost: a\r\n\r\n", false, func(_ context.Context, s *stream.Stream) error {
			return s.ResponseWriter.WriteResponse(s, 404, nil, nil)
		}},
		{"head", "HEAD /x HTTP/1.1\r\nHost: a\r\n\r\n", true, func(_ context.Context, s *stream.Stream) error {
			return s.ResponseWriter.WriteResponse(s, 200, [][2]string{{"content-type", "text/plain"}}, body)
		}},
		{"204", "GET /x HTTP/1.1\r\nHost: a\r\n\r\n", false, func(_ context.Context, s *stream.Stream) error {
			return s.ResponseWriter.WriteResponse(s, 204, nil, nil)
		}},
		{"304", "GET /x HTTP/1.1\r\nHost: a\r\n\r\n", false, func(_ context.Context, s *stream.Stream) error {
			return s.ResponseWriter.WriteResponse(s, 304, [][2]string{{"etag", "\"v\""}}, nil)
		}},
		{"chunked-stream", "GET /x HTTP/1.1\r\nHost: a\r\n\r\n", false, func(_ context.Context, s *stream.Stream) error {
			sw := s.ResponseWriter.(stream.Streamer)
			if err := sw.WriteHeader(s, 200, [][2]string{{"content-type", "text/plain"}}); err != nil {
				return err
			}
			if err := sw.Write(s, body); err != nil {
				return err
			}
			return sw.Close(s)
		}},
	}
	for _, sh := range shapes {
		t.Run(sh.name, func(t *testing.T) {
			var flag atomic.Bool
			flag.Store(true)
			st := NewH1State()
			st.Shutdown = &flag
			var k sink884
			err := process884(st, sh.h, &k, sh.req)
			if !errors.Is(err, errConnectionClose) {
				t.Errorf("ProcessH1 returned %v, want errConnectionClose: the connection stayed open after a response written past the start of the shutdown", err)
			}
			headOnly884 = sh.head
			defer func() { headOnly884 = false }()
			heads := heads884(t, k.buf.String())
			if len(heads) != 1 {
				t.Fatalf("%d responses in %q, want 1", len(heads), k.buf.String())
			}
			wantOneClose884(t, sh.name, heads[0])
		})
	}
}

// TestShutdownFlagWithoutShutdownChangesNothing884 is the control: a flag that
// is wired and down leaves the response and the connection as they were.
func TestShutdownFlagWithoutShutdownChangesNothing884(t *testing.T) {
	var flag atomic.Bool
	st := NewH1State()
	st.Shutdown = &flag
	h := funcHandler884(func(_ context.Context, s *stream.Stream) error {
		return s.ResponseWriter.WriteResponse(s, 200, [][2]string{{"content-type", "text/plain"}}, []byte("ok"))
	})
	var k sink884
	for range 3 {
		if err := process884(st, h, &k, "GET /x HTTP/1.1\r\nHost: a\r\n\r\n"); err != nil {
			t.Fatalf("a request with no shutdown: %v", err)
		}
	}
	for _, head := range heads884(t, k.buf.String()) {
		if l := connLines884(head); len(l) != 0 {
			t.Errorf("a response with no shutdown carries %q", l)
		}
	}
}

// TestShutdownFlagRaisedInsideHandler884: the flag goes up while the handler
// runs (a busy conn when Shutdown begins: a handler blocked on the loop thread,
// or an async-route handler on its goroutine). The response is written after
// it, so it carries Connection: close and the conn ends, though the request was
// read as keep-alive.
func TestShutdownFlagRaisedInsideHandler884(t *testing.T) {
	var flag atomic.Bool
	st := NewH1State()
	st.Shutdown = &flag
	h := funcHandler884(func(_ context.Context, s *stream.Stream) error {
		flag.Store(true) // Shutdown begins while the handler runs
		return s.ResponseWriter.WriteResponse(s, 200, [][2]string{{"content-type", "text/plain"}}, []byte("ok"))
	})
	var k sink884
	err := process884(st, h, &k, "GET /x HTTP/1.1\r\nHost: a\r\n\r\n")
	if !errors.Is(err, errConnectionClose) {
		t.Errorf("ProcessH1 returned %v, want errConnectionClose", err)
	}
	heads := heads884(t, k.buf.String())
	if len(heads) != 1 {
		t.Fatalf("%d responses, want 1", len(heads))
	}
	wantOneClose884(t, "flag raised inside the handler", heads[0])
}

// TestShutdownFlagStreamBegunBeforeIsNotRewritten884: a streamed response whose
// head went out before the flag is not rewritten (its head says keep-alive, and
// cannot be taken back); the chunks go on and the last chunk ends it. This is
// the documented limit: only a head written after the start carries the header.
func TestShutdownFlagStreamBegunBeforeIsNotRewritten884(t *testing.T) {
	var flag atomic.Bool
	st := NewH1State()
	st.Shutdown = &flag
	h := funcHandler884(func(_ context.Context, s *stream.Stream) error {
		sw := s.ResponseWriter.(stream.Streamer)
		if err := sw.WriteHeader(s, 200, [][2]string{{"content-type", "text/plain"}}); err != nil {
			return err
		}
		flag.Store(true)
		if err := sw.Write(s, []byte("late chunk")); err != nil {
			return err
		}
		return sw.Close(s)
	})
	var k sink884
	_ = process884(st, h, &k, "GET /x HTTP/1.1\r\nHost: a\r\n\r\n")
	out := k.buf.String()
	if !strings.Contains(out, "late chunk") || !strings.HasSuffix(out, "0\r\n\r\n") {
		t.Errorf("the stream begun before the shutdown was cut: %q", out)
	}
	if strings.Contains(strings.ToLower(out), "connection:") {
		t.Errorf("a head written before the shutdown was rewritten: %q", out)
	}
}

// TestShutdownFlagInterimHeadHasNoConnection884: a 1xx head written after the
// flag is not a final response: it carries no Connection header; the final one does.
func TestShutdownFlagInterimHeadHasNoConnection884(t *testing.T) {
	var flag atomic.Bool
	flag.Store(true)
	st := NewH1State()
	st.Shutdown = &flag
	h := funcHandler884(func(_ context.Context, s *stream.Stream) error {
		sw := s.ResponseWriter.(stream.Streamer)
		if err := sw.WriteHeader(s, 103, [][2]string{{"link", "</a.css>; rel=preload"}}); err != nil {
			return err
		}
		if err := sw.WriteHeader(s, 200, [][2]string{{"content-type", "text/plain"}}); err != nil {
			return err
		}
		if err := sw.Write(s, []byte("ok")); err != nil {
			return err
		}
		return sw.Close(s)
	})
	var k sink884
	_ = process884(st, h, &k, "GET /x HTTP/1.1\r\nHost: a\r\n\r\n")
	heads := heads884(t, k.buf.String())
	if len(heads) != 2 {
		t.Fatalf("%d heads in %q, want the 103 and the 200", len(heads), k.buf.String())
	}
	if l := connLines884(heads[0]); len(l) != 0 {
		t.Errorf("the 103 carries %q", l)
	}
	wantOneClose884(t, "final response after a 103", heads[1])
}

// TestShutdownFlagHandlerConnectionHeader884: a handler that set Connection
// itself does not get a second line (close), and one that said keep-alive is
// overruled by the one close the response carries, as net/http's response
// written after Shutdown began replaces the handler's value.
func TestShutdownFlagHandlerConnectionHeader884(t *testing.T) {
	for _, tc := range []struct {
		name  string
		hdrs  [][2]string
		body  []byte
		parts string
	}{
		{"close-general", [][2]string{{"content-type", "text/plain"}, {"connection", "close"}}, []byte("ok"), "general"},
		{"keep-alive-general", [][2]string{{"content-type", "text/plain"}, {"connection", "keep-alive"}}, []byte("ok"), "general"},
		{"close-first", [][2]string{{"connection", "close"}, {"content-type", "text/plain"}}, []byte("ok"), "general"},
		{"upper-case-key", [][2]string{{"Connection", "Close"}, {"content-type", "text/plain"}}, []byte("ok"), "general"},
		{"close-nobody", [][2]string{{"connection", "close"}}, nil, "nobody"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, flagged := range []bool{true, false} {
				var flag atomic.Bool
				flag.Store(flagged)
				st := NewH1State()
				st.Shutdown = &flag
				h := funcHandler884(func(_ context.Context, s *stream.Stream) error {
					return s.ResponseWriter.WriteResponse(s, 200, tc.hdrs, tc.body)
				})
				var k sink884
				_ = process884(st, h, &k, "GET /x HTTP/1.1\r\nHost: a\r\n\r\n")
				heads := heads884(t, k.buf.String())
				if len(heads) != 1 {
					t.Fatalf("%d responses", len(heads))
				}
				if flagged {
					wantOneClose884(t, tc.name, heads[0])
				}
			}
		})
	}
}

// TestShutdownFlagPipelinedRequestsAreNotServed884: two requests in one read:
// the first is answered close and the second is dropped unparsed, as net/http
// stops serving a conn after the response that closes it. Its bytes were read
// by the engine and are discarded with the conn.
func TestShutdownFlagPipelinedRequestsAreNotServed884(t *testing.T) {
	var flag atomic.Bool
	flag.Store(true)
	st := NewH1State()
	st.Shutdown = &flag
	served := 0
	h := funcHandler884(func(_ context.Context, s *stream.Stream) error {
		served++
		return s.ResponseWriter.WriteResponse(s, 200, [][2]string{{"content-type", "text/plain"}}, []byte("ok"))
	})
	var k sink884
	err := process884(st, h, &k, "GET /first HTTP/1.1\r\nHost: a\r\n\r\nGET /second HTTP/1.1\r\nHost: a\r\n\r\n")
	if !errors.Is(err, errConnectionClose) {
		t.Errorf("ProcessH1 returned %v, want errConnectionClose", err)
	}
	if served != 1 {
		t.Errorf("handler ran %d times, want once: a pipelined request was served after the response that closes the conn", served)
	}
	if heads := heads884(t, k.buf.String()); len(heads) != 1 {
		t.Errorf("%d responses, want 1", len(heads))
	}
}

// TestShutdownFlagRequestBodyInTwoReads884: a request whose body arrives in two
// reads, the flag raised between them, is answered close (the body path
// dispatches through handleH1Request too).
func TestShutdownFlagRequestBodyInTwoReads884(t *testing.T) {
	var flag atomic.Bool
	st := NewH1State()
	st.Shutdown = &flag
	h := funcHandler884(func(_ context.Context, s *stream.Stream) error {
		return s.ResponseWriter.WriteResponse(s, 200, [][2]string{{"content-type", "text/plain"}}, []byte("ok"))
	})
	var k sink884
	if err := process884(st, h, &k, "POST /up HTTP/1.1\r\nHost: a\r\nContent-Length: 10\r\n\r\n01234"); err != nil {
		t.Fatalf("first read: %v", err)
	}
	flag.Store(true)
	err := process884(st, h, &k, "56789")
	if !errors.Is(err, errConnectionClose) {
		t.Errorf("second read returned %v, want errConnectionClose", err)
	}
	heads := heads884(t, k.buf.String())
	if len(heads) != 1 {
		t.Fatalf("%d responses, want 1", len(heads))
	}
	wantOneClose884(t, "body in two reads", heads[0])
}

// TestShutdownFlagAndCloseAfterAreOneDecision884: the per-conn mark the engines'
// gate sets (celeris#885 item 49, #1104, #1138) still closes with no flag
// wired, and the two together do not double the header.
func TestShutdownFlagAndCloseAfterAreOneDecision884(t *testing.T) {
	h := funcHandler884(func(_ context.Context, s *stream.Stream) error {
		return s.ResponseWriter.WriteResponse(s, 200, [][2]string{{"content-type", "text/plain"}}, []byte("ok"))
	})
	for _, tc := range []struct {
		name       string
		closeAfter bool
		flag       bool
	}{{"gate-mark-only", true, false}, {"both", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			st := NewH1State()
			st.CloseAfter = tc.closeAfter
			if tc.flag {
				var f atomic.Bool
				f.Store(true)
				st.Shutdown = &f
			}
			var k sink884
			err := process884(st, h, &k, "GET /x HTTP/1.1\r\nHost: a\r\n\r\n")
			if !errors.Is(err, errConnectionClose) {
				t.Errorf("returned %v, want errConnectionClose", err)
			}
			heads := heads884(t, k.buf.String())
			if len(heads) != 1 {
				t.Fatalf("%d responses", len(heads))
			}
			wantOneClose884(t, tc.name, heads[0])
		})
	}
}

// TestShutdownFlagHTTP10884 pins what an HTTP/1.0 request gets: see the
// comment in the body for what net/http does (measured by the std arm of
// TestShutdownConnCloseContract884 in test/server).
func TestShutdownFlagHTTP10884(t *testing.T) {
	var flag atomic.Bool
	flag.Store(true)
	st := NewH1State()
	st.Shutdown = &flag
	h := funcHandler884(func(_ context.Context, s *stream.Stream) error {
		return s.ResponseWriter.WriteResponse(s, 200, [][2]string{{"content-type", "text/plain"}}, []byte("ok"))
	})
	for _, req := range []string{
		"GET /x HTTP/1.0\r\nHost: a\r\n\r\n",
		"GET /x HTTP/1.0\r\nHost: a\r\nConnection: keep-alive\r\n\r\n",
		"GET /x HTTP/1.1\r\nHost: a\r\nConnection: keep-alive\r\n\r\n",
	} {
		st := NewH1State()
		st.Shutdown = &flag
		var k sink884
		err := process884(st, h, &k, req)
		if !errors.Is(err, errConnectionClose) {
			t.Errorf("%q: returned %v, want errConnectionClose", req, err)
		}
		heads := heads884(t, k.buf.String())
		if len(heads) != 1 {
			t.Fatalf("%q: %d responses", req, len(heads))
		}
		wantOneClose884(t, req, heads[0])
	}
	_ = st
}

// TestShutdownFlagSendfileHeadCarriesClose884: a file served by sendfile(2)
// builds its head apart from WriteResponse's (appendFileResponseHead), and a
// handler's own Connection header is not repeated there either.
func TestShutdownFlagSendfileHeadCarriesClose884(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "f")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	size := int64(sendfileThreshold + 1)
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	for _, hdrs := range [][][2]string{
		{{"content-type", "application/octet-stream"}},
		{{"content-type", "application/octet-stream"}, {"connection", "close"}},
	} {
		var flag atomic.Bool
		flag.Store(true)
		st := NewH1State()
		st.Shutdown = &flag
		var sent []byte
		st.SetSendFileFn(func(header []byte, _ *os.File, _, _ int64) error {
			sent = append([]byte(nil), header...)
			return nil
		})
		h := funcHandler884(func(_ context.Context, s *stream.Stream) error {
			handled, err := s.ResponseWriter.(stream.FileResponder).WriteFileResponse(s, 200, hdrs, f, 0, size)
			if err != nil || !handled {
				t.Fatalf("WriteFileResponse: handled %v, %v", handled, err)
			}
			return nil
		})
		var k sink884
		err := process884(st, h, &k, "GET /f HTTP/1.1\r\nHost: a\r\n\r\n")
		if !errors.Is(err, errConnectionClose) {
			t.Errorf("ProcessH1 returned %v, want errConnectionClose", err)
		}
		wantOneClose884(t, "sendfile head", string(sent))
	}
}

// TestShutdownFlagHandshakeIsNotClosed884: a 101 (a WebSocket handshake, which
// goes out through WriteHeader and then detaches the conn) is an interim
// response: the flag adds no Connection: close to it, its own
// "Connection: Upgrade" stays, and it does not end the conn.
func TestShutdownFlagHandshakeIsNotClosed884(t *testing.T) {
	var flag atomic.Bool
	flag.Store(true)
	st := NewH1State()
	st.Shutdown = &flag
	h := funcHandler884(func(_ context.Context, s *stream.Stream) error {
		sw := s.ResponseWriter.(stream.Streamer)
		return sw.WriteHeader(s, 101, [][2]string{{"upgrade", "websocket"}, {"connection", "Upgrade"}})
	})
	var k sink884
	_ = process884(st, h, &k, "GET /ws HTTP/1.1\r\nHost: a\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
	lines := connLines884(k.buf.String())
	if len(lines) != 1 || !strings.EqualFold(lines[0], "connection: Upgrade") {
		t.Errorf("Connection lines %q in %q, want only the handler's \"Connection: Upgrade\"", lines, k.buf.String())
	}
}

// TestShutdownFlagStreamHeadConnectionHeader884: the streamed head (WriteHeader)
// is built apart from WriteResponse's, and drops a handler's Connection header
// the same way: "keep-alive" is replaced, "close" is not repeated.
func TestShutdownFlagStreamHeadConnectionHeader884(t *testing.T) {
	for _, v := range []string{"keep-alive", "close"} {
		var flag atomic.Bool
		flag.Store(true)
		st := NewH1State()
		st.Shutdown = &flag
		h := funcHandler884(func(_ context.Context, s *stream.Stream) error {
			sw := s.ResponseWriter.(stream.Streamer)
			if err := sw.WriteHeader(s, 200, [][2]string{{"content-type", "text/plain"}, {"connection", v}}); err != nil {
				return err
			}
			if err := sw.Write(s, []byte("ok")); err != nil {
				return err
			}
			return sw.Close(s)
		})
		var k sink884
		_ = process884(st, h, &k, "GET /x HTTP/1.1\r\nHost: a\r\n\r\n")
		heads := heads884(t, k.buf.String())
		if len(heads) != 1 {
			t.Fatalf("handler Connection %q: %d heads", v, len(heads))
		}
		wantOneClose884(t, "streamed head, handler Connection: "+v, heads[0])
	}
}

// TestExpectationFailedSaysClose884: the 417 that OnExpectContinue's refusal
// writes ends the conn (the body bytes may follow), and says so.
func TestExpectationFailedSaysClose884(t *testing.T) {
	st := NewH1State()
	st.OnExpectContinue = func(string, string, [][2]string) bool { return false }
	h := funcHandler884(func(context.Context, *stream.Stream) error { t.Fatal("handler ran"); return nil })
	var k sink884
	err := process884(st, h, &k, "POST /up HTTP/1.1\r\nHost: a\r\nContent-Length: 4\r\nExpect: 100-continue\r\n\r\n")
	if !errors.Is(err, errConnectionClose) {
		t.Errorf("returned %v, want errConnectionClose", err)
	}
	heads := heads884(t, k.buf.String())
	if len(heads) != 1 || !strings.HasPrefix(heads[0], "HTTP/1.1 417") {
		t.Fatalf("answer %q, want a 417", k.buf.String())
	}
	wantOneClose884(t, "417", heads[0])
}
