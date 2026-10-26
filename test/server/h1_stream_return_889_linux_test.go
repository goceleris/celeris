//go:build linux

package celeris_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// celeris#889 (items 50 and 54's neighbours, 2 and 3), the HTTP/1.1 twin of
// h2_handler_return_889_linux_test.go: a handler that starts a StreamWriter
// response (chunked) and returns without Close. net/http ends the response when
// its handler returns, with the last chunk (0\r\n\r\n); on epoll and io_uring
// (and Adaptive) it used to leave the client waiting for its own timeout. A
// response the handler broke off (a panic the router recovers, an error after
// the headers) must not be ended that way: the connection is closed without the
// last chunk, the HTTP/1.1 form of the reset. HEAD answers have no body and no
// last chunk, and the next request on the connection is answered.

// h1ReturnArms889 are the std engine (net/http is the model) and the native
// ones.
func h1ReturnArms889() []arm848 {
	return append([]arm848{{"std", celeris.Std, nil, ""}}, arms848()...)
}

type h1Return889Rig struct {
	addr string
}

func (r *h1Return889Rig) routes(s *celeris.Server) {
	// (a middleware that aborts the automatic OPTIONS of /optmw with a content-length and no answer)
	s.Use(func(c *celeris.Context) error {
		if c.Method() == "OPTIONS" && c.Path() == "/optmw" {
			c.SetHeader("content-length", "1234")
			c.Abort() // without it the chain goes on to the automatic answer, which sets its own length
			return nil
		}
		return c.Next()
	})
	streamed := func(closeIt bool, n int) celeris.HandlerFunc {
		return func(c *celeris.Context) error {
			sw := c.StreamWriter()
			if err := sw.WriteHeader(200, [][2]string{{"content-type", "text/plain"}}); err != nil {
				return err
			}
			for range n {
				if _, err := sw.Write([]byte("hello")); err != nil {
					return err
				}
			}
			if closeIt {
				return sw.Close()
			}
			return nil
		}
	}
	s.GET("/noclose", streamed(false, 1))
	s.GET("/closed", streamed(true, 1))
	s.GET("/headersonly", streamed(false, 0))
	s.GET("/many", streamed(false, 50))
	s.GET("/anoclose", streamed(false, 1)).Async()
	s.GET("/aheadersonly", streamed(false, 0)).Async()
	broken := func(fail func() error) celeris.HandlerFunc {
		return func(c *celeris.Context) error {
			sw := c.StreamWriter()
			if err := sw.WriteHeader(200, nil); err != nil {
				return err
			}
			if _, err := sw.Write([]byte("hello")); err != nil {
				return err
			}
			return fail()
		}
	}
	panics := func() error { panic("boom889") }
	errs := func() error { return errors.New("boom889") }
	s.GET("/panicstream", broken(panics))
	s.GET("/errstream", broken(errs))
	s.GET("/apanicstream", broken(panics)).Async()
	s.GET("/aerrstream", broken(errs)).Async()
	s.GET("/ok", func(c *celeris.Context) error { return c.String(200, "ok") })
	// A declared content-length, held to when the handler returns unclosed.
	declared := func(length, n int) celeris.HandlerFunc {
		return func(c *celeris.Context) error {
			sw := c.StreamWriter()
			if err := sw.WriteHeader(200, [][2]string{{"content-length", fmt.Sprint(length)}}); err != nil {
				return err
			}
			_, err := sw.Write([]byte(strings.Repeat("x", n)))
			return err
		}
	}
	s.GET("/exact", declared(5, 5))
	s.GET("/short", declared(100, 40))
	s.GET("/aexact", declared(5, 5)).Async()
	s.GET("/ashort", declared(100, 40)).Async()
	// Returns without answering, with a StreamWriter taken and never used or not,
	// and a no-content stream that is closed.
	s.GET("/nothing", func(c *celeris.Context) error { return nil })
	s.GET("/takenonly", func(c *celeris.Context) error { _ = c.StreamWriter(); return nil })
	s.GET("/anothing", func(c *celeris.Context) error { return nil }).Async()
	s.GET("/clset", func(c *celeris.Context) error { c.SetHeader("content-length", "1234"); return nil })
	s.HEAD("/hx", func(c *celeris.Context) error { c.SetHeader("content-length", "1234"); return nil })
	// OPTIONS: a middleware that answers the automatic OPTIONS with a
	// content-length and nothing else, and an OPTIONS route that does.
	s.GET("/optmw", func(c *celeris.Context) error { return c.String(200, "x") })
	s.OPTIONS("/optroute", func(c *celeris.Context) error { c.SetHeader("content-length", "1234"); return nil })
	s.GET("/xa", func(c *celeris.Context) error { c.SetHeader("x-a", "1"); return nil })
	s.GET("/interim", func(c *celeris.Context) error {
		return c.StreamWriter().WriteHeader(103, [][2]string{{"link", "</a.css>; rel=preload"}})
	})
	s.GET("/interimerr", func(c *celeris.Context) error {
		_ = c.StreamWriter().WriteHeader(103, nil)
		return errors.New("boom889")
	})
	s.GET("/s204close", func(c *celeris.Context) error {
		sw := c.StreamWriter()
		_ = sw.WriteHeader(204, nil)
		return sw.Close()
	})
	// No content: a last chunk after these would be read as the next response.
	s.GET("/s204", func(c *celeris.Context) error {
		return c.StreamWriter().WriteHeader(204, nil)
	})
	s.GET("/s304", func(c *celeris.Context) error {
		return c.StreamWriter().WriteHeader(304, nil)
	})
	// A detached stream: 30 chunks over ~600 ms, from a goroutine, the handler
	// returned. Close at the end.
	detached := func(c *celeris.Context) error {
		sw := c.StreamWriter()
		if err := sw.WriteHeader(200, nil); err != nil {
			return err
		}
		done := c.Detach()
		run := func() {
			defer done()
			for i := range 30 {
				if _, err := fmt.Fprintf(sw, "chunk%02d|", i); err != nil {
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
			_ = sw.Close()
		}
		if !c.EngineSupportsAsyncDetach() {
			run() // std: the handler holds net/http's response until the stream ends
			return nil
		}
		go run()
		return nil
	}
	s.GET("/detached", detached)
	s.GET("/adetached", detached).Async()
}

func startH1Return889(t *testing.T, a arm848) *h1Return889Rig {
	t.Helper()
	if a.start != "" {
		t.Setenv("CELERIS_ADAPTIVE_START", a.start)
	}
	r := &h1Return889Rig{}
	deadline := time.Now().Add(10 * time.Second)
	for {
		s := celeris.New(celeris.Config{Engine: a.eng, Protocol: celeris.HTTP1, Workers: 2})
		r.routes(s)
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- s.StartWithListenerAndContext(ctx, ln) }()
		if r.addr, err = c714WaitReady(s, done); err == nil {
			t.Cleanup(cancel)
			return r
		}
		cancel()
		_ = ln.Close()
		if !isRingENOMEM(err) || time.Now().After(deadline) {
			t.Fatalf("server did not start: %v", err)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

type h1Conn889 struct {
	t  *testing.T
	c  net.Conn
	br *bufio.Reader
}

func (r *h1Return889Rig) dial(t *testing.T) *h1Conn889 {
	t.Helper()
	c, err := net.DialTimeout("tcp", r.addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return &h1Conn889{t: t, c: c, br: bufio.NewReader(c)}
}

func (h *h1Conn889) send(s string) {
	h.t.Helper()
	_ = h.c.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.WriteString(h.c, s); err != nil {
		h.t.Fatalf("write: %v", err)
	}
}

// read parses one response, as net/http's client does, and reads its body to the
// end, all within wait. A response whose last chunk never comes ends in the
// deadline error.
func (h *h1Conn889) read(method string, wait time.Duration) (status int, body string, err error) {
	h.t.Helper()
	_ = h.c.SetReadDeadline(time.Now().Add(wait))
	resp, err := http.ReadResponse(h.br, &http.Request{Method: method})
	if err != nil {
		return 0, "", err
	}
	b, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, string(b), err
}

func isTimeout889(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func req889(method, path, extra string) string {
	return method + " " + path + " HTTP/1.1\r\nHost: x\r\n" + extra + "\r\n"
}

// A handler that returns without Close ends its chunked response with the last
// chunk, on a keep-alive connection that then serves the next request, and
// with requests pipelined behind it.
func TestH1UnclosedStreamEndsTheChunkedResponse889(t *testing.T) {
	for _, a := range h1ReturnArms889() {
		t.Run(a.name, func(t *testing.T) {
			need848(t, a.eng)
			r := startH1Return889(t, a)
			for _, tc := range []struct{ path, want string }{
				{"/closed", "hello"}, // the reference: a closed writer
				{"/noclose", "hello"},
				{"/anoclose", "hello"},
				{"/headersonly", ""},
				{"/aheadersonly", ""},
				{"/many", strings.Repeat("hello", 50)},
			} {
				h := r.dial(t)
				h.send(req889("GET", tc.path, ""))
				st, body, err := h.read("GET", 2*time.Second)
				if st != 200 || body != tc.want || err != nil {
					t.Errorf("GET %s: status %d body %q err %v (timeout %v), want 200 %q and a chunked body that ends", tc.path, st, body, err, isTimeout889(err), tc.want)
					continue
				}
				// the same connection serves the next request
				h.send(req889("GET", "/ok", ""))
				if st, body, err := h.read("GET", 2*time.Second); st != 200 || body != "ok" || err != nil {
					t.Errorf("GET /ok after %s on the same connection: %d %q %v", tc.path, st, body, err)
				}
			}
		})
	}
}

// Requests pipelined behind an unclosed stream are answered in order, and the
// bytes of the stream do not spill into the next answer.
func TestH1UnclosedStreamPipelined889(t *testing.T) {
	for _, a := range h1ReturnArms889() {
		t.Run(a.name, func(t *testing.T) {
			need848(t, a.eng)
			r := startH1Return889(t, a)
			h := r.dial(t)
			h.send(req889("GET", "/noclose", "") + req889("GET", "/anoclose", "") + req889("GET", "/ok", "") + req889("GET", "/noclose", ""))
			for i, want := range []string{"hello", "hello", "ok", "hello"} {
				st, body, err := h.read("GET", 2*time.Second)
				if st != 200 || body != want || err != nil {
					t.Fatalf("pipelined response %d: status %d body %q err %v (timeout %v), want 200 %q", i, st, body, err, isTimeout889(err), want)
				}
			}
		})
	}
}

// With "Connection: close" the response is chunked and the connection closes
// after it: the client must read a whole chunked body, not a truncated one.
func TestH1UnclosedStreamConnectionClose889(t *testing.T) {
	for _, a := range h1ReturnArms889() {
		t.Run(a.name, func(t *testing.T) {
			need848(t, a.eng)
			r := startH1Return889(t, a)
			for _, path := range []string{"/noclose", "/anoclose"} {
				h := r.dial(t)
				h.send(req889("GET", path, "Connection: close\r\n"))
				st, body, err := h.read("GET", 2*time.Second)
				if st != 200 || body != "hello" || err != nil {
					t.Errorf("GET %s with Connection: close: status %d body %q err %v (timeout %v), want 200 \"hello\" and a body that ends with its last chunk", path, st, body, err, isTimeout889(err))
				}
				// then EOF
				_ = h.c.SetReadDeadline(time.Now().Add(2 * time.Second))
				if n, err := h.br.Read(make([]byte, 1)); n != 0 || err != io.EOF {
					t.Errorf("GET %s with Connection: close: after the response, read %d bytes err %v, want EOF", path, n, err)
				}
			}
		})
	}
}

// HTTP/1.0 (no keep-alive): what the unclosed handler sends is what the closed
// one sends, byte for byte after the Date header, and the connection closes.
func TestH1UnclosedStreamHTTP10MatchesClosed889(t *testing.T) {
	for _, a := range h1ReturnArms889() {
		t.Run(a.name, func(t *testing.T) {
			need848(t, a.eng)
			r := startH1Return889(t, a)
			raw := func(path string) string {
				h := r.dial(t)
				h.send("GET " + path + " HTTP/1.0\r\nHost: x\r\n\r\n")
				_ = h.c.SetReadDeadline(time.Now().Add(2 * time.Second))
				b, err := io.ReadAll(h.br)
				if err != nil {
					t.Errorf("GET %s HTTP/1.0: read %q: %v (timeout %v), want the connection closed after the response", path, b, err, isTimeout889(err))
				}
				return string(b)
			}
			norm := func(s string) string {
				var out []string
				for _, l := range strings.Split(s, "\r\n") {
					if strings.HasPrefix(strings.ToLower(l), "date:") {
						continue
					}
					out = append(out, l)
				}
				return strings.Join(out, "\r\n")
			}
			ref := norm(raw("/closed"))
			for _, path := range []string{"/noclose", "/anoclose"} {
				if got := norm(raw(path)); got != ref {
					t.Errorf("GET %s HTTP/1.0 sent %q, the closed handler sent %q", path, got, ref)
				}
			}
		})
	}
}

// A response the handler broke off after its headers is not ended with the last
// chunk: the connection is closed, so the client sees a truncated body and not a
// complete one (the std engine is not an arm: the router recovers the panic
// before net/http sees it, so net/http ends the response; as on HTTP/2).
func TestH1StreamedResponseBrokenOffIsNotEnded889(t *testing.T) {
	for _, a := range arms848() {
		t.Run(a.name, func(t *testing.T) {
			need848(t, a.eng)
			r := startH1Return889(t, a)
			for _, path := range []string{"/panicstream", "/errstream", "/apanicstream", "/aerrstream"} {
				h := r.dial(t)
				h.send(req889("GET", path, ""))
				_, body, err := h.read("GET", 2*time.Second)
				if !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Errorf("GET %s: body %q err %v (timeout %v), want io.ErrUnexpectedEOF: a truncated body, the connection closed at once without the last chunk", path, body, err, isTimeout889(err))
				}
			}
			h := r.dial(t)
			h.send(req889("GET", "/ok", ""))
			if st, body, err := h.read("GET", 2*time.Second); st != 200 || body != "ok" || err != nil {
				t.Errorf("GET /ok on a fresh connection: %d %q %v", st, body, err)
			}
		})
	}
}

// HEAD with a StreamWriter: the headers and nothing else, closed or not, and the
// next response on the connection (pipelined or not) is intact. celeris#889
// items 2 and 3: the HEAD-ness of a streamed response comes from the request
// that began it.
func TestH1HeadStreamWriter889(t *testing.T) {
	for _, a := range h1ReturnArms889() {
		t.Run(a.name, func(t *testing.T) {
			need848(t, a.eng)
			r := startH1Return889(t, a)
			for _, path := range []string{"/closed", "/noclose", "/anoclose", "/headersonly", "/many"} {
				// pipelined: HEAD, GET /ok, HEAD, GET /ok
				h := r.dial(t)
				h.send(req889("HEAD", path, "") + req889("GET", "/ok", "") + req889("HEAD", path, "") + req889("GET", "/ok", ""))
				for i, m := range []string{"HEAD", "GET", "HEAD", "GET"} {
					st, body, err := h.read(m, 2*time.Second)
					want := ""
					if m == "GET" {
						want = "ok"
					}
					if st != 200 || body != want || err != nil {
						t.Fatalf("HEAD %s pipelined, response %d (%s): status %d body %q err %v (timeout %v), want 200 %q", path, i, m, st, body, err, isTimeout889(err), want)
					}
				}
			}
		})
	}
}

// A StreamWriter response that is live while more bytes arrive on its
// connection: the detached goroutine goes on writing chunks, whatever the
// connection's next request says (it may be a HEAD). Each chunk reaches the
// client whole and in order, the stream is not suppressed as a HEAD's, and
// under -race nothing in the adapter's per-request state is touched from two
// goroutines (celeris#889 item 2: the pipelined-SSE trigger).
func TestH1LiveDetachedStreamWhileBytesArrive889(t *testing.T) {
	for _, a := range h1ReturnArms889() {
		t.Run(a.name, func(t *testing.T) {
			need848(t, a.eng)
			for _, path := range []string{"/detached", "/adetached"} {
				r := startH1Return889(t, a)
				h := r.dial(t)
				h.send(req889("GET", path, ""))
				_ = h.c.SetReadDeadline(time.Now().Add(3 * time.Second))
				resp, err := http.ReadResponse(h.br, &http.Request{Method: "GET"})
				if err != nil {
					t.Fatalf("GET %s: %v", path, err)
				}
				// while the stream is live, the peer sends the next request, a HEAD
				time.Sleep(100 * time.Millisecond)
				h.send(req889("HEAD", "/ok", ""))
				b, _ := io.ReadAll(resp.Body)
				// What arrived is a prefix of the 30 chunks in order, never anything
				// else (the connection may be closed on the new bytes).
				var want bytes.Buffer
				for i := range 30 {
					fmt.Fprintf(&want, "chunk%02d|", i)
				}
				if !bytes.HasPrefix(want.Bytes(), b) {
					t.Errorf("GET %s: body %q is not a prefix of the stream's chunks", path, b)
				}
				if len(b) < len("chunk00|chunk01|chunk02|") {
					t.Errorf("GET %s: only %q arrived before the connection ended, want the first chunks (the stream was live)", path, b)
				}
			}
		})
	}
}

// A streamed response with no content (204, 304) that is not closed gets no last
// chunk: the bytes would be read as the start of the next response.
func TestH1UnclosedStreamNoContentStatus889(t *testing.T) {
	for _, a := range h1ReturnArms889() {
		t.Run(a.name, func(t *testing.T) {
			need848(t, a.eng)
			r := startH1Return889(t, a)
			for _, tc := range []struct {
				path   string
				status int
			}{{"/s204", 204}, {"/s304", 304}} {
				h := r.dial(t)
				h.send(req889("GET", tc.path, "") + req889("GET", "/ok", ""))
				st, _, err := h.read("GET", 2*time.Second)
				if st != tc.status || err != nil {
					t.Fatalf("GET %s: status %d err %v, want %d", tc.path, st, err, tc.status)
				}
				if st, body, err := h.read("GET", 2*time.Second); st != 200 || body != "ok" || err != nil {
					t.Errorf("GET /ok behind an unclosed %d: status %d body %q err %v (timeout %v)", tc.status, st, body, err, isTimeout889(err))
				}
			}
		})
	}
}

// A handler that declared a content-length and returns without Close: a body of
// exactly that length is ended, one short of it is not (a client that reads a
// chunked body, as net/http's does, would take the last chunk for the end of a
// whole response: net/http frames by the declared length and closes the
// connection on a short write, which the client sees as an unexpected EOF; the
// HTTP/2 processor resets the stream).
func TestH1UnclosedStreamDeclaredContentLength889(t *testing.T) {
	for _, a := range h1ReturnArms889() {
		t.Run(a.name, func(t *testing.T) {
			need848(t, a.eng)
			r := startH1Return889(t, a)
			for _, path := range []string{"/exact", "/aexact"} {
				h := r.dial(t)
				h.send(req889("GET", path, ""))
				if st, body, err := h.read("GET", 2*time.Second); st != 200 || body != "xxxxx" || err != nil {
					t.Errorf("GET %s: status %d body %q err %v (timeout %v), want 200 \"xxxxx\" and a clean end", path, st, body, err, isTimeout889(err))
				}
			}
			for _, path := range []string{"/short", "/ashort"} {
				h := r.dial(t)
				h.send(req889("GET", path, ""))
				_, body, err := h.read("GET", 2*time.Second)
				if !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Errorf("GET %s: body %q err %v (timeout %v), want io.ErrUnexpectedEOF: a body short of its declared length must not end like a whole one", path, body, err, isTimeout889(err))
				}
			}
		})
	}
}

// A handler that returns without answering gets an empty 200, as under net/http
// (the response behind it on the connection is not read as its answer), and a
// no-content stream that is closed gets no last chunk.
func TestH1NoAnswerAndClosedNoContent889(t *testing.T) {
	for _, a := range h1ReturnArms889() {
		t.Run(a.name, func(t *testing.T) {
			need848(t, a.eng)
			r := startH1Return889(t, a)
			for _, path := range []string{"/nothing", "/takenonly", "/anothing"} {
				h := r.dial(t)
				h.send(req889("GET", path, "") + req889("GET", "/ok", ""))
				if st, body, err := h.read("GET", 2*time.Second); st != 200 || body != "" || err != nil {
					t.Errorf("GET %s: status %d body %q err %v (timeout %v), want an empty 200", path, st, body, err, isTimeout889(err))
					continue
				}
				if st, body, err := h.read("GET", 2*time.Second); st != 200 || body != "ok" || err != nil {
					t.Errorf("GET /ok behind %s: status %d body %q err %v (timeout %v)", path, st, body, err, isTimeout889(err))
				}
			}
			h := r.dial(t)
			h.send(req889("GET", "/s204close", "") + req889("GET", "/ok", ""))
			if st, _, err := h.read("GET", 2*time.Second); st != 204 || err != nil {
				t.Fatalf("GET /s204close: status %d err %v", st, err)
			}
			if st, body, err := h.read("GET", 2*time.Second); st != 200 || body != "ok" || err != nil {
				t.Errorf("GET /ok behind a closed 204 stream: status %d body %q err %v", st, body, err)
			}
		})
	}
}

// The fallback answer of a handler that returned without answering is a valid
// response whatever the handler set: one content-length (a handler's own is not
// sent beside the fallback's: two differing ones are a framing error, RFC 9112
// section 6.3, and net/http's client refuses them), a HEAD keeps the size its
// handler declared; and the std engine's own answer is not changed by this.
func TestH1NoAnswerWithHeaders889(t *testing.T) {
	for _, a := range h1ReturnArms889() {
		t.Run(a.name, func(t *testing.T) {
			need848(t, a.eng)
			r := startH1Return889(t, a)
			h := r.dial(t)
			h.send(req889("GET", "/clset", "") + req889("GET", "/ok", ""))
			if st, body, err := h.read("GET", 2*time.Second); st != 200 || body != "" || err != nil {
				t.Errorf("GET /clset: status %d body %q err %v, want an empty 200", st, body, err)
			}
			if st, body, err := h.read("GET", 2*time.Second); st != 200 || body != "ok" || err != nil {
				t.Errorf("GET /ok behind /clset: status %d body %q err %v (timeout %v)", st, body, err, isTimeout889(err))
			}
			h = r.dial(t)
			h.send(req889("HEAD", "/hx", "") + req889("GET", "/ok", ""))
			_ = h.c.SetReadDeadline(time.Now().Add(2 * time.Second))
			resp, err := http.ReadResponse(h.br, &http.Request{Method: "HEAD"})
			// (std answers as net/http did, without the handler's headers)
			if err != nil || resp.StatusCode != 200 || (a.eng != celeris.Std && resp.ContentLength != 1234) {
				t.Fatalf("HEAD /hx: %v %+v, want a 200 declaring 1234", err, resp)
			}
			if st, body, err := h.read("GET", 2*time.Second); st != 200 || body != "ok" || err != nil {
				t.Errorf("GET /ok behind HEAD /hx: status %d body %q err %v (timeout %v)", st, body, err, isTimeout889(err))
			}
			// headers the handler set: std answers as net/http did (without them), the native
			// engines send them with the empty 200
			h = r.dial(t)
			h.send(req889("GET", "/xa", ""))
			_ = h.c.SetReadDeadline(time.Now().Add(2 * time.Second))
			resp, err = http.ReadResponse(h.br, &http.Request{Method: "GET"})
			if err != nil || resp.StatusCode != 200 {
				t.Fatalf("GET /xa: %v %+v", err, resp)
			}
			if got, want := resp.Header.Get("X-A"), map[bool]string{true: "", false: "1"}[a.eng == celeris.Std]; got != want {
				t.Errorf("GET /xa: X-A %q, want %q", got, want)
			}
		})
	}
}

// A handler that sent only an interim response through the StreamWriter gets
// its final response: an empty 200, or a 500 when it failed after the 1xx (std:
// net/http answers 200 and the router swallows the error).
func TestH1InterimOnlyIsAnswered889(t *testing.T) {
	for _, a := range h1ReturnArms889() {
		t.Run(a.name, func(t *testing.T) {
			need848(t, a.eng)
			r := startH1Return889(t, a)
			for _, tc := range []struct {
				path  string
				final int
			}{{"/interim", 200}, {"/interimerr", 500}} {
				h := r.dial(t)
				h.send(req889("GET", tc.path, "") + req889("GET", "/ok", ""))
				st, _, err := h.read("GET", 2*time.Second)
				if st != 103 || err != nil {
					t.Fatalf("GET %s: status %d err %v, want the 103", tc.path, st, err)
				}
				st, _, err = h.read("GET", 2*time.Second)
				want := tc.final
				if a.eng == celeris.Std {
					want = 200
				}
				if st != want || err != nil {
					t.Errorf("GET %s: after the 103 status %d err %v (timeout %v), want a final %d", tc.path, st, err, isTimeout889(err), want)
				}
				if tc.final == 200 || a.eng == celeris.Std {
					if st, body, err := h.read("GET", 2*time.Second); st != 200 || body != "ok" || err != nil {
						t.Errorf("GET /ok behind %s: status %d body %q err %v (timeout %v)", tc.path, st, body, err, isTimeout889(err))
					}
				}
			}
		})
	}
}

// The automatic OPTIONS answer behind a middleware that aborts without answering
// (/optmw: answerOptions' fallback) and an OPTIONS route that returns without
// answering (/optroute: the matched route's fallback) are answered with one
// content-length: 0, not beside a content-length the handler set (celeris#889).
func TestH1OptionsNoAnswerWithContentLength889(t *testing.T) {
	for _, a := range h1ReturnArms889() {
		t.Run(a.name, func(t *testing.T) {
			need848(t, a.eng)
			r := startH1Return889(t, a)
			for _, path := range []string{"/optmw", "/optroute"} {
				h := r.dial(t)
				h.send(req889("OPTIONS", path, "") + req889("GET", "/ok", ""))
				_ = h.c.SetReadDeadline(time.Now().Add(2 * time.Second))
				resp, err := http.ReadResponse(h.br, &http.Request{Method: "OPTIONS"})
				if err != nil || resp.StatusCode != 200 {
					t.Errorf("OPTIONS %s: %v %+v, want a 200 with one content-length", path, err, resp)
					continue
				}
				if b, _ := io.ReadAll(resp.Body); len(b) != 0 || resp.ContentLength != 0 {
					t.Errorf("OPTIONS %s: content-length %d body %q, want the empty answer", path, resp.ContentLength, b)
				}
				if st, body, err := h.read("GET", 2*time.Second); st != 200 || body != "ok" || err != nil {
					t.Errorf("GET /ok behind OPTIONS %s: status %d body %q err %v (timeout %v)", path, st, body, err, isTimeout889(err))
				}
			}
		})
	}
}
