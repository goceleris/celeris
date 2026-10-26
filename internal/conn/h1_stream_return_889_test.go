package conn

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/goceleris/celeris/internal/protocol/h2/stream"
)

// celeris#889: the HTTP/1 streaming response (h1ResponseAdapter as a
// stream.Streamer) at the conn level, on every native engine's path (they all
// run handleH1Request): a handler that returns without Close, a handler broken
// off after its headers, the HEAD-ness of a streamed response, and the cost of
// a chunk.

// streamHandler889 is a handler that streams through the response writer as
// celeris.StreamWriter does.
func streamHandler889(body func(st stream.Streamer, s *stream.Stream) error) stream.Handler {
	return stream.HandlerFunc(func(_ context.Context, s *stream.Stream) error {
		return body(s.ResponseWriter.(stream.Streamer), s)
	})
}

func feed889(t *testing.T, st *H1State, h stream.Handler, in string) (out string, err error) {
	t.Helper()
	var buf bytes.Buffer
	err = ProcessH1(context.Background(), []byte(in), st, h, func(b []byte) { buf.Write(b) })
	return buf.String(), err
}

const unclosed889 = "HTTP/1.1 200 OK\r\n"

func TestH1HandlerReturnWithoutCloseEndsTheChunkedResponse889(t *testing.T) {
	h := streamHandler889(func(st stream.Streamer, s *stream.Stream) error {
		if err := st.WriteHeader(s, 200, [][2]string{{"content-type", "text/plain"}}); err != nil {
			return err
		}
		return st.Write(s, []byte("hello"))
	})
	state := NewH1State()
	out, err := feed889(t, state, h, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
	if err != nil {
		t.Fatalf("ProcessH1: %v", err)
	}
	if !strings.HasSuffix(out, "5\r\nhello\r\n0\r\n\r\n") {
		t.Errorf("the response of a handler that returned without Close does not end with the last chunk: %q", out)
	}
	// the next request on the connection starts a response of its own, and the
	// unclosed one does not leak into it (the flag is per response)
	state2 := NewH1State()
	in := "GET / HTTP/1.1\r\nHost: x\r\n\r\nGET / HTTP/1.1\r\nHost: x\r\n\r\n"
	out, err = feed889(t, state2, h, in)
	if err != nil {
		t.Fatalf("ProcessH1 pipelined: %v", err)
	}
	if n := strings.Count(out, "5\r\nhello\r\n0\r\n\r\n"); n != 2 || strings.Count(out, unclosed889) != 2 {
		t.Errorf("two pipelined unclosed streams: %q", out)
	}
	// a closed writer is not ended twice
	h2 := streamHandler889(func(st stream.Streamer, s *stream.Stream) error {
		_ = st.WriteHeader(s, 200, nil)
		_ = st.Write(s, []byte("hello"))
		return st.Close(s)
	})
	out, _ = feed889(t, NewH1State(), h2, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
	if n := strings.Count(out, "0\r\n\r\n"); n != 1 {
		t.Errorf("a closed writer's response has %d last chunks, want 1: %q", n, out)
	}
	// a response that is not a stream's is not touched
	h3 := stream.HandlerFunc(func(_ context.Context, s *stream.Stream) error {
		return s.ResponseWriter.WriteResponse(s, 200, [][2]string{{"content-length", "2"}}, []byte("ok"))
	})
	out, _ = feed889(t, NewH1State(), h3, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
	if strings.Contains(out, "0\r\n\r\n") || !strings.HasSuffix(out, "\r\n\r\nok") {
		t.Errorf("a whole response got a last chunk: %q", out)
	}
}

func TestH1HandlerReturnWithoutCloseConnectionClose889(t *testing.T) {
	h := streamHandler889(func(st stream.Streamer, s *stream.Stream) error {
		_ = st.WriteHeader(s, 200, nil)
		return st.Write(s, []byte("hello"))
	})
	for _, req := range []string{
		"GET / HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n",
		"GET / HTTP/1.0\r\nHost: x\r\n\r\n",
	} {
		out, err := feed889(t, NewH1State(), h, req)
		if !errors.Is(err, errConnectionClose) {
			t.Errorf("%q: err %v, want errConnectionClose", req, err)
		}
		if !strings.HasSuffix(out, "5\r\nhello\r\n0\r\n\r\n") {
			t.Errorf("%q: the response does not end with the last chunk: %q", req, out)
		}
	}
}

// A response broken off after its headers is not ended: the last chunk would
// make a truncated body look whole. The connection is closed instead, the
// HTTP/1.1 form of RST_STREAM, and no 500 is appended to the chunked body.
func TestH1HandlerBrokenOffAfterHeadersIsNotEnded889(t *testing.T) {
	for name, h := range map[string]stream.Handler{
		"failed, returns nil (the router's recovered panic)": streamHandler889(func(st stream.Streamer, s *stream.Stream) error {
			_ = st.WriteHeader(s, 200, nil)
			_ = st.Write(s, []byte("hello"))
			// what the router's recovered panic and its swallowed error do
			// (celeris failResponse): the stream's mark, and the writer's
			s.FailResponse()
			if f, ok := s.ResponseWriter.(interface{ FailResponse() }); ok {
				f.FailResponse()
			}
			return nil
		}),
		"returns an error": streamHandler889(func(st stream.Streamer, s *stream.Stream) error {
			_ = st.WriteHeader(s, 200, nil)
			_ = st.Write(s, []byte("hello"))
			return errors.New("boom")
		}),
	} {
		t.Run(name, func(t *testing.T) {
			out, err := feed889(t, NewH1State(), h, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
			if err == nil {
				t.Error("ProcessH1 returned nil: the connection must be closed")
			}
			if strings.Contains(out, "0\r\n\r\n") {
				t.Errorf("a broken-off response was ended with the last chunk: %q", out)
			}
			if strings.Contains(out, "500") {
				t.Errorf("a 500 was written into the chunked body: %q", out)
			}
			if !strings.HasSuffix(out, "5\r\nhello\r\n") {
				t.Errorf("what was sent is cut: %q", out)
			}
		})
	}
	// An error before anything was sent is still answered with the 500.
	h := streamHandler889(func(st stream.Streamer, s *stream.Stream) error { return errors.New("boom") })
	out, err := feed889(t, NewH1State(), h, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
	if err == nil || !strings.HasPrefix(out, "HTTP/1.1 500") {
		t.Errorf("an error before any response: out %q err %v, want the 500", out, err)
	}
}

// HEAD: headers only, no body and no last chunk, closed or not, and the request
// behind it on the connection is answered from its own bytes.
func TestH1HeadStreamedResponse889(t *testing.T) {
	for _, closeIt := range []bool{false, true} {
		h := streamHandler889(func(st stream.Streamer, s *stream.Stream) error {
			if s.Method == "GET" {
				return s.ResponseWriter.WriteResponse(s, 200, [][2]string{{"content-length", "2"}}, []byte("ok"))
			}
			_ = st.WriteHeader(s, 200, nil)
			_ = st.Write(s, []byte("hello"))
			if closeIt {
				return st.Close(s)
			}
			return nil
		})
		in := "HEAD / HTTP/1.1\r\nHost: x\r\n\r\nGET / HTTP/1.1\r\nHost: x\r\n\r\n"
		out, err := feed889(t, NewH1State(), h, in)
		if err != nil {
			t.Fatalf("close=%v: %v", closeIt, err)
		}
		head, rest, _ := strings.Cut(out, "\r\n\r\n")
		if !strings.HasPrefix(head, "HTTP/1.1 200 OK") || !strings.HasPrefix(rest, "HTTP/1.1 200 OK") || !strings.HasSuffix(rest, "\r\n\r\nok") {
			t.Errorf("close=%v: want the HEAD's headers and then the GET's response, got %q", closeIt, out)
		}
		if strings.Contains(rest[:strings.Index(rest, "\r\n\r\n")], "0\r\n") || strings.Count(out, "hello") != 0 || strings.Count(out, "0\r\n\r\n") != 0 {
			t.Errorf("close=%v: bytes of a body or a last chunk on a HEAD response: %q", closeIt, out)
		}
	}
}

// Whether a streamed response is a HEAD's is decided when its headers are
// written, not by whatever request the connection's adapter holds when a later
// chunk arrives (the adapter is per connection and handleH1Request rewrites it
// for every request): a live stream does not depend on per-request adapter
// state (celeris#889 items 2 and 3).
func TestH1StreamedResponseHeadIsFixedAtWriteHeader889(t *testing.T) {
	for _, headFirst := range []bool{true, false} {
		var out bytes.Buffer
		a := &h1ResponseAdapter{write: func(b []byte) { out.Write(b) }, keepAlive: true, isHEAD: headFirst}
		if err := a.WriteHeader(nil, 200, nil); err != nil {
			t.Fatal(err)
		}
		head := out.Len()
		a.isHEAD = !headFirst // the connection's next request, rewritten under a live stream
		_ = a.Write(nil, []byte("hello"))
		_ = a.Close(nil)
		sent := out.String()[head:]
		if headFirst && sent != "" {
			t.Errorf("a HEAD response sent %q after its headers because the adapter's request changed", sent)
		}
		if !headFirst && sent != "5\r\nhello\r\n0\r\n\r\n" {
			t.Errorf("a GET response sent %q after its headers, want its chunk and last chunk, because the adapter's request changed", sent)
		}
	}
}

// A chunk costs no allocation: the framing is built in the connection's scratch
// buffer, as WriteHeader builds its head in respBuf. Every H1 write hook copies
// its argument before it returns (the engines append it to their send buffer),
// which is what lets the scratch be reused (celeris#889 item 54).
func TestH1StreamWriteAllocs889(t *testing.T) {
	var buf []byte
	a := &h1ResponseAdapter{write: func(p []byte) { buf = append(buf, p...) }, keepAlive: true}
	_ = a.WriteHeader(nil, 200, nil)
	for _, n := range []int{1, 20, 1024, 8000} {
		chunk := bytes.Repeat([]byte("x"), n)
		allocs := testing.AllocsPerRun(200, func() {
			buf = buf[:0]
			_ = a.Write(nil, chunk)
		})
		if allocs != 0 {
			t.Errorf("Write of %d bytes: %v allocs/op, want 0", n, allocs)
		}
		if want := strings.ToLower(strings.TrimLeft(intHex889(n), "0")) + "\r\n" + strings.Repeat("x", n) + "\r\n"; string(buf) != want {
			t.Errorf("Write of %d bytes framed as %.40q", n, buf)
		}
	}
}

func intHex889(n int) string {
	const digits = "0123456789abcdef"
	if n == 0 {
		return "0"
	}
	var s []byte
	for ; n > 0; n /= 16 {
		s = append([]byte{digits[n%16]}, s...)
	}
	return string(s)
}

// A status with no content (1xx, 204, 304) has no last chunk to add: the bytes
// would be read as the start of the next response on the connection.
func TestH1UnclosedStreamWithoutContentStatus889(t *testing.T) {
	for _, status := range []int{204, 304} {
		h := streamHandler889(func(st stream.Streamer, s *stream.Stream) error {
			if s.Path == "/ok" {
				return s.ResponseWriter.WriteResponse(s, 200, [][2]string{{"content-length", "2"}}, []byte("ok"))
			}
			return st.WriteHeader(s, status, nil)
		})
		in := "GET /s HTTP/1.1\r\nHost: x\r\n\r\nGET /ok HTTP/1.1\r\nHost: x\r\n\r\n"
		out, err := feed889(t, NewH1State(), h, in)
		if err != nil {
			t.Fatalf("%d: %v", status, err)
		}
		if strings.Contains(out, "0\r\n\r\n") {
			t.Errorf("%d: a last chunk was added to a response that has no content: %q", status, out)
		}
		if !strings.HasSuffix(out, "\r\n\r\nok") {
			t.Errorf("%d: the response behind it is not intact: %q", status, out)
		}
	}
}

// An empty Write is no chunk: a zero-length chunk is the last chunk, which
// would end the response in the middle of its stream (net/http ignores empty
// writes the same way).
func TestH1EmptyStreamWriteIsNotTheLastChunk889(t *testing.T) {
	var out bytes.Buffer
	a := &h1ResponseAdapter{write: func(b []byte) { out.Write(b) }, keepAlive: true}
	_ = a.WriteHeader(nil, 200, nil)
	head := out.Len()
	_ = a.Write(nil, nil)
	_ = a.Write(nil, []byte{})
	_ = a.Write(nil, []byte("hi"))
	_ = a.Close(nil)
	if got := out.String()[head:]; got != "2\r\nhi\r\n0\r\n\r\n" {
		t.Errorf("empty writes put %q on the wire, want only the chunk \"hi\" and the last chunk", got)
	}
}

// A declared content-length is held to when the handler returns without Close:
// the response is ended only if the body is exactly that long (the HTTP/2
// processor's rule, celeris#965); otherwise the connection is closed without
// the last chunk. A content-length that cannot be held to (not a plain number,
// or several of them) is a mismatch.
func TestH1UnclosedStreamDeclaredLength889(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers [][2]string
		n       int
		ended   bool
	}{
		{"exact", [][2]string{{"content-length", "5"}}, 5, true},
		{"exact, mixed case", [][2]string{{"Content-Length", "5"}}, 5, true},
		{"none declared", nil, 5, true},
		{"short", [][2]string{{"content-length", "100"}}, 40, false},
		{"over", [][2]string{{"content-length", "5"}}, 10, false},
		{"nothing written", [][2]string{{"content-length", "5"}}, 0, false},
		{"zero declared, none written", [][2]string{{"content-length", "0"}}, 0, true},
		{"not a number", [][2]string{{"content-length", "5x"}}, 5, false},
		{"signed", [][2]string{{"content-length", "+5"}}, 5, false},
		{"two", [][2]string{{"content-length", "5"}, {"content-length", "5"}}, 5, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := streamHandler889(func(st stream.Streamer, s *stream.Stream) error {
				_ = st.WriteHeader(s, 200, tc.headers)
				return st.Write(s, bytes.Repeat([]byte("x"), tc.n))
			})
			out, err := feed889(t, NewH1State(), h, "GET / HTTP/1.1\r\nHost: x\r\n\r\nGET / HTTP/1.1\r\nHost: x\r\n\r\n")
			ended := strings.Contains(out, "\r\n0\r\n\r\n")
			if ended != tc.ended || (err == nil) != tc.ended {
				t.Errorf("ended=%v err=%v, want ended=%v (and the connection closed otherwise): %q", ended, err, tc.ended, out)
			}
			if !tc.ended && strings.Count(out, "HTTP/1.1 200") != 1 {
				t.Errorf("a response was written behind a response that was cut: %q", out)
			}
			// a closed writer is the handler's own word, whatever it wrote
			h2 := streamHandler889(func(st stream.Streamer, s *stream.Stream) error {
				_ = st.WriteHeader(s, 200, tc.headers)
				_ = st.Write(s, bytes.Repeat([]byte("x"), tc.n))
				return st.Close(s)
			})
			out, err = feed889(t, NewH1State(), h2, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
			if err != nil || !strings.HasSuffix(out, "\r\n0\r\n\r\n") {
				t.Errorf("an explicit Close is left alone: err %v %q", err, out)
			}
		})
	}
}

// Close on a response with no content (1xx, 204, 304) sends no last chunk, and
// those responses carry no transfer-encoding (RFC 9112 section 6.1: a server
// must not send it); a second Close is not a second last chunk, and a Write
// after the headers of a response with no content sends nothing.
func TestH1CloseOnNoContentStatus889(t *testing.T) {
	for _, status := range []int{204, 304} {
		h := streamHandler889(func(st stream.Streamer, s *stream.Stream) error {
			if s.Path == "/ok" {
				return s.ResponseWriter.WriteResponse(s, 200, [][2]string{{"content-length", "2"}}, []byte("ok"))
			}
			_ = st.WriteHeader(s, status, nil)
			_ = st.Write(s, []byte("ignored"))
			return st.Close(s)
		})
		in := "GET /s HTTP/1.1\r\nHost: x\r\n\r\nGET /ok HTTP/1.1\r\nHost: x\r\n\r\n"
		out, err := feed889(t, NewH1State(), h, in)
		if err != nil {
			t.Fatalf("%d: %v", status, err)
		}
		first, rest, _ := strings.Cut(out, "\r\n\r\n")
		if strings.Contains(first, "transfer-encoding") {
			t.Errorf("%d: a response with no content carries transfer-encoding: %q", status, first)
		}
		if !strings.HasPrefix(rest, "HTTP/1.1 200 OK") || strings.Contains(out, "0\r\n\r\n") || strings.Contains(out, "ignored") {
			t.Errorf("%d: bytes of a body or a last chunk on a response with no content: %q", status, out)
		}
	}
	// a second Close
	var out bytes.Buffer
	a := &h1ResponseAdapter{write: func(b []byte) { out.Write(b) }, keepAlive: true}
	_ = a.WriteHeader(nil, 200, nil)
	_ = a.Close(nil)
	_ = a.Close(nil)
	_ = a.Write(nil, []byte("late"))
	if n := strings.Count(out.String(), "0\r\n\r\n"); n != 1 || strings.Contains(out.String(), "late") {
		t.Errorf("Close twice, then a Write: %q", out.String())
	}
}

// A raw handler (not the router, which returns nil) that returns an error after
// a response was begun and ended, or begun for a HEAD or a status with no
// content, must not have a 500 appended behind it.
func TestH1HandlerErrorAfterEndedStream889(t *testing.T) {
	for name, tc := range map[string]struct {
		method string
		body   func(st stream.Streamer, s *stream.Stream)
	}{
		"closed":       {"GET", func(st stream.Streamer, s *stream.Stream) { _ = st.WriteHeader(s, 200, nil); _ = st.Close(s) }},
		"204":          {"GET", func(st stream.Streamer, s *stream.Stream) { _ = st.WriteHeader(s, 204, nil) }},
		"HEAD":         {"HEAD", func(st stream.Streamer, s *stream.Stream) { _ = st.WriteHeader(s, 200, nil) }},
		"HEAD, closed": {"HEAD", func(st stream.Streamer, s *stream.Stream) { _ = st.WriteHeader(s, 200, nil); _ = st.Close(s) }},
	} {
		t.Run(name, func(t *testing.T) {
			h := streamHandler889(func(st stream.Streamer, s *stream.Stream) error {
				tc.body(st, s)
				return errors.New("boom")
			})
			out, err := feed889(t, NewH1State(), h, tc.method+" / HTTP/1.1\r\nHost: x\r\n\r\n")
			if err == nil || strings.Contains(out, "500") || strings.Count(out, "HTTP/1.1") != 1 {
				t.Errorf("err %v, want an error and no second response: %q", err, out)
			}
		})
	}
}

// A handler that sent only interim (1xx) responses through the writer has sent
// no final response: it is answered, with an empty 200, with a 500 when it
// failed or returned an error, and one that went on to a final response is
// left alone (a raw handler's error after a 1xx is answered with the 500; after
// a final response nothing is appended).
func TestH1InterimOnlyStream889(t *testing.T) {
	get := func(p string) string { return "GET " + p + " HTTP/1.1\r\nHost: x\r\n\r\n" }
	ok := func(s *stream.Stream) error {
		return s.ResponseWriter.WriteResponse(s, 200, [][2]string{{"content-length", "2"}}, []byte("ok"))
	}
	for name, tc := range map[string]struct {
		body    func(st stream.Streamer, s *stream.Stream) error
		final   string // the final response wanted behind the 103
		closing bool
	}{
		"returns nil": {func(st stream.Streamer, s *stream.Stream) error { return st.WriteHeader(s, 103, nil) }, "HTTP/1.1 200 OK", false},
		"two 1xx": {func(st stream.Streamer, s *stream.Stream) error {
			_ = st.WriteHeader(s, 103, nil)
			return st.WriteHeader(s, 102, nil)
		}, "HTTP/1.1 200 OK", false},
		"returns an error": {func(st stream.Streamer, s *stream.Stream) error {
			_ = st.WriteHeader(s, 103, nil)
			return errors.New("boom")
		}, "HTTP/1.1 500", true},
		"failed (the router's swallowed error)": {func(st stream.Streamer, s *stream.Stream) error {
			_ = st.WriteHeader(s, 103, nil)
			if f, ok := s.ResponseWriter.(interface{ FailResponse() }); ok {
				f.FailResponse()
			}
			return nil
		}, "HTTP/1.1 500", true},
	} {
		t.Run(name, func(t *testing.T) {
			h := stream.HandlerFunc(func(_ context.Context, s *stream.Stream) error {
				if s.Path == "/ok" {
					return ok(s)
				}
				return tc.body(s.ResponseWriter.(stream.Streamer), s)
			})
			out, err := feed889(t, NewH1State(), h, get("/s")+get("/ok"))
			if !strings.HasPrefix(out, "HTTP/1.1 103") || !strings.Contains(out, "\r\n\r\n"+tc.final) {
				t.Errorf("want the 103 and then %q: %q", tc.final, out)
			}
			if tc.closing && (err == nil || strings.Contains(out, "ok")) {
				t.Errorf("a failed response must close the connection: err %v %q", err, out)
			}
			if !tc.closing && (err != nil || strings.Count(out, "HTTP/1.1 200 OK") != 2 || !strings.HasSuffix(out, "\r\n\r\nok")) {
				t.Errorf("the request behind it must be answered: err %v %q", err, out)
			}
		})
	}
	// a 1xx and then a final response: nothing is added, and an error after it
	// is not answered with a second final response
	h := streamHandler889(func(st stream.Streamer, s *stream.Stream) error {
		_ = st.WriteHeader(s, 103, nil)
		_ = st.WriteHeader(s, 200, nil)
		_ = st.Write(s, []byte("hi"))
		return st.Close(s)
	})
	out, err := feed889(t, NewH1State(), h, get("/s"))
	if err != nil || strings.Count(out, "HTTP/1.1") != 2 || !strings.HasSuffix(out, "2\r\nhi\r\n0\r\n\r\n") {
		t.Errorf("103 then 200: err %v %q", err, out)
	}
	h = streamHandler889(func(st stream.Streamer, s *stream.Stream) error {
		_ = st.WriteHeader(s, 103, nil)
		_ = st.WriteHeader(s, 200, nil)
		return errors.New("boom")
	})
	out, err = feed889(t, NewH1State(), h, get("/s"))
	if err == nil || strings.Contains(out, "500") {
		t.Errorf("an error after a 1xx and a final response: err %v %q", err, out)
	}
}
