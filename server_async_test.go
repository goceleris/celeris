package celeris

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// startMixedAsyncServer starts a std-engine server on a random loopback
// port with a mix of sync and async routes, returning the base URL and a
// shutdown func. The std engine runs every request on a net/http goroutine,
// so .Async() is a documented no-op there — this test proves the per-route
// API is wired end-to-end and harmless (correct responses) regardless of
// the flag. The dispatch differential itself is covered by the H2 processor
// tests (per-stream) and the probatorium cluster matrix (iouring/epoll).
func startMixedAsyncServer(t *testing.T, cfg Config) (string, func()) {
	t.Helper()
	cfg.Engine = Std
	s := New(cfg)

	// Sync route (inherits server default when AsyncHandlers=false).
	s.GET("/cpu", func(c *Context) error { return c.String(http.StatusOK, "cpu") })
	// Explicit async route.
	s.GET("/db", func(c *Context) error { return c.String(http.StatusOK, "db") }).Async()
	// Explicit sync override (only meaningful when default is async).
	s.GET("/cached", func(c *Context) error { return c.String(http.StatusOK, "cached") }).Async(false)
	// Param + async.
	s.GET("/users/:id", func(c *Context) error {
		return c.String(http.StatusOK, "user:%s", c.Param("id"))
	}).Async()
	// Group async + per-route override inside it.
	api := s.Group("/api").Async()
	api.GET("/products", func(c *Context) error { return c.String(http.StatusOK, "products") })
	api.GET("/static", func(c *Context) error { return c.String(http.StatusOK, "static") }).Async(false)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = s.StartWithListener(ln) }()

	base := "http://" + ln.Addr().String()
	// Wait until the server answers.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/cpu")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return base, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	}
}

func getBody(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestServerMixedAsync_CorrectResponses verifies every route shape
// (sync, async, sync-override, param-async, group-async, group-override)
// returns the right body on a sync-default server.
func TestServerMixedAsync_CorrectResponses(t *testing.T) {
	base, stop := startMixedAsyncServer(t, Config{AsyncHandlers: false})
	defer stop()

	cases := []struct{ path, want string }{
		{"/cpu", "cpu"},
		{"/db", "db"},
		{"/cached", "cached"},
		{"/users/42", "user:42"},
		{"/api/products", "products"},
		{"/api/static", "static"},
	}
	for _, tc := range cases {
		code, body := getBody(t, base+tc.path)
		if code != http.StatusOK {
			t.Errorf("%s: status %d, want 200", tc.path, code)
		}
		if body != tc.want {
			t.Errorf("%s: body %q, want %q", tc.path, body, tc.want)
		}
	}
}

// TestServerMixedAsync_AsyncDefault verifies the same route surface on an
// async-default server (Config.AsyncHandlers=true) still returns correct
// responses, including the .Async(false) sync overrides.
func TestServerMixedAsync_AsyncDefault(t *testing.T) {
	base, stop := startMixedAsyncServer(t, Config{AsyncHandlers: true})
	defer stop()

	for _, tc := range []struct{ path, want string }{
		{"/cpu", "cpu"}, {"/db", "db"}, {"/cached", "cached"},
		{"/users/7", "user:7"}, {"/api/products", "products"}, {"/api/static", "static"},
	} {
		code, body := getBody(t, base+tc.path)
		if code != http.StatusOK || body != tc.want {
			t.Errorf("%s: got (%d,%q), want (200,%q)", tc.path, code, body, tc.want)
		}
	}
}

// TestServerMixedAsync_ConcurrentNoCrosstalk hammers sync + async routes
// concurrently on the same server and asserts no response cross-talk
// (every request gets exactly its route's body). Guards against shared
// per-request state bleeding across the sync/async dispatch boundary.
func TestServerMixedAsync_ConcurrentNoCrosstalk(t *testing.T) {
	base, stop := startMixedAsyncServer(t, Config{AsyncHandlers: false})
	defer stop()

	routes := []struct{ path, want string }{
		{"/cpu", "cpu"}, {"/db", "db"}, {"/users/99", "user:99"},
		{"/api/products", "products"}, {"/api/static", "static"},
	}
	var wg sync.WaitGroup
	var mismatches atomic.Int64
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tc := routes[i%len(routes)]
			code, body := getBody(t, base+tc.path)
			if code != http.StatusOK || body != tc.want {
				mismatches.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if n := mismatches.Load(); n != 0 {
		t.Fatalf("%d concurrent responses mismatched their route", n)
	}
}

// TestServerMixedAsync_KeepAliveMixedRoutes reuses a single keep-alive
// connection across sync and async routes (the case that, on iouring/epoll,
// exercises a conn touching both dispatch modes) and asserts ordering +
// correctness.
func TestServerMixedAsync_KeepAliveMixedRoutes(t *testing.T) {
	base, stop := startMixedAsyncServer(t, Config{AsyncHandlers: false})
	defer stop()

	// Single connection, disable keep-alive pooling churn.
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 1}}
	seq := []struct{ path, want string }{
		{"/cpu", "cpu"}, {"/db", "db"}, {"/cpu", "cpu"},
		{"/users/1", "user:1"}, {"/api/static", "static"}, {"/api/products", "products"},
	}
	for i := 0; i < 3; i++ {
		for _, tc := range seq {
			resp, err := client.Get(base + tc.path)
			if err != nil {
				t.Fatalf("iter %d %s: %v", i, tc.path, err)
			}
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK || string(b) != tc.want {
				t.Fatalf("iter %d %s: got (%d,%q), want (200,%q)", i, tc.path, resp.StatusCode, string(b), tc.want)
			}
		}
	}
	_ = fmt.Sprint
}
