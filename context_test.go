package celeris

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/goceleris/celeris/protocol/h2/stream"

	"golang.org/x/net/http2"
)

type mockResponseWriter struct {
	status  int
	headers [][2]string
	body    []byte
}

func (m *mockResponseWriter) WriteResponse(_ *stream.Stream, status int, headers [][2]string, body []byte) error {
	m.status = status
	m.headers = make([][2]string, len(headers))
	copy(m.headers, headers)
	m.body = make([]byte, len(body))
	copy(m.body, body)
	return nil
}
func (m *mockResponseWriter) SendGoAway(_ uint32, _ http2.ErrCode, _ []byte) error { return nil }
func (m *mockResponseWriter) MarkStreamClosed(_ uint32)                            {}
func (m *mockResponseWriter) IsStreamClosed(_ uint32) bool                         { return false }
func (m *mockResponseWriter) WriteRSTStreamPriority(_ uint32, _ http2.ErrCode) error {
	return nil
}
func (m *mockResponseWriter) CloseConn() error { return nil }

func newTestStream(method, path string) (*stream.Stream, *mockResponseWriter) {
	s := stream.NewStream(1)
	s.Headers = [][2]string{
		{":method", method},
		{":path", path},
		{":scheme", "http"},
		{":authority", "localhost"},
	}
	rw := &mockResponseWriter{}
	s.ResponseWriter = rw
	return s, rw
}

type mockHijackerResponseWriter struct {
	mockResponseWriter
	hijackFn func(*stream.Stream) (net.Conn, error)
}

func (m *mockHijackerResponseWriter) Hijack(s *stream.Stream) (net.Conn, error) {
	return m.hijackFn(s)
}

var _ stream.Hijacker = (*mockHijackerResponseWriter)(nil)

func TestContextAcquireRelease(t *testing.T) {
	s, _ := newTestStream("GET", "/test?foo=bar")
	defer s.Release()

	c := acquireContext(s)
	if c.Method() != "GET" {
		t.Fatalf("expected GET, got %s", c.Method())
	}
	if c.Path() != "/test" {
		t.Fatalf("expected /test, got %s", c.Path())
	}
	if c.Query("foo") != "bar" {
		t.Fatalf("expected bar, got %s", c.Query("foo"))
	}
	releaseContext(c)

	if c.method != "" {
		t.Fatal("expected reset after release")
	}
}

func TestContextAbort(t *testing.T) {
	s, _ := newTestStream("GET", "/abort")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	secondCalled := false
	c.handlers = []HandlerFunc{
		func(c *Context) error {
			c.Abort()
			return nil
		},
		func(_ *Context) error {
			secondCalled = true
			return nil
		},
	}
	_ = c.Next()

	if secondCalled {
		t.Fatal("second handler should not have been called after abort")
	}
	if !c.IsAborted() {
		t.Fatal("expected aborted")
	}
}

func TestContextNext(t *testing.T) {
	s, _ := newTestStream("GET", "/next")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	order := ""
	c.handlers = []HandlerFunc{
		func(c *Context) error {
			order += "1"
			_ = c.Next()
			order += "3"
			return nil
		},
		func(_ *Context) error {
			order += "2"
			return nil
		},
	}
	_ = c.Next()

	if order != "123" {
		t.Fatalf("expected order 123, got %s", order)
	}
}

func TestContextSetGet(t *testing.T) {
	s, _ := newTestStream("GET", "/kv")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	_, ok := c.Get("key")
	if ok {
		t.Fatal("expected not found")
	}

	c.Set("key", "value")
	v, ok := c.Get("key")
	if !ok || v != "value" {
		t.Fatalf("expected value, got %v", v)
	}
}

func TestContextContext(t *testing.T) {
	s, _ := newTestStream("GET", "/ctx")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	// Default context should be non-nil.
	ctx := c.Context()
	if ctx == nil {
		t.Fatal("expected non-nil context")
	}

	// SetContext should override.
	type ctxKey struct{}
	custom := context.WithValue(context.Background(), ctxKey{}, "test-value")
	c.SetContext(custom)

	got := c.Context()
	if got != custom {
		t.Fatal("expected custom context")
	}
	if got.Value(ctxKey{}) != "test-value" {
		t.Fatal("expected context value")
	}
}

func TestContextContextResetOnRelease(t *testing.T) {
	s, _ := newTestStream("GET", "/ctx-reset")
	defer s.Release()

	c := acquireContext(s)
	type ctxKey struct{}
	c.SetContext(context.WithValue(context.Background(), ctxKey{}, "val"))
	releaseContext(c)

	if c.ctx != nil {
		t.Fatal("expected ctx to be nil after release")
	}
}

func TestContextFullPath(t *testing.T) {
	s, _ := newTestStream("GET", "/users/42")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	// Not set yet.
	if c.FullPath() != "" {
		t.Fatalf("expected empty fullPath, got %q", c.FullPath())
	}

	c.fullPath = "/users/:id"
	if c.FullPath() != "/users/:id" {
		t.Fatalf("expected /users/:id, got %q", c.FullPath())
	}
}

func TestContextFullPathResetOnRelease(t *testing.T) {
	s, _ := newTestStream("GET", "/test")
	defer s.Release()

	c := acquireContext(s)
	c.fullPath = "/test"
	releaseContext(c)

	if c.fullPath != "" {
		t.Fatal("expected fullPath to be empty after release")
	}
}

func TestContextAddParam(t *testing.T) {
	s, _ := newTestStream("GET", "/users/42")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	c.params = append(c.params, Param{Key: "id", Value: "42"})
	if c.Param("id") != "42" {
		t.Fatalf("expected 42, got %s", c.Param("id"))
	}
}

func TestContextNoDeadlineFromWriteTimeout(t *testing.T) {
	// WriteTimeout is enforced at the engine level via timer wheel, not
	// via context.WithTimeout. The handler's context should NOT have a deadline.
	s := New(Config{WriteTimeout: 5 * time.Second})
	var hadDeadline bool
	s.GET("/test", func(c *Context) error {
		_, ok := c.Context().Deadline()
		hadDeadline = ok
		return c.String(200, "ok")
	})

	adapter := &routerAdapter{server: s}
	st, _ := newTestStream("GET", "/test")
	if err := adapter.HandleStream(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if hadDeadline {
		t.Fatal("expected no context deadline — WriteTimeout is engine-level")
	}
	st.Release()
}

func TestContextNoDeadlineWithoutTimeout(t *testing.T) {
	s := New(Config{})
	var hadDeadline bool
	s.GET("/test", func(c *Context) error {
		_, ok := c.Context().Deadline()
		hadDeadline = ok
		return c.String(200, "ok")
	})

	adapter := &routerAdapter{server: s}
	st, _ := newTestStream("GET", "/test")
	if err := adapter.HandleStream(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if hadDeadline {
		t.Fatal("expected no deadline without WriteTimeout")
	}
	st.Release()
}

func TestContextKeys(t *testing.T) {
	s, _ := newTestStream("GET", "/test")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	if c.Keys() != nil {
		t.Fatal("expected nil for empty keys")
	}

	c.Set("user", "alice")
	c.Set("role", "admin")

	keys := c.Keys()
	if len(keys) != 2 {
		t.Fatalf("expected 2 keys, got %d", len(keys))
	}
	if keys["user"] != "alice" || keys["role"] != "admin" {
		t.Fatalf("unexpected keys: %v", keys)
	}

	// Verify it's a copy — mutating returned map doesn't affect context.
	keys["user"] = "modified"
	v, _ := c.Get("user")
	if v != "alice" {
		t.Fatal("Keys() should return a copy")
	}
}

func TestHandleUnmatchedFallback(t *testing.T) {
	// Custom 404 handler that returns nil without writing should fall back
	// to default response.
	s, rw := newTestStream("GET", "/nonexistent")
	defer s.Release()

	srv := New(Config{Addr: ":0"})
	srv.NotFound(func(_ *Context) error {
		// deliberately don't write anything
		return nil
	})
	srv.GET("/other", func(c *Context) error { return c.String(200, "ok") })

	adapter := &routerAdapter{server: srv}
	_ = adapter.HandleStream(context.Background(), s)

	if rw.status != 404 {
		t.Fatalf("expected 404 fallback response, got %d", rw.status)
	}
}
