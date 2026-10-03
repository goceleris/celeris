package celeris_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/debug"
	"github.com/goceleris/celeris/middleware/etag"
	"github.com/goceleris/celeris/middleware/healthcheck"
	"github.com/goceleris/celeris/middleware/pprof"
	"github.com/goceleris/celeris/middleware/static"
	"github.com/goceleris/celeris/middleware/swagger"
)

// celeris#852: the middlewares that serve their own paths, mounted with
// Server.Use as their docs say, answer those paths on a real server with no
// route registered for them; an unknown path still gets the 404, a known
// path's other methods the 405 with Allow, and every one of these responses
// went through the global middleware.

const spec852 = `{"openapi":"3.0.0","info":{"title":"Spec852","version":"1.0"},"paths":{}}`

// mountCase852 is one request and what the response must hold.
type mountCase852 struct {
	method, path string
	status       int
	body         string // the response body must contain it
	header       [2]string
}

// useMountCases852 are the requests checkUseMounts852 sends. Assets and other
// paths come from the middlewares' own exported names.
func useMountCases852() []mountCase852 {
	bundle := "/swagger/assets/swagger-ui-dist@" + swagger.SwaggerUIVersion + "/swagger-ui-bundle.js"
	return []mountCase852{
		{method: "GET", path: "/swagger/", status: 200, body: "swagger-ui"},
		{method: "GET", path: "/swagger/spec", status: 200, body: "Spec852"},
		{method: "GET", path: bundle, status: 200, body: "SwaggerUIBundle"},
		{method: "GET", path: "/swagger", status: 301},
		{method: "GET", path: "/debug/pprof/", status: 200, body: "goroutine"},
		{method: "GET", path: "/debug/celeris/status", status: 200, body: "uptime"},
		{method: "GET", path: "/livez", status: 200, body: `"ok"`},
		{method: "GET", path: "/static/hello.txt", status: 200, body: "hello 852"},
		{method: "GET", path: "/ping", status: 200, body: "pong"},
		{method: "GET", path: "/nope", status: 404, body: "404 Not Found"},
		{method: "DELETE", path: "/ping", status: 405, body: "405 Method Not Allowed", header: [2]string{"Allow", "GET"}},
	}
}

// setupUseMounts852 mounts every path-serving middleware with Use, behind a
// global middleware that marks each response it sees, and optionally an
// etag middleware (a buffering one) in front and a custom NotFound handler.
func setupUseMounts852(withETag, customNotFound bool) func(*celeris.Server) {
	return func(s *celeris.Server) {
		s.Use(func(c *celeris.Context) error {
			c.SetHeader("x-global-852", "ran")
			return c.Next()
		})
		if withETag {
			s.Use(etag.New())
		}
		s.Use(
			swagger.New(swagger.Config{SpecContent: []byte(spec852)}),
			pprof.New(),
			debug.New(debug.Config{Server: s}),
			healthcheck.New(),
			static.New(static.Config{FS: fstest.MapFS{"hello.txt": {Data: []byte("hello 852")}}, Prefix: "/static"}),
		)
		if customNotFound {
			s.NotFound(func(c *celeris.Context) error { return c.String(404, "404 Not Found (custom)") })
		}
		s.GET("/ping", func(c *celeris.Context) error { return c.String(200, "pong") })
	}
}

// checkUseMounts852 starts a server on eng in each setup and sends every
// case over one keep-alive connection.
func checkUseMounts852(t *testing.T, eng celeris.EngineType) {
	for _, v := range []struct {
		name                   string
		withETag, customNotFnd bool
	}{
		{"plain", false, false},
		{"custom-notfound", false, true},
		{"etag-buffered+custom-notfound", true, true},
	} {
		t.Run(v.name, func(t *testing.T) {
			addr := startServer852(t, eng, setupUseMounts852(v.withETag, v.customNotFnd))
			tr := &http.Transport{MaxConnsPerHost: 1, DisableCompression: true}
			cl := &http.Client{Timeout: 10 * time.Second, Transport: tr,
				CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			defer cl.CloseIdleConnections()
			for _, rq := range useMountCases852() {
				desc := rq.method + " " + rq.path
				req, err := http.NewRequest(rq.method, "http://"+addr+rq.path, nil)
				if err != nil {
					t.Fatal(err)
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
				if resp.StatusCode != rq.status || !strings.Contains(string(b), rq.body) {
					t.Errorf("%s: %d %.80q, want %d with %q", desc, resp.StatusCode, b, rq.status, rq.body)
				}
				if g := resp.Header.Values("X-Global-852"); len(g) != 1 || g[0] != "ran" {
					t.Errorf("%s: X-Global-852 %v: the response did not go through the global middleware", desc, g)
				}
				if rq.header[0] != "" {
					got := resp.Header.Values(rq.header[0])
					if len(got) != 1 || !strings.Contains(got[0], rq.header[1]) || strings.Contains(got[0], rq.method) {
						t.Errorf("%s: %s %v, want one header listing %s and not %s", desc, rq.header[0], got, rq.header[1], rq.method)
					}
				}
			}
		})
	}
}

// TestUseMountedMiddlewaresAnswerTheirPaths852 runs the check on the std
// engine on every OS; unmatched_mounts_852_linux_test.go adds the native
// engines.
func TestUseMountedMiddlewaresAnswerTheirPaths852(t *testing.T) {
	checkUseMounts852(t, celeris.Std)
}

// startServer852 starts a server on a free port after setup, which may call
// Server.Use, and stops it at cleanup. A start that fails with ENOMEM (an
// io_uring ring whose locked memory another test binary still holds) is
// retried for up to 30 s.
func startServer852(t *testing.T, eng celeris.EngineType, setup func(*celeris.Server)) string {
	t.Helper()
	retryUntil := time.Now().Add(30 * time.Second)
	for tries := 1; ; tries++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		s := celeris.New(celeris.Config{Engine: eng})
		setup(s)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- s.StartWithListenerAndContext(ctx, ln) }()
		addr, err := waitReady852(s, done)
		if err == nil {
			t.Cleanup(func() {
				cancel()
				select {
				case <-done:
				case <-time.After(15 * time.Second):
					t.Errorf("server did not stop within 15s")
				}
			})
			return addr
		}
		cancel()
		<-done
		if strings.Contains(err.Error(), "cannot allocate memory") && time.Now().Before(retryUntil) {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		t.Fatalf("server did not start (try %d): %v", tries, err)
	}
}

func waitReady852(s *celeris.Server, done <-chan error) (string, error) {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if a := s.Addr(); a != nil {
			if c, err := net.DialTimeout("tcp", a.String(), 100*time.Millisecond); err == nil {
				_ = c.Close()
				return a.String(), nil
			}
		}
		select {
		case err := <-done:
			if err == nil {
				err = errors.New("Start returned nil before the server was ready")
			}
			return "", err
		case <-time.After(20 * time.Millisecond):
		}
	}
	return "", errors.New("server not ready within 10s")
}
