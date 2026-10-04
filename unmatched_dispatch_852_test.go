package celeris

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"
)

// celeris#852, dispatch: an unmatched request runs the global middleware (and
// any NotFound / MethodNotAllowed handler), which can block as a route's
// chain can. Under the AsyncHandlers default it is dispatched like a route
// that inherits that default: inline until a run of its chain blocks, then
// async, for 404s, 405s and the automatic OPTIONS answer alike. Without the
// default it runs inline, as such a route does.

// startStd852 starts s on the std engine (Start builds the unmatched chains
// and decides their dispatch, as on every engine) and returns its base URL
// and a client; the server stops at cleanup. The client keeps one
// connection, so a request is read only after the previous request's handler
// has returned (barrier852).
func startStd852(t *testing.T, s *Server) (string, *http.Client) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.StartWithListenerAndContext(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("server did not stop within 10s")
		}
	})
	base := "http://" + ln.Addr().String()
	tr := &http.Transport{MaxConnsPerHost: 1}
	t.Cleanup(tr.CloseIdleConnections)
	cl := &http.Client{Timeout: 5 * time.Second, Transport: tr}
	for deadline := time.Now().Add(5 * time.Second); ; {
		resp, err := cl.Get(base + "/ready-852")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server not ready: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return base, cl
}

// barrier852 returns once the server has finished the client's previous
// request. std flushes a response from inside the chain, before HandleStream
// times the run and promotes the unmatched chain, so the client can see the
// 404 before the run is recorded; the next request on the one connection is
// read only after that.
func barrier852(t *testing.T, cl *http.Client, base string) {
	t.Helper()
	get852(t, cl, "GET", base+"/ready-852")
}

// get852 sends one request and returns its status.
func get852(t *testing.T, cl *http.Client, method, url string) int {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

// TestUnmatchedDispatchedLikeDefaultRoute852: after one unmatched request
// whose chain blocked (5 ms, over adaptiveBlockingThreshold), routeAsync, the
// engines' inline-or-async decision (H1 per request, H2 per stream), answers
// true for every unmatched request when the server default is async, and
// false when it is not; routed requests are decided as before. Fast runs do
// not promote. (The startup probe, GET /ready-852, is itself an unmatched
// request: fast.)
func TestUnmatchedDispatchedLikeDefaultRoute852(t *testing.T) {
	block := func(c *Context) {
		if c.Path() == "/block" {
			time.Sleep(5 * time.Millisecond)
		}
	}
	unmatched := [][2]string{
		{"GET", "/nope"},     // 404
		{"HEAD", "/nope"},    // 404
		{"PURGE", "/nope"},   // 404, custom method
		{"DELETE", "/ping"},  // 405
		{"OPTIONS", "/ping"}, // the automatic OPTIONS answer
	}
	for _, tc := range []struct {
		name         string
		async        bool // Config.AsyncHandlers
		blocker      string
		wantPromoted bool
	}{
		{"async-default/global-middleware-blocks", true, "middleware", true},
		{"async-default/notfound-handler-blocks", true, "notfound", true},
		{"sync-default/global-middleware-blocks", false, "middleware", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(Config{Engine: Std, AsyncHandlers: tc.async})
			if tc.blocker == "middleware" {
				s.Use(func(c *Context) error { block(c); return c.Next() })
			} else {
				s.NotFound(func(c *Context) error { block(c); return c.String(404, "nf") })
			}
			s.GET("/ping", func(c *Context) error { return c.String(200, "pong") })
			s.GET("/db", func(c *Context) error { return c.String(200, "db") }).Async()
			base, cl := startStd852(t, s)
			rt := s.router

			for range 3 {
				if st := get852(t, cl, "GET", base+"/nope"); st != 404 {
					t.Fatalf("GET /nope: %d, want 404", st)
				}
			}
			for _, rq := range unmatched {
				if rt.routeAsync(rq[0], rq[1]) {
					t.Errorf("after fast runs: routeAsync(%s %s) = true, want false (inline until a run blocks)", rq[0], rq[1])
				}
			}
			if st := get852(t, cl, "GET", base+"/block"); st != 404 {
				t.Fatalf("GET /block: %d, want 404", st)
			}
			barrier852(t, cl, base)
			for _, rq := range unmatched {
				if got := rt.routeAsync(rq[0], rq[1]); got != tc.wantPromoted {
					t.Errorf("after a blocking run: routeAsync(%s %s) = %v, want %v", rq[0], rq[1], got, tc.wantPromoted)
				}
			}
			for _, rq := range []struct {
				method, path string
				want         bool
			}{{"GET", "/ping", false}, {"HEAD", "/ping", false}, {"GET", "/db", true}} {
				if got := rt.routeAsync(rq.method, rq.path); got != rq.want {
					t.Errorf("routed: routeAsync(%s %s) = %v, want %v", rq.method, rq.path, got, rq.want)
				}
			}
		})
	}
}

// TestUnmatchedDispatchWithoutRoutes852: with the async default and global
// middleware but no route at all, hasAsyncRoutes (which gates the H2
// processor's per-stream question) is true, so an unmatched stream whose
// chain blocked is not run inline either; and with no adaptive route (only
// an explicit .Async() one) the settle re-opener (celeris#592) still runs,
// for the unmatched chain.
func TestUnmatchedDispatchWithoutRoutes852(t *testing.T) {
	s := New(Config{Engine: Std, AsyncHandlers: true})
	s.Use(func(c *Context) error {
		if c.Path() == "/block" {
			time.Sleep(5 * time.Millisecond)
		}
		return c.Next()
	})
	base, cl := startStd852(t, s)
	if !s.router.hasAsyncRoutes() {
		t.Error("hasAsyncRoutes = false with the async default and a global middleware: H2 would never ask routeAsync")
	}
	get852(t, cl, "GET", base+"/block")
	barrier852(t, cl, base)
	if !s.router.routeAsync("GET", "/anything") {
		t.Error("routeAsync(GET /anything) = false after a blocking run, want true")
	}

	s2 := New(Config{Engine: Std, AsyncHandlers: true})
	s2.Use(func(c *Context) error { return c.Next() })
	s2.GET("/db", func(c *Context) error { return nil }).Async() // explicit: not adaptive
	startStd852(t, s2)
	s2.router.reopenMu.Lock()
	running := s2.router.reopenStop != nil
	s2.router.reopenMu.Unlock()
	if !running {
		t.Error("settle re-opener not started: a settled unmatched chain would never be re-timed (celeris#592)")
	}
}

// TestUnmatchedBuiltinAnswerNotTimed852: with the async default but nothing
// in the unmatched chain except the built-in answer (no global middleware, no
// NotFound / MethodNotAllowed handler), which cannot block, unmatched requests
// stay on the inline path untimed, as before.
func TestUnmatchedBuiltinAnswerNotTimed852(t *testing.T) {
	s := New(Config{Engine: Std, AsyncHandlers: true})
	s.GET("/ping", func(c *Context) error { return c.String(200, "pong") })
	base, cl := startStd852(t, s)
	for range 10 {
		get852(t, cl, "GET", base+"/nope")
	}
	barrier852(t, cl, base)
	if _, timed := s.router.fastStreak.Load("<unmatched>"); timed {
		t.Error("the built-in 404 was timed as an adaptive chain")
	}
	if s.router.routeAsync("GET", "/nope") {
		t.Error("routeAsync(GET /nope) = true for the built-in answer")
	}
}

// TestAsyncHandlersReadWhileStarting852: a driver opened WithEngine(srv)
// calls Server.AsyncHandlers first, possibly while Start runs on another
// goroutine with nothing ordering the two. With the sync default that call
// reads router.unmatchedAdaptive, so Start must not write it then; with the
// async default it returns before reading it. No route is registered, as an
// async one would answer hasAsyncRoutes before the flag is read. Under -race
// a write by Start is a DATA RACE report, which fails the test.
func TestAsyncHandlersReadWhileStarting852(t *testing.T) {
	for _, tc := range []struct {
		name  string
		async bool // Config.AsyncHandlers
		mw    bool // a global middleware
	}{
		{"sync-default", false, false},
		{"sync-default/global-middleware", false, true},
		{"async-default/global-middleware", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(Config{Engine: Std, AsyncHandlers: tc.async})
			if tc.mw {
				s.Use(func(c *Context) error { return c.Next() })
			}
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- s.StartWithListenerAndContext(ctx, ln) }()
			for deadline := time.Now().Add(200 * time.Millisecond); time.Now().Before(deadline); {
				if got := s.AsyncHandlers(); got != tc.async {
					t.Errorf("AsyncHandlers() = %v during Start, want %v", got, tc.async)
					break
				}
			}
			cancel()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Error("server did not stop within 10s")
			}
		})
	}
}

// TestDetachedRequestNotAnsweredOnErrorOrPanic852: a global middleware
// detaches the request and returns without Next (a Use-mounted websocket or
// sse middleware does); a later global middleware returns an error or
// panics. Neither the error path nor the panic path writes a response under
// the detached request, routed or not; the OnError handler is still called.
func TestDetachedRequestNotAnsweredOnErrorOrPanic852(t *testing.T) {
	for _, shape := range []string{"error", "panic"} {
		for _, path := range []string{"/missing", "/exists"} {
			t.Run(shape+path, func(t *testing.T) {
				var done func()
				var onError []error
				s := New(Config{Logger: slog.New(slog.DiscardHandler)}) // the panic arm logs a stack
				s.OnError(func(c *Context, err error) {
					onError = append(onError, err)
					_ = c.String(500, "from OnError") // refused: ErrDetached
				})
				// Detaching answers the request (celeris#927): the middleware
				// calls Next itself so that the next one still runs.
				s.Use(func(c *Context) error { done = c.Detach(); return c.Next() })
				s.Use(func(*Context) error {
					if shape == "panic" {
						panic("after detach")
					}
					return NewHTTPError(429, "Too Many Requests")
				})
				s.GET("/exists", func(c *Context) error { return c.String(200, "ok") })
				st, _ := newTestStream("GET", path)
				rw := &recordingRW852{}
				st.ResponseWriter = rw
				if err := (&routerAdapter{server: s, errorHandler: s.errorHandler}).HandleStream(context.Background(), st); err != nil {
					t.Fatalf("HandleStream: %v", err)
				}
				if done == nil {
					t.Fatal("the detaching middleware did not run")
				}
				done()
				st.Release()
				if len(rw.writes) != 0 {
					t.Errorf("%d responses written under the detached request, want 0: %+v", len(rw.writes), rw.writes)
				}
				if shape == "error" {
					var he *HTTPError
					if len(onError) != 1 || !errors.As(onError[0], &he) || he.Code != 429 {
						t.Errorf("OnError saw %v, want the 429 once", onError)
					}
				}
			})
		}
	}
}
