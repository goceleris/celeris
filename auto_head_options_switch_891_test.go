package celeris

import (
	"testing"
)

// Tests for celeris#891 item 26: Config.DisableAutoHEAD and
// Config.DisableAutoOPTIONS switch the automatic answers of celeris#421 off.
// The zero value keeps them on (auto_head_options_421_test.go); with a switch
// set, the request is answered like a method the path does not support, and
// the Allow header lists only what the path answers.

func newServer891(cfg Config) *Server {
	s := New(cfg)
	s.GET("/r", func(c *Context) error {
		c.SetHeader("x-method-seen", c.Method())
		return c.String(200, "hello")
	})
	s.POST("/r", func(c *Context) error { return c.String(201, "made") })
	s.GET("/u/:id", func(c *Context) error { return c.String(200, "user %s", c.Param("id")) })
	s.GET("/x", func(c *Context) error { return c.String(200, "get-x") })
	s.HEAD("/x", func(c *Context) error {
		c.SetHeader("x-explicit-head", "1")
		return c.NoContent(204)
	})
	s.GET("/o", func(c *Context) error { return c.String(200, "get-o") })
	s.OPTIONS("/o", func(c *Context) error {
		c.SetHeader("x-explicit-options", "1")
		return c.NoContent(204)
	})
	s.GET("/async", func(c *Context) error { return c.String(200, "a") }).Async()
	return s
}

func TestDisableAutoHEAD891(t *testing.T) {
	s := newServer891(Config{DisableAutoHEAD: true})
	cases := []struct {
		method, path string
		status       int
		allow        string
	}{
		{"HEAD", "/r", 405, "GET, POST, OPTIONS"},
		{"HEAD", "/u/42", 405, "GET, OPTIONS"},
		{"HEAD", "/x", 204, ""}, // an explicit HEAD route still answers
		{"HEAD", "/nope", 404, ""},
		{"OPTIONS", "/r", 200, "GET, POST, OPTIONS"}, // HEAD is not listed, OPTIONS still is
		{"OPTIONS", "/x", 200, "GET, HEAD, OPTIONS"}, // listed where a HEAD route exists
		{"DELETE", "/r", 405, "GET, POST, OPTIONS"},
		{"GET", "/r", 200, ""},
	}
	for _, tc := range cases {
		r := serve421(t, s, tc.method, tc.path)
		if r.status != tc.status || r.header("allow") != tc.allow {
			t.Errorf("%s %s: %d allow %q, want %d allow %q", tc.method, tc.path, r.status, r.header("allow"), tc.status, tc.allow)
		}
	}
	if r := serve421(t, s, "HEAD", "/r"); r.header("x-method-seen") != "" {
		t.Error("the GET handler ran for a HEAD request")
	}
	// routeAsync must agree with HandleStream: the HEAD does not resolve to
	// the async GET route it no longer runs.
	if s.router.routeAsync("HEAD", "/async") {
		t.Error("HEAD /async resolves to the async GET route although DisableAutoHEAD is set")
	}
	if !s.router.routeAsync("GET", "/async") {
		t.Error("fixture: GET /async is not async")
	}
}

func TestDisableAutoOPTIONS891(t *testing.T) {
	s := newServer891(Config{DisableAutoOPTIONS: true})
	cases := []struct {
		method, path string
		status       int
		allow        string
	}{
		{"OPTIONS", "/r", 405, "GET, POST, HEAD"},
		{"OPTIONS", "/u/42", 405, "GET, HEAD"},
		{"OPTIONS", "/o", 204, ""}, // an explicit OPTIONS route still answers
		{"OPTIONS", "/nope", 404, ""},
		{"HEAD", "/r", 200, ""}, // auto-HEAD is a separate switch
		{"DELETE", "/r", 405, "GET, POST, HEAD"},
		{"DELETE", "/o", 405, "GET, HEAD, OPTIONS"}, // listed where an OPTIONS route exists
	}
	for _, tc := range cases {
		r := serve421(t, s, tc.method, tc.path)
		if r.status != tc.status || r.header("allow") != tc.allow {
			t.Errorf("%s %s: %d allow %q, want %d allow %q", tc.method, tc.path, r.status, r.header("allow"), tc.status, tc.allow)
		}
	}
}

func TestDisableBothAutoAnswers891(t *testing.T) {
	s := newServer891(Config{DisableAutoHEAD: true, DisableAutoOPTIONS: true})
	for _, tc := range []struct {
		method, path string
		status       int
		allow        string
	}{
		{"HEAD", "/r", 405, "GET, POST"},
		{"OPTIONS", "/r", 405, "GET, POST"},
		{"DELETE", "/r", 405, "GET, POST"},
		{"HEAD", "/x", 204, ""},
		{"OPTIONS", "/o", 204, ""},
		{"OPTIONS", "/nope", 404, ""},
	} {
		r := serve421(t, s, tc.method, tc.path)
		if r.status != tc.status || r.header("allow") != tc.allow {
			t.Errorf("%s %s: %d allow %q, want %d allow %q", tc.method, tc.path, r.status, r.header("allow"), tc.status, tc.allow)
		}
	}
}

// With DisableAutoOPTIONS the OPTIONS request takes the 405 path, which runs
// the same global middleware as the automatic answer did, so a CORS preflight
// installed with Server.Use is still answered, and a custom MethodNotAllowed
// handler is the one that answers a plain OPTIONS.
func TestDisableAutoOPTIONSKeepsGlobalMiddlewareAndPreflight891(t *testing.T) {
	s := New(Config{DisableAutoOPTIONS: true})
	s.Use(func(c *Context) error {
		c.SetHeader("x-global", "1")
		if c.Method() == "OPTIONS" && c.Header("access-control-request-method") != "" {
			c.SetHeader("access-control-allow-methods", "GET")
			return c.NoContent(204)
		}
		return c.Next()
	})
	s.GET("/r", func(c *Context) error { return c.String(200, "hello") })
	s.MethodNotAllowed(func(c *Context) error { return c.String(405, "custom") })

	r := serve421(t, s, "OPTIONS", "/r", [2]string{"origin", "https://a.example"}, [2]string{"access-control-request-method", "GET"})
	if r.status != 204 || r.header("access-control-allow-methods") != "GET" || r.header("x-global") != "1" {
		t.Fatalf("preflight: %d access-control-allow-methods %q x-global %q, want the middleware's 204", r.status, r.header("access-control-allow-methods"), r.header("x-global"))
	}
	r = serve421(t, s, "OPTIONS", "/r")
	if r.status != 405 || r.body != "custom" || r.header("allow") != "GET, HEAD" || r.header("x-global") != "1" {
		t.Fatalf("plain OPTIONS: %d %q allow %q x-global %q, want the custom 405 with Allow GET, HEAD", r.status, r.body, r.header("allow"), r.header("x-global"))
	}
}

// The default is unchanged: a zero Config keeps both automatic answers.
func TestAutoHeadOptionsDefaultOn891(t *testing.T) {
	s := newServer891(Config{})
	if r := serve421(t, s, "HEAD", "/r"); r.status != 200 {
		t.Errorf("HEAD /r with the zero Config: %d, want the GET route's 200", r.status)
	}
	if r := serve421(t, s, "OPTIONS", "/r"); r.status != 200 || r.header("allow") != "GET, POST, HEAD, OPTIONS" {
		t.Errorf("OPTIONS /r with the zero Config: %d allow %q", r.status, r.header("allow"))
	}
}
