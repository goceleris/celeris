package celeris

import (
	"context"
	"strings"
	"testing"

	"github.com/goceleris/celeris/internal/protocol/h2/stream"
)

// celeris#852: the global middleware (Server.Use) runs for a request no route
// matches, before its 404 or 405 answer, with or without a NotFound /
// MethodNotAllowed handler; the answer itself is unchanged.

// recordingRW852 records every response written to the stream.
type recordingRW852 struct {
	writes []recordedResponse852
}

type recordedResponse852 struct {
	status  int
	headers [][2]string
	body    string
}

func (r *recordingRW852) WriteResponse(_ *stream.Stream, status int, headers [][2]string, body []byte) error {
	r.writes = append(r.writes, recordedResponse852{status, append([][2]string(nil), headers...), string(body)})
	return nil
}

func (r recordedResponse852) header(k string) []string {
	var v []string
	for _, h := range r.headers {
		if h[0] == k {
			v = append(v, h[1])
		}
	}
	return v
}

// serve852 runs one request through a bare adapter, as the other tests of
// this package do (handleUnmatched builds the chains per request; the chains
// Start builds are covered by the live-server tests in
// unmatched_mounts_852_test.go), and returns what it wrote, keyed by the arm
// name the assertions print.
func serve852(t *testing.T, s *Server, method, path string) map[string]*recordingRW852 {
	t.Helper()
	st, _ := newTestStream(method, path)
	rw := &recordingRW852{}
	st.ResponseWriter = rw
	if err := (&routerAdapter{server: s}).HandleStream(context.Background(), st); err != nil {
		t.Fatalf("%s %s: HandleStream: %v", method, path, err)
	}
	st.Release()
	return map[string]*recordingRW852{"adapter": rw}
}

// one852 returns the single response rw recorded, failing on zero or several.
func one852(t *testing.T, desc string, rw *recordingRW852) recordedResponse852 {
	t.Helper()
	if len(rw.writes) != 1 {
		t.Fatalf("%s: %d responses written, want 1: %+v", desc, len(rw.writes), rw.writes)
	}
	return rw.writes[0]
}

// TestGlobalMiddlewareRunsOnUnmatched852: a 404 and a 405 run the global
// middleware, which sees the request and can set a header, and the answer is
// the one the server gives without middleware: the built-in 404 / 405 (with
// the Allow list) or the custom handler's.
func TestGlobalMiddlewareRunsOnUnmatched852(t *testing.T) {
	for _, custom := range []bool{false, true} {
		name := "builtin"
		if custom {
			name = "custom-handlers"
		}
		t.Run(name, func(t *testing.T) {
			var seen []string
			s := New(Config{})
			s.Use(func(c *Context) error {
				seen = append(seen, c.Method()+" "+c.Path())
				c.SetHeader("x-global", "ran")
				return c.Next()
			})
			s.GET("/exists", func(c *Context) error { return c.String(200, "ok") })
			want404, want405 := "404 Not Found", "405 Method Not Allowed"
			if custom {
				s.NotFound(func(c *Context) error { return c.String(404, "custom 404") })
				s.MethodNotAllowed(func(c *Context) error { return c.String(405, "custom 405") })
				want404, want405 = "custom 404", "custom 405"
			}
			for _, rq := range []struct {
				method, path, body string
				status             int
			}{
				{"GET", "/missing", want404, 404},
				{"HEAD", "/missing", want404, 404},
				{"POST", "/exists", want405, 405},
			} {
				for arm, rw := range serve852(t, s, rq.method, rq.path) {
					desc := arm + " " + rq.method + " " + rq.path
					r := one852(t, desc, rw)
					if r.status != rq.status || r.body != rq.body {
						t.Errorf("%s: %d %q, want %d %q", desc, r.status, r.body, rq.status, rq.body)
					}
					if g := r.header("x-global"); len(g) != 1 || g[0] != "ran" {
						t.Errorf("%s: x-global %v: the global middleware did not run", desc, g)
					}
					if !custom {
						if ct := r.header("content-type"); len(ct) != 1 || ct[0] != "text/plain" {
							t.Errorf("%s: content-type %v, want [text/plain]", desc, ct)
						}
					}
					allow := r.header("allow")
					switch {
					case rq.status == 405 && (len(allow) != 1 || !strings.Contains(allow[0], "GET") || strings.Contains(allow[0], "POST")):
						t.Errorf("%s: Allow %v, want one header listing GET and not POST", desc, allow)
					case rq.status == 404 && len(allow) != 0:
						t.Errorf("%s: Allow %v on a 404", desc, allow)
					}
				}
			}
			if len(seen) != 3 {
				t.Errorf("the global middleware saw %d unmatched requests, want 3: %v", len(seen), seen)
			}
		})
	}
}

// answering852 serves /docs/ itself without calling Next, as swagger, pprof
// and the other path-prefix middlewares do: that does not stop the chain.
func answering852(c *Context) error {
	if strings.HasPrefix(c.Path(), "/docs/") {
		return c.String(200, "docs page")
	}
	return c.Next()
}

// buffering852 captures the response the rest of the chain writes and sends
// it on the way out, as compress, etag or cache do.
func buffering852(c *Context) error {
	c.BufferResponse()
	err := c.Next()
	if ferr := c.FlushResponse(); ferr != nil && err == nil {
		err = ferr
	}
	return err
}

// bufferingNoFlush852 captures the response and leaves the send to the
// server, which flushes a captured response after the chain.
func bufferingNoFlush852(c *Context) error {
	c.BufferResponse()
	return c.Next()
}

// TestUseMountedMiddlewareAnswersUnmatched852: a Use-mounted middleware that
// serves its own path answers it with no route for it, directly and behind
// a buffering middleware (one that flushes, one that leaves the flush to the
// server); the not-found end of the chain (built-in or custom) does not
// answer again, overwrite the buffered page, or hand the middleware above an
// error. Other paths still get the 404.
func TestUseMountedMiddlewareAnswersUnmatched852(t *testing.T) {
	for _, buffer := range []string{"direct", "buffered", "buffered-no-flush"} {
		for _, custom := range []bool{false, true} {
			name := buffer + "/" + map[bool]string{false: "builtin", true: "custom-handlers"}[custom]
			t.Run(name, func(t *testing.T) {
				var outerErrs []error
				s := New(Config{})
				s.Use(func(c *Context) error {
					err := c.Next()
					outerErrs = append(outerErrs, err)
					return err
				})
				switch buffer {
				case "buffered":
					s.Use(buffering852)
				case "buffered-no-flush":
					s.Use(bufferingNoFlush852)
				}
				s.Use(answering852)
				s.GET("/exists", func(c *Context) error { return c.String(200, "ok") })
				want404 := "404 Not Found"
				if custom {
					s.NotFound(func(c *Context) error { return c.String(404, "custom 404") })
					s.MethodNotAllowed(func(c *Context) error { return c.String(405, "custom 405") })
					want404 = "custom 404"
				}
				for _, rq := range []struct {
					method, path, body string
					status             int
				}{
					{"GET", "/docs/index", "docs page", 200},
					{"GET", "/elsewhere", want404, 404},
				} {
					for arm, rw := range serve852(t, s, rq.method, rq.path) {
						desc := arm + " " + rq.method + " " + rq.path
						r := one852(t, desc, rw)
						if r.status != rq.status || r.body != rq.body {
							t.Errorf("%s: %d %q, want %d %q", desc, r.status, r.body, rq.status, rq.body)
						}
					}
				}
				for i, err := range outerErrs {
					if err != nil {
						t.Errorf("request %d: the outermost middleware got %v from Next, want nil", i, err)
					}
				}
				if len(outerErrs) != 2 {
					t.Errorf("the outermost middleware ran %d times, want 2", len(outerErrs))
				}
			})
		}
	}
}

// TestUnmatchedNothingAnsweredFallsBack852: when nothing in the chain answers
// (a middleware aborts without writing, or the custom handler writes
// nothing), the built-in 404 / 405 is still sent, once, with one Allow
// header on the 405.
func TestUnmatchedNothingAnsweredFallsBack852(t *testing.T) {
	for _, shape := range []string{"middleware-aborts", "custom-handlers-write-nothing"} {
		t.Run(shape, func(t *testing.T) {
			s := New(Config{})
			if shape == "middleware-aborts" {
				s.Use(func(c *Context) error { c.Abort(); return nil })
			} else {
				s.NotFound(func(*Context) error { return nil })
				s.MethodNotAllowed(func(*Context) error { return nil })
			}
			s.GET("/exists", func(c *Context) error { return c.String(200, "ok") })
			for _, rq := range []struct {
				method, path, body string
				status             int
			}{
				{"GET", "/missing", "404 Not Found", 404},
				{"POST", "/exists", "405 Method Not Allowed", 405},
			} {
				for arm, rw := range serve852(t, s, rq.method, rq.path) {
					desc := arm + " " + rq.method + " " + rq.path
					r := one852(t, desc, rw)
					if r.status != rq.status || r.body != rq.body {
						t.Errorf("%s: %d %q, want %d %q", desc, r.status, r.body, rq.status, rq.body)
					}
					if n := len(r.header("allow")); rq.status == 405 && n != 1 {
						t.Errorf("%s: %d Allow headers, want 1: %v", desc, n, r.headers)
					}
				}
			}
		})
	}
}

// TestGlobalMiddlewareErrorAnswersUnmatched852: an error a global middleware
// returns for an unmatched request is answered like one for a routed request
// (an authentication middleware's 401 instead of the 404).
func TestGlobalMiddlewareErrorAnswersUnmatched852(t *testing.T) {
	s := New(Config{})
	s.Use(func(*Context) error { return NewHTTPError(401, "Unauthorized") })
	s.GET("/exists", func(c *Context) error { return c.String(200, "ok") })
	for _, rq := range [][2]string{{"GET", "/missing"}, {"POST", "/exists"}, {"GET", "/exists"}} {
		for arm, rw := range serve852(t, s, rq[0], rq[1]) {
			desc := arm + " " + rq[0] + " " + rq[1]
			if r := one852(t, desc, rw); r.status != 401 || r.body != "Unauthorized" {
				t.Errorf("%s: %d %q, want 401 Unauthorized", desc, r.status, r.body)
			}
		}
	}
}

// TestUnmatchedDetachedByMiddlewareNotAnswered852: a global middleware that
// takes a request over (Detach, as a WebSocket or SSE middleware does) owns
// its response: nothing in the not-found chain writes one.
func TestUnmatchedDetachedByMiddlewareNotAnswered852(t *testing.T) {
	for _, custom := range []bool{false, true} {
		t.Run(map[bool]string{false: "builtin", true: "custom-handlers"}[custom], func(t *testing.T) {
			var done func()
			s := New(Config{})
			s.Use(func(c *Context) error {
				if c.Path() == "/live" {
					done = c.Detach()
					return nil
				}
				return c.Next()
			})
			if custom {
				s.NotFound(func(c *Context) error { return c.String(404, "custom 404") })
			}
			s.GET("/exists", func(c *Context) error { return c.String(200, "ok") })
			rws := serve852(t, s, "GET", "/live")
			if done == nil {
				t.Fatal("the middleware did not run for the unmatched request")
			}
			done()
			for arm, rw := range rws {
				if len(rw.writes) != 0 {
					t.Errorf("%s: %d responses written for a detached request, want 0: %+v", arm, len(rw.writes), rw.writes)
				}
			}
		})
	}
}

// TestAutoOptionsAfterAnsweringMiddleware852: the automatic OPTIONS answer
// (celeris#421) is the same kind of chain end: a global middleware that answers
// an OPTIONS request without calling Next (as cors does for a preflight) is not
// answered again and the middleware above it gets nil from Next, and one that
// takes the request over gets no response written for it.
func TestAutoOptionsAfterAnsweringMiddleware852(t *testing.T) {
	for _, shape := range []string{"answers", "detaches"} {
		t.Run(shape, func(t *testing.T) {
			var outerErrs []error
			var done func()
			s := New(Config{})
			s.Use(func(c *Context) error {
				err := c.Next()
				outerErrs = append(outerErrs, err)
				return err
			})
			s.Use(func(c *Context) error {
				if c.Method() != "OPTIONS" {
					return c.Next()
				}
				if shape == "detaches" {
					done = c.Detach()
					return nil
				}
				return c.NoContent(204)
			})
			s.GET("/hello", func(c *Context) error { return c.String(200, "hi") })
			rws := serve852(t, s, "OPTIONS", "/hello")
			if done != nil {
				done()
			}
			for arm, rw := range rws {
				want := 1
				if shape == "detaches" {
					want = 0
				}
				if len(rw.writes) != want {
					t.Fatalf("%s: %d responses written, want %d: %+v", arm, len(rw.writes), want, rw.writes)
				}
				if want == 1 && rw.writes[0].status != 204 {
					t.Errorf("%s: status %d, want the middleware's 204", arm, rw.writes[0].status)
				}
			}
			if len(outerErrs) != 1 || outerErrs[0] != nil {
				t.Errorf("the outermost middleware got %v from Next, want [<nil>]", outerErrs)
			}
		})
	}
}
