package adapters

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
	"github.com/goceleris/celeris/middleware/internal/testutil"
)

func TestWrapMiddleware(t *testing.T) {
	// A stdlib middleware that adds a header and calls next.
	stdMW := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Added", "yes")
			next.ServeHTTP(w, r)
		})
	}

	handler := func(c *celeris.Context) error {
		return c.String(200, "hello")
	}

	chain := []celeris.HandlerFunc{WrapMiddleware(stdMW), handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "hello")
}

func TestWrapMiddlewareShortCircuit(t *testing.T) {
	// A stdlib middleware that returns 403 without calling next.
	stdMW := func(_ http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Reason", "denied")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("forbidden"))
		})
	}

	nextCalled := false
	handler := func(c *celeris.Context) error {
		nextCalled = true
		return c.String(200, "ok")
	}

	chain := []celeris.HandlerFunc{WrapMiddleware(stdMW), handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/secret")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 403)
	testutil.AssertBodyContains(t, rec, "forbidden")
	testutil.AssertHeader(t, rec, "x-reason", "denied")
	if nextCalled {
		t.Fatal("expected next handler to NOT be called")
	}
}

func TestWrapMiddlewareCallsNext(t *testing.T) {
	// Verify the downstream handler actually executes and writes its
	// response when the stdlib middleware calls through.
	var executed bool
	stdMW := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)
		})
	}

	handler := func(c *celeris.Context) error {
		executed = true
		return c.String(200, "downstream")
	}

	chain := []celeris.HandlerFunc{WrapMiddleware(stdMW), handler}
	rec, err := testutil.RunChain(t, chain, "POST", "/action")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "downstream")
	if !executed {
		t.Fatal("expected downstream handler to execute")
	}
}

func TestToStdlib(t *testing.T) {
	h := func(c *celeris.Context) error {
		return c.String(200, "from celeris")
	}

	stdHandler := ToStdlib(h)
	srv := httptest.NewServer(stdHandler)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/test")
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	if string(body) != "from celeris" {
		t.Fatalf("body: got %q, want %q", string(body), "from celeris")
	}
}

func TestReverseProxy(t *testing.T) {
	// Backend echoes the request path.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Backend", "true")
		_, _ = w.Write([]byte("proxied:" + r.URL.Path))
	}))
	defer backend.Close()

	target, _ := url.Parse(backend.URL)
	proxyHandler := ReverseProxy(target)

	// Use the celeris ToHandler bridge to test via httptest.
	srv := httptest.NewServer(celeris.ToHandler(proxyHandler))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/hello")
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	if string(body) != "proxied:/hello" {
		t.Fatalf("body: got %q, want %q", string(body), "proxied:/hello")
	}
}

func TestReverseProxyNilPanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for nil target")
		}
		msg, ok := r.(string)
		if !ok || msg != "adapters: ReverseProxy target must not be nil" {
			t.Fatalf("unexpected panic: %v", r)
		}
	}()
	ReverseProxy(nil)
}

func TestBuildRequest(t *testing.T) {
	ctx, _ := celeristest.NewContextT(t, "POST", "/path",
		celeristest.WithQuery("key", "val"),
		celeristest.WithHeader("x-custom", "123"),
		celeristest.WithBody([]byte("body data")),
	)

	req := buildRequest(ctx)

	if req.Method != "POST" {
		t.Fatalf("method: got %q, want POST", req.Method)
	}
	if !contains(req.URL.String(), "/path") {
		t.Fatalf("URL should contain /path: got %q", req.URL.String())
	}
	if !contains(req.URL.String(), "key=val") {
		t.Fatalf("URL should contain key=val: got %q", req.URL.String())
	}
	if req.Header.Get("X-Custom") != "123" {
		t.Fatalf("header x-custom: got %q, want 123", req.Header.Get("X-Custom"))
	}
	b, _ := io.ReadAll(req.Body)
	if string(b) != "body data" {
		t.Fatalf("body: got %q, want %q", string(b), "body data")
	}
}

func TestResponseCapture(t *testing.T) {
	rec := &responseCapture{header: make(http.Header)}

	rec.Header().Set("Content-Type", "text/plain")
	rec.WriteHeader(201)
	n, err := rec.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("Write error: %v", err)
	}
	if n != 5 {
		t.Fatalf("Write n: got %d, want 5", n)
	}

	if rec.code != 201 {
		t.Fatalf("code: got %d, want 201", rec.code)
	}
	if string(rec.body) != "hello" {
		t.Fatalf("body: got %q, want hello", string(rec.body))
	}
	if rec.header.Get("Content-Type") != "text/plain" {
		t.Fatalf("header: got %q, want text/plain", rec.header.Get("Content-Type"))
	}
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
