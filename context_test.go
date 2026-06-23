package celeris

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/goceleris/celeris/protocol/h2/stream"
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

// TestContextResetWithOverflowedRespHeaders locks in the overflow-safe
// reset path. Pre-fix (probatorium nightly 25993346060 root cause) a
// middleware stack that emitted ≥ 17 response headers (kitchen_sink:
// recovery + requestid + secure × 8 + cors + ratelimit × 3 + etag +
// cache + x-cache + x-request-id) caused c.reset() to slice the
// fixed-size respHdrBuf beyond its capacity:
//
//	clear(c.respHdrBuf[:n])  →  panic: runtime error: slice bounds
//	                            out of range [:n] with length len(respHdrBuf)
//
// On the iouring/epoll engines the panic propagated through
// recoverAndRelease AFTER WriteResponse had queued bytes into the
// per-conn writeBuf but BEFORE the worker's flushSend pushed those
// bytes to the socket — the client saw zero bytes, the connection
// stalled, the validator's walker timed out. std-engine escaped
// the wire-visible damage because net/http writes inline.
func TestContextResetWithOverflowedRespHeaders(t *testing.T) {
	s, _ := newTestStream("GET", "/test")
	defer s.Release()

	c := acquireContext(s)
	// Push 28 headers — overflows respHdrBuf (24 slots) and forces
	// append() to allocate a new backing array.
	const nHdr = 28
	for i := 0; i < nHdr; i++ {
		c.SetHeader("x-test-"+strconv.Itoa(i), "v")
	}
	if got := len(c.respHeaders); got != nHdr {
		t.Fatalf("expected %d headers staged, got %d", nHdr, got)
	}

	// Pre-fix this would panic with "slice bounds out of range
	// [:28] with length 24". Recover so the test framework reports
	// the panic clearly rather than crashing the whole process.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("reset panicked with overflowed respHeaders: %v", r)
		}
	}()
	releaseContext(c)

	// After release, respHeaders is reset back to respHdrBuf[:0].
	if cap(c.respHeaders) < len(c.respHdrBuf) {
		t.Fatalf("respHeaders capacity collapsed below respHdrBuf size: cap=%d", cap(c.respHeaders))
	}
}

// TestContextRespHeaderOverflowReuseZeroAlloc locks in celeris#360: once a
// middleware stack pushes respHeaders past the inline respHdrBuf, the grown
// heap backing array is retained as respHdrScratch and reused on every
// subsequent request — so an overflow route allocates the backing array ONCE
// per pooled Context, not once per request.
func TestContextRespHeaderOverflowReuseZeroAlloc(t *testing.T) {
	s, _ := newTestStream("GET", "/test")
	defer s.Release()
	c := acquireContext(s)
	defer releaseContext(c)

	// 25 clean lowercase headers > respHdrBuf's 24 slots → forces the heap path.
	hdrs := [][2]string{
		{"h00", "v"}, {"h01", "v"}, {"h02", "v"}, {"h03", "v"}, {"h04", "v"},
		{"h05", "v"}, {"h06", "v"}, {"h07", "v"}, {"h08", "v"}, {"h09", "v"},
		{"h10", "v"}, {"h11", "v"}, {"h12", "v"}, {"h13", "v"}, {"h14", "v"},
		{"h15", "v"}, {"h16", "v"}, {"h17", "v"}, {"h18", "v"}, {"h19", "v"},
		{"h20", "v"}, {"h21", "v"}, {"h22", "v"}, {"h23", "v"}, {"h24", "v"},
	}
	avg := testing.AllocsPerRun(500, func() {
		for _, h := range hdrs {
			c.SetHeader(h[0], h[1])
		}
		c.reset()
	})
	if avg != 0 {
		t.Fatalf("overflowed respHeaders must reuse the scratch backing array: got %.2f allocs/op, want 0", avg)
	}
	if cap(c.respHdrScratch) < len(hdrs) {
		t.Fatalf("respHdrScratch should retain a >=%d cap backing array, got cap=%d", len(hdrs), cap(c.respHdrScratch))
	}
}

// nopRW is a no-op ResponseWriter so an alloc test can isolate the Context path
// from the mock writer's own header/body copies.
type nopRW struct{}

func (nopRW) WriteResponse(_ *stream.Stream, _ int, _ [][2]string, _ []byte) error { return nil }

// TestContextBlobManyHeadersZeroAlloc locks in the chain-fullstack fix: when a
// response carries more than the inline-buffer's headers (25 user + content-type
// + content-length = 27 > 24), Blob must reuse blobHdrScratch instead of
// allocating make([][2]string,0,total) every request — that was the dominant
// per-request allocation (≈77% of chain-fullstack allocs → GC pressure).
func TestContextBlobManyHeadersZeroAlloc(t *testing.T) {
	s, _ := newTestStream("GET", "/test")
	defer s.Release()
	s.ResponseWriter = nopRW{} // the default mock writer allocates; isolate Blob
	c := acquireContext(s)
	defer releaseContext(c)

	// 25 user headers ⇒ total 27 > respHdrBuf's 24 ⇒ Blob's many-header path.
	const nUser = 25
	for i := 0; i < nUser; i++ {
		c.SetHeader("h"+strconv.Itoa(i), "v")
	}
	body := []byte("hello")
	avg := testing.AllocsPerRun(500, func() {
		c.written = false
		_ = c.Blob(200, "application/json", body)
	})
	if avg != 0 {
		t.Fatalf("Blob many-header path must reuse blobHdrScratch: got %.2f allocs/op, want 0", avg)
	}
	if cap(c.blobHdrScratch) < nUser+2 {
		t.Fatalf("blobHdrScratch should retain a >=%d cap backing array, got cap=%d", nUser+2, cap(c.blobHdrScratch))
	}
}

// TestContextFullStackHeadersInlineNoScratch locks in the B4 respHdrBuf bump:
// a fullstack-volume response (18 user headers + content-type + content-length
// = 20) now fits the inline respHdrBuf, so neither the respHdrScratch (SetHeader
// overflow) nor the blobHdrScratch (Blob overflow) heap backing array is ever
// allocated for it. With the old 16-slot buffer both were forced into being on
// the first such request per pooled Context; 24 slots absorb the chain inline.
func TestContextFullStackHeadersInlineNoScratch(t *testing.T) {
	s, _ := newTestStream("GET", "/test")
	defer s.Release()
	s.ResponseWriter = nopRW{}
	c := acquireContext(s)
	defer releaseContext(c)
	// Start from a pristine inline buffer: a pooled Context may carry a warm
	// scratch array from a prior test, which would mask the buffer-size check.
	c.respHeaders = c.respHdrBuf[:0]
	c.respHdrScratch = nil
	c.blobHdrScratch = nil

	for i := 0; i < 18; i++ {
		c.SetHeaderTrust("h"+strconv.Itoa(i), "v")
	}
	if got, want := len(c.respHeaders), 18; got != want {
		t.Fatalf("expected %d staged headers, got %d", want, got)
	}
	// 18 user headers must not have grown respHeaders off the inline buffer.
	if cap(c.respHeaders) > len(c.respHdrBuf) {
		t.Fatalf("18 headers escaped the inline respHdrBuf: cap=%d > %d", cap(c.respHeaders), len(c.respHdrBuf))
	}
	if c.respHdrScratch != nil {
		t.Fatal("respHdrScratch allocated for a 18-header response that fits inline")
	}
	if err := c.Blob(200, "application/json", []byte("hello")); err != nil {
		t.Fatalf("Blob: %v", err)
	}
	// 20 total headers (18 + content-type + content-length) must not have hit
	// Blob's many-header scratch path.
	if c.blobHdrScratch != nil {
		t.Fatal("blobHdrScratch allocated for a 20-header response that fits inline")
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

func TestOnReleaseFiresOnRelease(t *testing.T) {
	s, _ := newTestStream("GET", "/release")
	defer s.Release()

	c := acquireContext(s)
	var fired bool
	c.OnRelease(func() { fired = true })
	releaseContext(c)

	if !fired {
		t.Fatal("expected OnRelease callback to fire during releaseContext")
	}
}

func TestOnReleaseLIFOOrder(t *testing.T) {
	s, _ := newTestStream("GET", "/lifo")
	defer s.Release()

	c := acquireContext(s)
	var order []int
	c.OnRelease(func() { order = append(order, 1) })
	c.OnRelease(func() { order = append(order, 2) })
	c.OnRelease(func() { order = append(order, 3) })
	releaseContext(c)

	if len(order) != 3 || order[0] != 3 || order[1] != 2 || order[2] != 1 {
		t.Fatalf("expected LIFO order [3 2 1], got %v", order)
	}
}

func TestOnReleasePanicRecovery(t *testing.T) {
	s, _ := newTestStream("GET", "/panic-release")
	defer s.Release()

	c := acquireContext(s)
	var normalFired bool
	c.OnRelease(func() { normalFired = true })
	c.OnRelease(func() { panic("boom") })
	releaseContext(c)

	if !normalFired {
		t.Fatal("expected normal callback to fire even after panic in another")
	}
}

func TestOnReleaseResetClearsCallbacks(t *testing.T) {
	s, _ := newTestStream("GET", "/clear")
	defer s.Release()

	c := acquireContext(s)
	callCount := 0
	c.OnRelease(func() { callCount++ })
	releaseContext(c)

	if callCount != 1 {
		t.Fatalf("expected 1 call, got %d", callCount)
	}

	// Re-acquire and release again without registering new callbacks.
	// The callback must NOT fire a second time.
	c2 := acquireContext(s)
	releaseContext(c2)

	if callCount != 1 {
		t.Fatalf("expected callback count to remain 1 after reuse, got %d", callCount)
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

func TestContextStartTime(t *testing.T) {
	s, _ := newTestStream("GET", "/test")
	defer s.Release()

	before := time.Now()
	c := acquireContext(s)
	c.startTime = time.Now()
	after := time.Now()
	defer releaseContext(c)

	st := c.StartTime()
	if st.IsZero() {
		t.Fatal("expected non-zero StartTime")
	}
	if st.Before(before) || st.After(after) {
		t.Fatalf("StartTime %v not within bracket [%v, %v]", st, before, after)
	}
}
