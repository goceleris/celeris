package celeris

import (
	"context"
	"testing"
)

// Tests for celeris#421: a HEAD request to a path that has a GET route and no
// HEAD route is answered by the GET route (the engine drops the body; RFC 9110
// §9.3.2), and an OPTIONS request to a path that has any route and no OPTIONS
// route is answered 200 with an Allow header listing every method the path
// answers, Content-Length: 0 (§9.3.7, §10.2.1). Explicit HEAD and OPTIONS
// routes still win, a method the path does not answer still gets 405, and its
// Allow header now lists HEAD and OPTIONS too.

type resp421 struct {
	status  int
	headers [][2]string
	body    string
}

func (r resp421) header(k string) string {
	for _, h := range r.headers {
		if h[0] == k {
			return h[1]
		}
	}
	return ""
}

func (r resp421) headerCount(k string) int {
	n := 0
	for _, h := range r.headers {
		if h[0] == k {
			n++
		}
	}
	return n
}

func serve421(t *testing.T, s *Server, method, path string, hdrs ...[2]string) resp421 {
	t.Helper()
	st, rw := newTestStream(method, path)
	st.Headers = append(st.Headers, hdrs...)
	// A bare adapter, as the other router tests use; the chains doPrepare
	// builds are exercised on real engines in
	// TestAutoHeadOptionsOnEveryEngine421.
	if err := (&routerAdapter{server: s}).HandleStream(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	st.Release()
	return resp421{rw.status, rw.headers, string(rw.body)}
}

func newServer421() *Server {
	s := New(Config{})
	s.GET("/r", func(c *Context) error {
		c.SetHeader("x-method-seen", c.Method())
		return c.String(200, "hello")
	})
	s.POST("/r", func(c *Context) error { return c.String(201, "made") })
	s.GET("/u/:id", func(c *Context) error { return c.String(200, "user %s", c.Param("id")) })
	s.GET("/files/*path", func(c *Context) error { return c.String(200, "file %s", c.Param("path")) })
	s.POST("/only-post", func(c *Context) error { return c.String(200, "posted") })
	s.GET("/x", func(c *Context) error { return c.String(200, "get-x") })
	s.HEAD("/x", func(c *Context) error {
		c.SetHeader("x-explicit-head", "1")
		return c.NoContent(204)
	})
	s.OPTIONS("/o", func(c *Context) error {
		c.SetHeader("x-explicit-options", "1")
		return c.NoContent(204)
	})
	s.GET("/o", func(c *Context) error { return c.String(200, "get-o") })
	s.Handle("PROPFIND", "/dav", func(c *Context) error { return c.String(207, "multi") })
	s.GET("/dav", func(c *Context) error { return c.String(200, "dav") })
	g := s.Group("/api", func(c *Context) error {
		c.SetHeader("x-group-mw", "1")
		return c.Next()
	})
	g.GET("/v", func(c *Context) error { return c.String(200, "v1") })
	s.GET("/async", func(c *Context) error { return c.String(200, "a") }).Async()
	return s
}

func TestAutoHead421(t *testing.T) {
	s := newServer421()
	cases := []struct {
		name, path string
		status     int
		body       string
		check      func(t *testing.T, r resp421)
	}{
		{"static-route", "/r", 200, "hello", func(t *testing.T, r resp421) {
			if got := r.header("x-method-seen"); got != "HEAD" {
				t.Fatalf("the GET handler saw method %q, want HEAD", got)
			}
			if got := r.header("content-length"); got != "5" {
				t.Fatalf("content-length %q, want the GET body's 5", got)
			}
		}},
		{"param-route", "/u/42", 200, "user 42", nil},
		{"catch-all-route", "/files/a/b.txt", 200, "file /a/b.txt", nil},
		{"group-route-runs-group-middleware", "/api/v", 200, "v1", func(t *testing.T, r resp421) {
			if r.header("x-group-mw") != "1" {
				t.Fatal("group middleware did not run for HEAD")
			}
		}},
		{"explicit-head-wins", "/x", 204, "", func(t *testing.T, r resp421) {
			if r.header("x-explicit-head") != "1" {
				t.Fatal("explicit HEAD handler did not run")
			}
		}},
		{"post-only-is-405", "/only-post", 405, "405 Method Not Allowed", func(t *testing.T, r resp421) {
			if got := r.header("allow"); got != "POST, OPTIONS" {
				t.Fatalf("allow %q, want %q", got, "POST, OPTIONS")
			}
		}},
		{"unknown-path-is-404", "/nope", 404, "404 Not Found", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := serve421(t, s, "HEAD", tc.path)
			// The mock writer records the body the handler produced; the
			// engines drop it for HEAD (TestAutoHeadOptionsOnEveryEngine421).
			if r.status != tc.status || r.body != tc.body {
				t.Fatalf("HEAD %s: %d %q, want %d %q", tc.path, r.status, r.body, tc.status, tc.body)
			}
			if tc.check != nil {
				tc.check(t, r)
			}
		})
	}
	t.Run("async-flag-follows-the-get-route", func(t *testing.T) {
		if !s.router.routeAsync("GET", "/async") {
			t.Fatal("fixture: GET /async is not async")
		}
		if !s.router.routeAsync("HEAD", "/async") {
			t.Fatal("HEAD /async resolves to a sync dispatch; the GET route it runs is async")
		}
		if s.router.routeAsync("HEAD", "/r") {
			t.Fatal("HEAD /r resolves to async; GET /r is sync")
		}
	})
}

func TestAutoOptions421(t *testing.T) {
	s := newServer421()
	cases := []struct {
		name, path string
		status     int
		allow      string
		check      func(t *testing.T, r resp421)
	}{
		{"get-and-post", "/r", 200, "GET, POST, HEAD, OPTIONS", nil},
		{"param-route", "/u/7", 200, "GET, HEAD, OPTIONS", nil},
		{"post-only", "/only-post", 200, "POST, OPTIONS", nil},
		{"explicit-head-listed-once", "/x", 200, "GET, HEAD, OPTIONS", nil},
		{"custom-method", "/dav", 200, "GET, HEAD, OPTIONS, PROPFIND", nil},
		{"explicit-options-wins", "/o", 204, "", func(t *testing.T, r resp421) {
			if r.header("x-explicit-options") != "1" {
				t.Fatal("explicit OPTIONS handler did not run")
			}
		}},
		{"unknown-path-is-404", "/nope", 404, "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := serve421(t, s, "OPTIONS", tc.path)
			if r.status != tc.status || r.header("allow") != tc.allow {
				t.Fatalf("OPTIONS %s: %d allow %q, want %d allow %q", tc.path, r.status, r.header("allow"), tc.status, tc.allow)
			}
			if tc.status == 200 {
				if r.body != "" || r.header("content-length") != "0" || r.headerCount("content-length") != 1 {
					t.Fatalf("OPTIONS %s: body %q content-length %q (x%d), want empty and exactly one \"0\"",
						tc.path, r.body, r.header("content-length"), r.headerCount("content-length"))
				}
				if r.headerCount("allow") != 1 {
					t.Fatalf("OPTIONS %s: %d allow headers", tc.path, r.headerCount("allow"))
				}
			}
			if tc.check != nil {
				tc.check(t, r)
			}
		})
	}
}

func TestAutoOptionsRunsGlobalMiddleware421(t *testing.T) {
	s := New(Config{})
	s.Use(func(c *Context) error {
		c.SetHeader("x-global", "1")
		// A preflight-style middleware answers OPTIONS itself.
		if c.Method() == "OPTIONS" && c.Header("access-control-request-method") != "" {
			c.SetHeader("access-control-allow-methods", "GET")
			return c.NoContent(204)
		}
		return c.Next()
	})
	s.GET("/r", func(c *Context) error { return c.String(200, "hello") })

	r := serve421(t, s, "OPTIONS", "/r")
	if r.status != 200 || r.header("x-global") != "1" || r.header("allow") != "GET, HEAD, OPTIONS" {
		t.Fatalf("plain OPTIONS: %d x-global %q allow %q", r.status, r.header("x-global"), r.header("allow"))
	}
	r = serve421(t, s, "OPTIONS", "/r", [2]string{"origin", "https://a.example"}, [2]string{"access-control-request-method", "GET"})
	if r.status != 204 || r.header("access-control-allow-methods") != "GET" {
		t.Fatalf("preflight: %d access-control-allow-methods %q, want the middleware's 204", r.status, r.header("access-control-allow-methods"))
	}
}

func TestMethodNotAllowed421(t *testing.T) {
	s := newServer421()
	r := serve421(t, s, "DELETE", "/r")
	if r.status != 405 || r.header("allow") != "GET, POST, HEAD, OPTIONS" {
		t.Fatalf("DELETE /r: %d allow %q, want 405 %q", r.status, r.header("allow"), "GET, POST, HEAD, OPTIONS")
	}
	r = serve421(t, s, "PUT", "/x")
	if r.status != 405 || r.header("allow") != "GET, HEAD, OPTIONS" {
		t.Fatalf("PUT /x: %d allow %q", r.status, r.header("allow"))
	}

	// A custom 405 handler still answers what the path does not answer,
	// and no longer sees HEAD (GET route) or OPTIONS.
	s2 := New(Config{})
	s2.GET("/r", func(c *Context) error { return c.String(200, "hello") })
	s2.MethodNotAllowed(func(c *Context) error { return c.String(405, "custom") })
	if r := serve421(t, s2, "DELETE", "/r"); r.status != 405 || r.body != "custom" || r.header("allow") != "GET, HEAD, OPTIONS" {
		t.Fatalf("custom 405: %d %q allow %q", r.status, r.body, r.header("allow"))
	}
	if r := serve421(t, s2, "HEAD", "/r"); r.status != 200 {
		t.Fatalf("HEAD with a custom 405 handler: %d", r.status)
	}
	if r := serve421(t, s2, "OPTIONS", "/r"); r.status != 200 || r.header("allow") != "GET, HEAD, OPTIONS" {
		t.Fatalf("OPTIONS with a custom 405 handler: %d allow %q", r.status, r.header("allow"))
	}
}
