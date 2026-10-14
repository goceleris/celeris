package celeris_test

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/cors"
	"github.com/goceleris/celeris/middleware/debug"
	"github.com/goceleris/celeris/middleware/etag"
	"github.com/goceleris/celeris/middleware/healthcheck"
	"github.com/goceleris/celeris/middleware/pprof"
	"github.com/goceleris/celeris/middleware/sse"
	"github.com/goceleris/celeris/middleware/static"
	"github.com/goceleris/celeris/middleware/swagger"
)

// celeris#927: a Use-mounted middleware that answers a request itself
// (swagger, pprof, debug, healthcheck and static for their paths, cors for a
// preflight) returns without calling Next, and only Abort stopped a chain, so
// a route that also matched the request (a catch-all, an explicit OPTIONS
// route) ran after it. Unbuffered, its write failed with ErrResponseWritten,
// which then reached every middleware above as a failed request; behind a
// buffering middleware (etag) it replaced the middleware's response. The
// middleware that answered wins: once a handler has answered (written the
// response, had it captured by a buffering middleware, or taken the
// connection over), the rest of the chain does not run.

const routeBody927 = "route answered"

// routedCase927 is one request and the answer it must get.
type routedCase927 struct {
	method, path string
	hdr          [][2]string
	status       int
	body         string // the body must contain it
	routeRuns    int32  // how often the matching route may run
}

func routedCases927() []routedCase927 {
	preflight := [][2]string{{"Origin", "https://app.example"}, {"Access-Control-Request-Method", "POST"}}
	return []routedCase927{
		{method: "GET", path: "/swagger/spec", status: 200, body: "Spec927"},
		{method: "GET", path: "/debug/pprof/", status: 200, body: "goroutine"},
		{method: "GET", path: "/debug/celeris/status", status: 200, body: "uptime"},
		{method: "GET", path: "/livez", status: 200, body: `"ok"`},
		{method: "GET", path: "/static/hello.txt", status: 200, body: "hello 927"},
		{method: "OPTIONS", path: "/api/items", hdr: preflight, status: 204},
		// A Use-mounted middleware that takes the connection over (sse
		// detaches it) answers too.
		{method: "GET", path: "/events", status: 200, body: "data: hello 927"},
		// Controls: the routes still answer what no middleware does.
		{method: "GET", path: "/anything/else", status: 200, body: routeBody927, routeRuns: 1},
		{method: "OPTIONS", path: "/api/items", status: 200, body: routeBody927, routeRuns: 1},
	}
}

// setupRouted927 mounts the answering middlewares with Use behind an outer
// middleware that records what its Next returned, optionally behind etag,
// and registers routes that also match their requests: a GET catch-all and
// an OPTIONS route.
func setupRouted927(withETag bool, runs *atomic.Int32, nextErrs *sync.Map) func(*celeris.Server) {
	return func(s *celeris.Server) {
		s.Use(func(c *celeris.Context) error {
			key := c.Method() + " " + c.Path()
			err := c.Next()
			nextErrs.Store(key, err)
			return err
		})
		// sse streams, so it goes in front of the buffering etag.
		events := sse.New(sse.Config{HeartbeatInterval: -1, Handler: func(client *sse.Client) {
			_ = client.SendData("hello 927")
		}})
		s.Use(func(c *celeris.Context) error {
			if c.Path() == "/events" {
				return events(c)
			}
			return c.Next()
		})
		if withETag {
			s.Use(etag.New())
		}
		s.Use(
			cors.New(),
			swagger.New(swagger.Config{SpecContent: []byte(`{"openapi":"3.0.0","info":{"title":"Spec927","version":"1.0"},"paths":{}}`)}),
			pprof.New(),
			debug.New(debug.Config{Server: s}),
			healthcheck.New(),
			static.New(static.Config{FS: fstest.MapFS{"hello.txt": {Data: []byte("hello 927")}}, Prefix: "/static"}),
		)
		route := func(c *celeris.Context) error {
			runs.Add(1)
			return c.String(200, routeBody927)
		}
		s.GET("/*filepath", route)
		s.OPTIONS("/api/items", route)
	}
}

// checkRouted927 starts a server on eng in each setup and sends every case.
func checkRouted927(t *testing.T, eng celeris.EngineType) {
	for _, withETag := range []bool{false, true} {
		name := "direct"
		if withETag {
			name = "behind-etag"
		}
		t.Run(name, func(t *testing.T) {
			var runs atomic.Int32
			var nextErrs sync.Map
			addr := startServer852(t, eng, setupRouted927(withETag, &runs, &nextErrs))
			cl := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{MaxConnsPerHost: 1, DisableCompression: true}}
			defer cl.CloseIdleConnections()
			for _, rq := range routedCases927() {
				desc := rq.method + " " + rq.path
				if len(rq.hdr) > 0 {
					desc += " (preflight)"
				}
				runs.Store(0)
				nextErrs.Delete(rq.method + " " + rq.path)
				req, err := http.NewRequest(rq.method, "http://"+addr+rq.path, nil)
				if err != nil {
					t.Fatal(err)
				}
				for _, h := range rq.hdr {
					req.Header.Set(h[0], h[1])
				}
				resp, err := cl.Do(req)
				if err != nil {
					t.Errorf("%s: %v", desc, err)
					continue
				}
				b, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if err != nil {
					t.Errorf("%s: reading the body: %v", desc, err)
					continue
				}
				if resp.StatusCode != rq.status || !strings.Contains(string(b), rq.body) ||
					(rq.routeRuns == 0 && strings.Contains(string(b), routeBody927)) {
					t.Errorf("%s: %d %.80q, want %d with %q (the answer of the middleware that answered)", desc, resp.StatusCode, b, rq.status, rq.body)
				}
				if len(rq.hdr) > 0 && resp.Header.Get("Access-Control-Allow-Origin") == "" {
					t.Errorf("%s: no Access-Control-Allow-Origin: not cors' preflight answer", desc)
				}
				if r := runs.Load(); r != rq.routeRuns {
					t.Errorf("%s: the route ran %d times, want %d", desc, r, rq.routeRuns)
				}
				// The outer middleware records after the response is out;
				// give a sync handler on another goroutine a moment.
				var got any
				for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
					var ok bool
					if got, ok = nextErrs.Load(rq.method + " " + rq.path); ok {
						break
					}
				}
				if err, _ := got.(error); err != nil {
					t.Errorf("%s: the outer middleware's Next returned %v, want nil (the request was served)", desc, err)
				}
			}
		})
	}
}

// TestRouteDoesNotRunAfterAMiddlewareAnswered927 runs the check on the std
// engine on every OS; routed_answer_927_linux_test.go adds the native
// engines.
func TestRouteDoesNotRunAfterAMiddlewareAnswered927(t *testing.T) {
	checkRouted927(t, celeris.Std)
}
