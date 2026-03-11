package celeris

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	m.headers = headers
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

func TestContextJSON(t *testing.T) {
	s, rw := newTestStream("GET", "/json")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	data := map[string]string{"hello": "world"}
	if err := c.JSON(200, data); err != nil {
		t.Fatal(err)
	}

	if rw.status != 200 {
		t.Fatalf("expected 200, got %d", rw.status)
	}

	var result map[string]string
	if err := json.Unmarshal(rw.body, &result); err != nil {
		t.Fatal(err)
	}
	if result["hello"] != "world" {
		t.Fatalf("expected world, got %s", result["hello"])
	}
}

func TestContextString(t *testing.T) {
	s, rw := newTestStream("GET", "/str")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	if err := c.String(200, "hello %s", "world"); err != nil {
		t.Fatal(err)
	}
	if string(rw.body) != "hello world" {
		t.Fatalf("expected 'hello world', got '%s'", string(rw.body))
	}
}

func TestContextBlob(t *testing.T) {
	s, rw := newTestStream("GET", "/blob")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	if err := c.Blob(201, "application/octet-stream", []byte{0x01, 0x02}); err != nil {
		t.Fatal(err)
	}
	if rw.status != 201 {
		t.Fatalf("expected 201, got %d", rw.status)
	}
	if len(rw.body) != 2 {
		t.Fatalf("expected 2 bytes, got %d", len(rw.body))
	}
}

func TestContextNoContent(t *testing.T) {
	s, rw := newTestStream("DELETE", "/item")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	if err := c.NoContent(204); err != nil {
		t.Fatal(err)
	}
	if rw.status != 204 {
		t.Fatalf("expected 204, got %d", rw.status)
	}
	if len(rw.body) != 0 {
		t.Fatalf("expected empty body, got %d bytes", len(rw.body))
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

func TestContextBind(t *testing.T) {
	s, _ := newTestStream("POST", "/bind")
	s.Data.Write([]byte(`{"name":"test"}`))
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	var v struct{ Name string }
	if err := c.Bind(&v); err != nil {
		t.Fatal(err)
	}
	if v.Name != "test" {
		t.Fatalf("expected test, got %s", v.Name)
	}
}

func TestContextHeader(t *testing.T) {
	s, _ := newTestStream("GET", "/headers")
	s.Headers = append(s.Headers, [2]string{"x-custom", "val"})
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	if c.Header("x-custom") != "val" {
		t.Fatalf("expected val, got %s", c.Header("x-custom"))
	}
	if c.Header("x-missing") != "" {
		t.Fatal("expected empty for missing header")
	}
}

func TestContextParam(t *testing.T) {
	s, _ := newTestStream("GET", "/users/42")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	c.params = Params{{Key: "id", Value: "42"}}
	if c.Param("id") != "42" {
		t.Fatalf("expected 42, got %s", c.Param("id"))
	}
	if c.Param("missing") != "" {
		t.Fatal("expected empty for missing param")
	}
}

func TestContextStatusCode(t *testing.T) {
	s, _ := newTestStream("GET", "/status")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	if c.StatusCode() != 200 {
		t.Fatalf("expected default 200, got %d", c.StatusCode())
	}

	_ = c.String(201, "created")
	if c.StatusCode() != 201 {
		t.Fatalf("expected 201, got %d", c.StatusCode())
	}
}

func TestContextRedirect(t *testing.T) {
	s, rw := newTestStream("GET", "/old")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	if err := c.Redirect(302, "/new"); err != nil {
		t.Fatal(err)
	}

	if rw.status != 302 {
		t.Fatalf("expected 302, got %d", rw.status)
	}

	var found bool
	for _, h := range rw.headers {
		if h[0] == "location" && h[1] == "/new" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected location: /new header")
	}
}

func TestContextCookieRoundTrip(t *testing.T) {
	s, rw := newTestStream("GET", "/cookie")
	s.Headers = append(s.Headers, [2]string{"cookie", "session=abc123; theme=dark"})
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	// Read cookies.
	v, err := c.Cookie("session")
	if err != nil {
		t.Fatal(err)
	}
	if v != "abc123" {
		t.Fatalf("expected abc123, got %s", v)
	}

	v, err = c.Cookie("theme")
	if err != nil {
		t.Fatal(err)
	}
	if v != "dark" {
		t.Fatalf("expected dark, got %s", v)
	}

	_, err = c.Cookie("missing")
	if err != ErrNoCookie {
		t.Fatalf("expected ErrNoCookie, got %v", err)
	}

	// Set cookie.
	c.SetCookie(&Cookie{
		Name:     "token",
		Value:    "xyz",
		Path:     "/api",
		MaxAge:   3600,
		HTTPOnly: true,
		Secure:   true,
	})
	_ = c.NoContent(200)

	var cookieHeader string
	for _, h := range rw.headers {
		if h[0] == "set-cookie" {
			cookieHeader = h[1]
			break
		}
	}
	if cookieHeader == "" {
		t.Fatal("expected set-cookie header")
	}

	expected := "token=xyz; Path=/api; Max-Age=3600; HttpOnly; Secure"
	if cookieHeader != expected {
		t.Fatalf("expected %q, got %q", expected, cookieHeader)
	}
}

func TestContextCookieNoCookieHeader(t *testing.T) {
	s, _ := newTestStream("GET", "/no-cookie")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	_, err := c.Cookie("anything")
	if err != ErrNoCookie {
		t.Fatalf("expected ErrNoCookie, got %v", err)
	}
}

func TestContextClientIP(t *testing.T) {
	tests := []struct {
		name     string
		headers  [][2]string
		expected string
	}{
		{
			name: "x-forwarded-for single",
			headers: [][2]string{
				{":method", "GET"}, {":path", "/ip"},
				{"x-forwarded-for", "1.2.3.4"},
			},
			expected: "1.2.3.4",
		},
		{
			name: "x-forwarded-for multiple",
			headers: [][2]string{
				{":method", "GET"}, {":path", "/ip"},
				{"x-forwarded-for", "1.2.3.4, 5.6.7.8"},
			},
			expected: "1.2.3.4",
		},
		{
			name: "x-real-ip",
			headers: [][2]string{
				{":method", "GET"}, {":path", "/ip"},
				{"x-real-ip", "10.0.0.1"},
			},
			expected: "10.0.0.1",
		},
		{
			name: "x-forwarded-for takes precedence",
			headers: [][2]string{
				{":method", "GET"}, {":path", "/ip"},
				{"x-forwarded-for", "1.2.3.4"},
				{"x-real-ip", "10.0.0.1"},
			},
			expected: "1.2.3.4",
		},
		{
			name: "no proxy headers",
			headers: [][2]string{
				{":method", "GET"}, {":path", "/ip"},
			},
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := stream.NewStream(1)
			st.Headers = tt.headers
			rw := &mockResponseWriter{}
			st.ResponseWriter = rw
			defer st.Release()

			c := acquireContext(st)
			defer releaseContext(c)

			if got := c.ClientIP(); got != tt.expected {
				t.Fatalf("expected %q, got %q", tt.expected, got)
			}
		})
	}
}

func TestContextSetHeaderCRLFInjection(t *testing.T) {
	s, rw := newTestStream("GET", "/crlf")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	// Attempt header injection via \r\n in value.
	c.SetHeader("location", "http://evil.com\r\nSet-Cookie: admin=true")
	_ = c.NoContent(302)

	// The injected \r\n must be stripped — no Set-Cookie header should appear.
	for _, h := range rw.headers {
		if h[0] == "set-cookie" {
			t.Fatal("CRLF injection: Set-Cookie header was injected via location value")
		}
	}

	// Verify the location value has CRLF stripped.
	for _, h := range rw.headers {
		if h[0] == "location" {
			if h[1] != "http://evil.comSet-Cookie: admin=true" {
				t.Fatalf("expected CRLF stripped, got %q", h[1])
			}
			return
		}
	}
	t.Fatal("expected location header")
}

func TestContextRedirectCRLFInjection(t *testing.T) {
	s, rw := newTestStream("GET", "/redirect-crlf")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	// Simulate user-controlled redirect target with CRLF.
	_ = c.Redirect(302, "/safe\r\nX-Injected: bad")

	for _, h := range rw.headers {
		if h[0] == "x-injected" {
			t.Fatal("CRLF injection via Redirect")
		}
	}
}

func TestContextSetHeaderLowercase(t *testing.T) {
	s, rw := newTestStream("GET", "/headers")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	c.SetHeader("Content-Type", "application/json")
	c.SetHeader("X-Custom-Header", "value")
	_ = c.NoContent(200)

	for _, h := range rw.headers {
		if h[0] == "Content-Type" {
			t.Fatal("Content-Type should be lowercased to content-type")
		}
		if h[0] == "X-Custom-Header" {
			t.Fatal("X-Custom-Header should be lowercased to x-custom-header")
		}
	}

	var found bool
	for _, h := range rw.headers {
		if h[0] == "content-type" && h[1] == "application/json" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected lowercased content-type header")
	}
}

func TestContextQueryCaching(t *testing.T) {
	s, _ := newTestStream("GET", "/search?q=hello&page=2")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	// First call parses and caches.
	if c.Query("q") != "hello" {
		t.Fatalf("expected hello, got %s", c.Query("q"))
	}

	// Second call should use cache (same result).
	if c.Query("page") != "2" {
		t.Fatalf("expected 2, got %s", c.Query("page"))
	}

	if c.Query("missing") != "" {
		t.Fatal("expected empty for missing query param")
	}

	// Verify cache is cleared on reset.
	releaseContext(c)
	if c.queryCached {
		t.Fatal("expected queryCached to be false after reset")
	}
}

func TestContextSetCookieDeleteExpired(t *testing.T) {
	s, rw := newTestStream("GET", "/cookie-delete")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	c.SetCookie(&Cookie{
		Name:   "session",
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	})
	_ = c.NoContent(200)

	var cookieHeader string
	for _, h := range rw.headers {
		if h[0] == "set-cookie" {
			cookieHeader = h[1]
			break
		}
	}

	expected := "session=; Path=/; Max-Age=0"
	if cookieHeader != expected {
		t.Fatalf("expected %q, got %q", expected, cookieHeader)
	}
}

func TestContextSetCookieDomainAndSameSite(t *testing.T) {
	s, rw := newTestStream("GET", "/cookie-domain")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	c.SetCookie(&Cookie{
		Name:     "session",
		Value:    "abc",
		Path:     "/",
		Domain:   "example.com",
		MaxAge:   3600,
		HTTPOnly: true,
		Secure:   true,
		SameSite: SameSiteStrictMode,
	})
	_ = c.NoContent(200)

	var cookieHeader string
	for _, h := range rw.headers {
		if h[0] == "set-cookie" {
			cookieHeader = h[1]
			break
		}
	}

	expected := "session=abc; Path=/; Domain=example.com; Max-Age=3600; HttpOnly; Secure; SameSite=Strict"
	if cookieHeader != expected {
		t.Fatalf("expected %q, got %q", expected, cookieHeader)
	}
}

func TestContextSetCookieSameSiteLax(t *testing.T) {
	s, rw := newTestStream("GET", "/cookie-lax")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	c.SetCookie(&Cookie{
		Name:     "pref",
		Value:    "dark",
		Path:     "/",
		SameSite: SameSiteLaxMode,
	})
	_ = c.NoContent(200)

	var cookieHeader string
	for _, h := range rw.headers {
		if h[0] == "set-cookie" {
			cookieHeader = h[1]
			break
		}
	}

	expected := "pref=dark; Path=/; SameSite=Lax"
	if cookieHeader != expected {
		t.Fatalf("expected %q, got %q", expected, cookieHeader)
	}
}

func TestContextSetCookieSameSiteNone(t *testing.T) {
	s, rw := newTestStream("GET", "/cookie-none")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	c.SetCookie(&Cookie{
		Name:     "track",
		Value:    "x",
		Path:     "/",
		Secure:   true,
		SameSite: SameSiteNoneMode,
	})
	_ = c.NoContent(200)

	var cookieHeader string
	for _, h := range rw.headers {
		if h[0] == "set-cookie" {
			cookieHeader = h[1]
			break
		}
	}

	expected := "track=x; Path=/; Secure; SameSite=None"
	if cookieHeader != expected {
		t.Fatalf("expected %q, got %q", expected, cookieHeader)
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

func TestContextSetHeaderReplaces(t *testing.T) {
	s, rw := newTestStream("GET", "/replace")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	c.SetHeader("x-foo", "a")
	c.SetHeader("x-foo", "b")
	_ = c.NoContent(200)

	count := 0
	var val string
	for _, h := range rw.headers {
		if h[0] == "x-foo" {
			count++
			val = h[1]
		}
	}
	if count != 1 {
		t.Fatalf("expected 1 x-foo header, got %d", count)
	}
	if val != "b" {
		t.Fatalf("expected value 'b', got %q", val)
	}
}

func TestContextAddHeaderAppends(t *testing.T) {
	s, rw := newTestStream("GET", "/append")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	c.AddHeader("set-cookie", "a=1")
	c.AddHeader("set-cookie", "b=2")
	_ = c.NoContent(200)

	count := 0
	for _, h := range rw.headers {
		if h[0] == "set-cookie" {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("expected 2 set-cookie headers, got %d", count)
	}
}

func TestContextDoubleWriteGuard(t *testing.T) {
	s, _ := newTestStream("GET", "/double")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	// First write should succeed.
	err := c.JSON(200, map[string]string{"ok": "true"})
	if err != nil {
		t.Fatalf("first write should succeed, got %v", err)
	}

	// Second write should return ErrResponseWritten.
	err = c.JSON(200, map[string]string{"bad": "true"})
	if err != ErrResponseWritten {
		t.Fatalf("expected ErrResponseWritten, got %v", err)
	}

	// NoContent should also be guarded.
	err = c.NoContent(204)
	if err != ErrResponseWritten {
		t.Fatalf("expected ErrResponseWritten from NoContent, got %v", err)
	}
}

func TestContextDoubleWriteStringBlob(t *testing.T) {
	s, _ := newTestStream("GET", "/double2")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	err := c.String(200, "first")
	if err != nil {
		t.Fatal(err)
	}

	err = c.Blob(200, "text/plain", []byte("second"))
	if err != ErrResponseWritten {
		t.Fatalf("expected ErrResponseWritten, got %v", err)
	}
}

func TestNextErrorPropagation(t *testing.T) {
	s, _ := newTestStream("GET", "/err")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	handlerErr := fmt.Errorf("handler failed")
	c.handlers = []HandlerFunc{
		func(_ *Context) error {
			return handlerErr
		},
	}

	err := c.Next()
	if err != handlerErr {
		t.Fatalf("expected handlerErr, got %v", err)
	}
}

func TestNextErrorShortCircuit(t *testing.T) {
	s, _ := newTestStream("GET", "/short")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	secondCalled := false
	c.handlers = []HandlerFunc{
		func(_ *Context) error {
			return fmt.Errorf("stop")
		},
		func(_ *Context) error {
			secondCalled = true
			return nil
		},
	}

	err := c.Next()
	if err == nil {
		t.Fatal("expected error")
	}
	if secondCalled {
		t.Fatal("second handler should not have been called after error")
	}
}

func TestMiddlewareErrorSwallow(t *testing.T) {
	s, _ := newTestStream("GET", "/swallow")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	c.handlers = []HandlerFunc{
		func(c *Context) error {
			err := c.Next()
			if err != nil {
				// Middleware swallows the error.
				return nil
			}
			return nil
		},
		func(_ *Context) error {
			return fmt.Errorf("downstream error")
		},
	}

	err := c.Next()
	if err != nil {
		t.Fatalf("expected nil after swallow, got %v", err)
	}
}

func TestHTTPErrorType(t *testing.T) {
	he := NewHTTPError(404, "not found")
	if he.Code != 404 {
		t.Fatalf("expected 404, got %d", he.Code)
	}
	if he.Message != "not found" {
		t.Fatalf("expected 'not found', got %s", he.Message)
	}
	if he.Error() != "code=404, message=not found" {
		t.Fatalf("unexpected Error(): %s", he.Error())
	}

	// With wrapped error.
	inner := fmt.Errorf("db error")
	he.Err = inner
	if he.Unwrap() != inner {
		t.Fatal("expected Unwrap to return inner error")
	}
}

func TestAbortWithStatusReturnsError(t *testing.T) {
	s, rw := newTestStream("GET", "/abort-status")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	err := c.AbortWithStatus(403)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if rw.status != 403 {
		t.Fatalf("expected 403, got %d", rw.status)
	}
	if !c.IsAborted() {
		t.Fatal("expected aborted")
	}
}

func TestXMLResponse(t *testing.T) {
	s, rw := newTestStream("GET", "/xml")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	type Item struct {
		Name string `xml:"name"`
	}
	err := c.XML(200, Item{Name: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if rw.status != 200 {
		t.Fatalf("expected 200, got %d", rw.status)
	}

	var ctFound bool
	for _, h := range rw.headers {
		if h[0] == "content-type" && h[1] == "application/xml" {
			ctFound = true
			break
		}
	}
	if !ctFound {
		t.Fatal("expected content-type: application/xml")
	}

	if !strings.Contains(string(rw.body), "<name>test</name>") {
		t.Fatalf("expected XML body, got %s", string(rw.body))
	}
}

func TestBindJSON(t *testing.T) {
	s, _ := newTestStream("POST", "/bind-json")
	s.Data.Write([]byte(`{"name":"test"}`))
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	var v struct{ Name string }
	if err := c.BindJSON(&v); err != nil {
		t.Fatal(err)
	}
	if v.Name != "test" {
		t.Fatalf("expected test, got %s", v.Name)
	}
}

func TestBindXML(t *testing.T) {
	s, _ := newTestStream("POST", "/bind-xml")
	s.Data.Write([]byte(`<Item><Name>test</Name></Item>`))
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	type Item struct {
		Name string `xml:"Name"`
	}
	var v Item
	if err := c.BindXML(&v); err != nil {
		t.Fatal(err)
	}
	if v.Name != "test" {
		t.Fatalf("expected test, got %s", v.Name)
	}
}

func TestBindContentTypeDetection(t *testing.T) {
	// JSON (default, no Content-Type).
	s, _ := newTestStream("POST", "/bind-auto")
	s.Data.Write([]byte(`{"name":"json"}`))
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	var v struct{ Name string }
	if err := c.Bind(&v); err != nil {
		t.Fatal(err)
	}
	if v.Name != "json" {
		t.Fatalf("expected json, got %s", v.Name)
	}

	// XML with Content-Type header.
	s2, _ := newTestStream("POST", "/bind-auto-xml")
	s2.Headers = append(s2.Headers, [2]string{"content-type", "application/xml"})
	s2.Data.Write([]byte(`<Item><Name>xml</Name></Item>`))
	defer s2.Release()

	c2 := acquireContext(s2)
	defer releaseContext(c2)

	type Item struct {
		Name string `xml:"Name"`
	}
	var v2 Item
	if err := c2.Bind(&v2); err != nil {
		t.Fatal(err)
	}
	if v2.Name != "xml" {
		t.Fatalf("expected xml, got %s", v2.Name)
	}
}

func TestBindEmptyBody(t *testing.T) {
	s, _ := newTestStream("POST", "/bind-empty")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	var v struct{ Name string }
	err := c.Bind(&v)
	if err == nil {
		t.Fatal("expected error for empty body")
	}

	err = c.BindJSON(&v)
	if err == nil {
		t.Fatal("expected error for empty body")
	}

	err = c.BindXML(&v)
	if err == nil {
		t.Fatal("expected error for empty body")
	}
}

func TestContextQueryValues(t *testing.T) {
	s, _ := newTestStream("GET", "/multi?color=red&color=blue&color=green")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	vals := c.QueryValues("color")
	if len(vals) != 3 {
		t.Fatalf("expected 3 values, got %d", len(vals))
	}
	if vals[0] != "red" || vals[1] != "blue" || vals[2] != "green" {
		t.Fatalf("expected [red blue green], got %v", vals)
	}

	// Non-existent key returns nil.
	vals = c.QueryValues("missing")
	if vals != nil {
		t.Fatalf("expected nil for missing key, got %v", vals)
	}
}

func TestContextQueryParams(t *testing.T) {
	s, _ := newTestStream("GET", "/all?a=1&b=2&c=3")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	params := c.QueryParams()
	if params == nil {
		t.Fatal("expected non-nil params")
	}
	if params.Get("a") != "1" {
		t.Fatalf("expected a=1, got %s", params.Get("a"))
	}
	if params.Get("b") != "2" {
		t.Fatalf("expected b=2, got %s", params.Get("b"))
	}
	if params.Get("c") != "3" {
		t.Fatalf("expected c=3, got %s", params.Get("c"))
	}
}

func TestHTTPErrorWithError(t *testing.T) {
	inner := fmt.Errorf("database connection failed")
	he := NewHTTPError(500, "internal error").WithError(inner)

	if he.Code != 500 {
		t.Fatalf("expected 500, got %d", he.Code)
	}
	if he.Message != "internal error" {
		t.Fatalf("expected 'internal error', got %s", he.Message)
	}
	if he.Err != inner {
		t.Fatal("expected wrapped error")
	}
	if he.Unwrap() != inner {
		t.Fatal("Unwrap should return inner error")
	}

	// Error() should include the wrapped error.
	errStr := he.Error()
	if !strings.Contains(errStr, "database connection failed") {
		t.Fatalf("expected error string to contain inner error, got %s", errStr)
	}
	if !strings.Contains(errStr, "code=500") {
		t.Fatalf("expected error string to contain code, got %s", errStr)
	}
}

func TestRedirectInvalidCode(t *testing.T) {
	s, _ := newTestStream("GET", "/bad-redirect")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	// 200 is not a valid redirect code.
	err := c.Redirect(200, "/dest")
	if err == nil {
		t.Fatal("expected error for non-3xx redirect code")
	}
	if !strings.Contains(err.Error(), "3xx") {
		t.Fatalf("expected error message to mention 3xx, got %s", err.Error())
	}

	// 404 is not a valid redirect code.
	err = c.Redirect(404, "/dest")
	if err == nil {
		t.Fatal("expected error for 404 redirect code")
	}
}

// --- BasicAuth tests ---

func TestContextBasicAuth(t *testing.T) {
	s, _ := newTestStream("GET", "/auth")
	s.Headers = append(s.Headers, [2]string{"authorization", "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:s3cret"))})
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	user, pass, ok := c.BasicAuth()
	if !ok {
		t.Fatal("expected ok")
	}
	if user != "alice" {
		t.Fatalf("expected alice, got %s", user)
	}
	if pass != "s3cret" {
		t.Fatalf("expected s3cret, got %s", pass)
	}
}

func TestContextBasicAuthMissing(t *testing.T) {
	s, _ := newTestStream("GET", "/auth")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	_, _, ok := c.BasicAuth()
	if ok {
		t.Fatal("expected not ok for missing header")
	}
}

func TestContextBasicAuthNonBasic(t *testing.T) {
	s, _ := newTestStream("GET", "/auth")
	s.Headers = append(s.Headers, [2]string{"authorization", "Bearer token123"})
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	_, _, ok := c.BasicAuth()
	if ok {
		t.Fatal("expected not ok for Bearer scheme")
	}
}

func TestContextBasicAuthMalformedBase64(t *testing.T) {
	s, _ := newTestStream("GET", "/auth")
	s.Headers = append(s.Headers, [2]string{"authorization", "Basic !!!invalid!!!"})
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	_, _, ok := c.BasicAuth()
	if ok {
		t.Fatal("expected not ok for malformed base64")
	}
}

func TestContextBasicAuthNoColon(t *testing.T) {
	s, _ := newTestStream("GET", "/auth")
	s.Headers = append(s.Headers, [2]string{"authorization", "Basic " + base64.StdEncoding.EncodeToString([]byte("nocolon"))})
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	_, _, ok := c.BasicAuth()
	if ok {
		t.Fatal("expected not ok for missing colon")
	}
}

func TestContextBasicAuthEmptyUsername(t *testing.T) {
	s, _ := newTestStream("GET", "/auth")
	s.Headers = append(s.Headers, [2]string{"authorization", "Basic " + base64.StdEncoding.EncodeToString([]byte(":password"))})
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	user, pass, ok := c.BasicAuth()
	if !ok {
		t.Fatal("expected ok")
	}
	if user != "" {
		t.Fatalf("expected empty username, got %s", user)
	}
	if pass != "password" {
		t.Fatalf("expected password, got %s", pass)
	}
}

func TestContextBasicAuthPasswordWithColons(t *testing.T) {
	s, _ := newTestStream("GET", "/auth")
	s.Headers = append(s.Headers, [2]string{"authorization", "Basic " + base64.StdEncoding.EncodeToString([]byte("user:pass:word:more"))})
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	user, pass, ok := c.BasicAuth()
	if !ok {
		t.Fatal("expected ok")
	}
	if user != "user" {
		t.Fatalf("expected user, got %s", user)
	}
	if pass != "pass:word:more" {
		t.Fatalf("expected pass:word:more, got %s", pass)
	}
}

// --- Form handling tests ---

func TestContextFormValueURLEncoded(t *testing.T) {
	s, _ := newTestStream("POST", "/form")
	s.Headers = append(s.Headers, [2]string{"content-type", "application/x-www-form-urlencoded"})
	s.Data.Write([]byte("name=alice&age=30"))
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	if c.FormValue("name") != "alice" {
		t.Fatalf("expected alice, got %s", c.FormValue("name"))
	}
	if c.FormValue("age") != "30" {
		t.Fatalf("expected 30, got %s", c.FormValue("age"))
	}
	if c.FormValue("missing") != "" {
		t.Fatal("expected empty for missing field")
	}
}

func TestContextFormValuesMultiple(t *testing.T) {
	s, _ := newTestStream("POST", "/form")
	s.Headers = append(s.Headers, [2]string{"content-type", "application/x-www-form-urlencoded"})
	s.Data.Write([]byte("color=red&color=blue&color=green"))
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	vals := c.FormValues("color")
	if len(vals) != 3 {
		t.Fatalf("expected 3 values, got %d", len(vals))
	}
	if vals[0] != "red" || vals[1] != "blue" || vals[2] != "green" {
		t.Fatalf("expected [red blue green], got %v", vals)
	}
}

func TestContextMultipartFormValue(t *testing.T) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("name", "bob")
	_ = w.WriteField("age", "25")
	_ = w.Close()

	s, _ := newTestStream("POST", "/upload")
	s.Headers = append(s.Headers, [2]string{"content-type", w.FormDataContentType()})
	s.Data.Write(buf.Bytes())
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	if c.FormValue("name") != "bob" {
		t.Fatalf("expected bob, got %s", c.FormValue("name"))
	}
	if c.FormValue("age") != "25" {
		t.Fatalf("expected 25, got %s", c.FormValue("age"))
	}
}

func TestContextFormFile(t *testing.T) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", `form-data; name="file"; filename="test.txt"`)
	h.Set("Content-Type", "text/plain")
	part, err := w.CreatePart(h)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("file content"))
	_ = w.Close()

	s, _ := newTestStream("POST", "/upload")
	s.Headers = append(s.Headers, [2]string{"content-type", w.FormDataContentType()})
	s.Data.Write(buf.Bytes())
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	f, fh, err := c.FormFile("file")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	if fh.Filename != "test.txt" {
		t.Fatalf("expected test.txt, got %s", fh.Filename)
	}

	data, _ := io.ReadAll(f)
	if string(data) != "file content" {
		t.Fatalf("expected 'file content', got '%s'", string(data))
	}
}

func TestContextFormFileNonMultipart(t *testing.T) {
	s, _ := newTestStream("POST", "/upload")
	s.Headers = append(s.Headers, [2]string{"content-type", "application/x-www-form-urlencoded"})
	s.Data.Write([]byte("key=value"))
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	_, _, err := c.FormFile("file")
	if err == nil {
		t.Fatal("expected error for non-multipart request")
	}
}

func TestContextFormEmptyBody(t *testing.T) {
	s, _ := newTestStream("POST", "/empty")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	if c.FormValue("key") != "" {
		t.Fatal("expected empty for empty body")
	}
}

func TestContextFormCaching(t *testing.T) {
	s, _ := newTestStream("POST", "/form")
	s.Headers = append(s.Headers, [2]string{"content-type", "application/x-www-form-urlencoded"})
	s.Data.Write([]byte("a=1&b=2"))
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	// First call parses.
	if c.FormValue("a") != "1" {
		t.Fatalf("expected 1, got %s", c.FormValue("a"))
	}
	// Second call uses cache.
	if c.FormValue("b") != "2" {
		t.Fatalf("expected 2, got %s", c.FormValue("b"))
	}
}

func TestContextFormResetClearsForm(t *testing.T) {
	s, _ := newTestStream("POST", "/form")
	s.Headers = append(s.Headers, [2]string{"content-type", "application/x-www-form-urlencoded"})
	s.Data.Write([]byte("key=value"))
	defer s.Release()

	c := acquireContext(s)
	_ = c.FormValue("key") // trigger parse
	releaseContext(c)

	if c.formParsed {
		t.Fatal("expected formParsed to be false after reset")
	}
	if c.formValues != nil {
		t.Fatal("expected formValues to be nil after reset")
	}
}

// --- File serving tests ---

func TestContextFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	_ = os.WriteFile(path, []byte("hello file"), 0644)

	s, rw := newTestStream("GET", "/file")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	if err := c.File(path); err != nil {
		t.Fatal(err)
	}
	if rw.status != 200 {
		t.Fatalf("expected 200, got %d", rw.status)
	}
	if string(rw.body) != "hello file" {
		t.Fatalf("expected 'hello file', got '%s'", string(rw.body))
	}
}

func TestContextFileMissing(t *testing.T) {
	s, _ := newTestStream("GET", "/file")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	err := c.File("/nonexistent/file.txt")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestContextFileContentType(t *testing.T) {
	tests := []struct {
		name    string
		ext     string
		wantCT  string
		content string
	}{
		{"html", ".html", "text/html", "<h1>Hello</h1>"},
		{"css", ".css", "text/css", "body {}"},
		{"json", ".json", "application/json", `{"a":1}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "file"+tt.ext)
			_ = os.WriteFile(path, []byte(tt.content), 0644)

			st, rw := newTestStream("GET", "/file")
			defer st.Release()

			c := acquireContext(st)
			defer releaseContext(c)

			if err := c.File(path); err != nil {
				t.Fatal(err)
			}

			var ct string
			for _, h := range rw.headers {
				if h[0] == "content-type" {
					ct = h[1]
					break
				}
			}
			if !strings.HasPrefix(ct, tt.wantCT) {
				t.Fatalf("expected content-type starting with %s, got %s", tt.wantCT, ct)
			}
		})
	}
}

func TestContextFileRange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "range.txt")
	_ = os.WriteFile(path, []byte("0123456789"), 0644)

	s, rw := newTestStream("GET", "/file")
	s.Headers = append(s.Headers, [2]string{"range", "bytes=2-5"})
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	if err := c.File(path); err != nil {
		t.Fatal(err)
	}
	if rw.status != 206 {
		t.Fatalf("expected 206, got %d", rw.status)
	}
	if string(rw.body) != "2345" {
		t.Fatalf("expected '2345', got '%s'", string(rw.body))
	}

	var cr string
	for _, h := range rw.headers {
		if h[0] == "content-range" {
			cr = h[1]
			break
		}
	}
	if cr != "bytes 2-5/10" {
		t.Fatalf("expected 'bytes 2-5/10', got %q", cr)
	}
}

func TestContextStream(t *testing.T) {
	s, rw := newTestStream("GET", "/stream")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	r := bytes.NewReader([]byte("streamed data"))
	if err := c.Stream(200, "text/plain", r); err != nil {
		t.Fatal(err)
	}
	if rw.status != 200 {
		t.Fatalf("expected 200, got %d", rw.status)
	}
	if string(rw.body) != "streamed data" {
		t.Fatalf("expected 'streamed data', got '%s'", string(rw.body))
	}
}

func TestContextParamInt(t *testing.T) {
	s, _ := newTestStream("GET", "/users/42")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	c.params = append(c.params, Param{Key: "id", Value: "42"})

	v, err := c.ParamInt("id")
	if err != nil {
		t.Fatal(err)
	}
	if v != 42 {
		t.Fatalf("expected 42, got %d", v)
	}

	_, err = c.ParamInt("missing")
	if err == nil {
		t.Fatal("expected error for missing param")
	}

	c.params = append(c.params, Param{Key: "bad", Value: "abc"})
	_, err = c.ParamInt("bad")
	if err == nil {
		t.Fatal("expected error for non-integer param")
	}
}

func TestContextParamInt64(t *testing.T) {
	s, _ := newTestStream("GET", "/items/9999999999")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	c.params = append(c.params, Param{Key: "id", Value: "9999999999"})

	v, err := c.ParamInt64("id")
	if err != nil {
		t.Fatal(err)
	}
	if v != 9999999999 {
		t.Fatalf("expected 9999999999, got %d", v)
	}

	_, err = c.ParamInt64("missing")
	if err == nil {
		t.Fatal("expected error for missing param")
	}
}

func TestContextQueryDefault(t *testing.T) {
	s, _ := newTestStream("GET", "/search?q=hello")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	if c.QueryDefault("q", "fallback") != "hello" {
		t.Fatalf("expected hello, got %s", c.QueryDefault("q", "fallback"))
	}
	if c.QueryDefault("missing", "fallback") != "fallback" {
		t.Fatalf("expected fallback, got %s", c.QueryDefault("missing", "fallback"))
	}
}

func TestContextQueryInt(t *testing.T) {
	s, _ := newTestStream("GET", "/list?page=3&bad=abc")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	if c.QueryInt("page", 1) != 3 {
		t.Fatalf("expected 3, got %d", c.QueryInt("page", 1))
	}
	if c.QueryInt("missing", 1) != 1 {
		t.Fatalf("expected 1 (default), got %d", c.QueryInt("missing", 1))
	}
	if c.QueryInt("bad", 1) != 1 {
		t.Fatalf("expected 1 (default for non-int), got %d", c.QueryInt("bad", 1))
	}
}

func TestContextHTML(t *testing.T) {
	s, rw := newTestStream("GET", "/page")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	if err := c.HTML(200, "<h1>Hello</h1>"); err != nil {
		t.Fatal(err)
	}
	if rw.status != 200 {
		t.Fatalf("expected 200, got %d", rw.status)
	}
	if string(rw.body) != "<h1>Hello</h1>" {
		t.Fatalf("expected <h1>Hello</h1>, got %s", string(rw.body))
	}
	var ct string
	for _, h := range rw.headers {
		if h[0] == "content-type" {
			ct = h[1]
			break
		}
	}
	if ct != "text/html; charset=utf-8" {
		t.Fatalf("expected text/html; charset=utf-8, got %s", ct)
	}
}

func TestContextFormValueOk(t *testing.T) {
	body := "name=alice&empty="
	s, _ := newTestStream("POST", "/form")
	s.Headers = append(s.Headers, [2]string{"content-type", "application/x-www-form-urlencoded"})
	s.Data.Write([]byte(body))
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	v, ok := c.FormValueOk("name")
	if !ok || v != "alice" {
		t.Fatalf("expected (alice, true), got (%s, %v)", v, ok)
	}

	v, ok = c.FormValueOk("empty")
	if !ok {
		t.Fatal("expected field 'empty' to be present")
	}
	if v != "" {
		t.Fatalf("expected empty string, got %q", v)
	}

	_, ok = c.FormValueOk("missing")
	if ok {
		t.Fatal("expected missing field to return false")
	}
}

func TestContextFileFromDir(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "file.txt"), []byte("hello"), 0644)

	s, rw := newTestStream("GET", "/files/file.txt")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	if err := c.FileFromDir(dir, "file.txt"); err != nil {
		t.Fatal(err)
	}
	if rw.status != 200 {
		t.Fatalf("expected 200, got %d", rw.status)
	}
	if string(rw.body) != "hello" {
		t.Fatalf("expected hello, got %s", string(rw.body))
	}
}

func TestContextFileFromDirTraversal(t *testing.T) {
	dir := t.TempDir()

	s, _ := newTestStream("GET", "/files/../../../etc/passwd")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	err := c.FileFromDir(dir, "../../../etc/passwd")
	if err == nil {
		t.Fatal("expected error for traversal attempt")
	}
	var he *HTTPError
	if !errors.As(err, &he) || he.Code != 400 {
		t.Fatalf("expected HTTPError 400, got %v", err)
	}
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

func TestContextScheme(t *testing.T) {
	// Default: falls back to :scheme pseudo-header.
	s, _ := newTestStream("GET", "/test")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	if c.Scheme() != "http" {
		t.Fatalf("expected http, got %s", c.Scheme())
	}

	// X-Forwarded-Proto takes precedence.
	s2, _ := newTestStream("GET", "/test")
	s2.Headers = append(s2.Headers, [2]string{"x-forwarded-proto", "https"})
	defer s2.Release()

	c2 := acquireContext(s2)
	defer releaseContext(c2)

	if c2.Scheme() != "https" {
		t.Fatalf("expected https, got %s", c2.Scheme())
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
