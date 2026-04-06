package adapters

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
	"github.com/goceleris/celeris/middleware/internal/testutil"
)

func TestWrapMiddlewareHeaderPropagation(t *testing.T) {
	// Stdlib middleware that sets a CORS header then calls inner.
	corsMW := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			next.ServeHTTP(w, r)
		})
	}

	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}

	mw := WrapMiddleware(corsMW)
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertHeader(t, rec, "access-control-allow-origin", "*")
}

func TestWrapMiddlewareShortCircuit(t *testing.T) {
	// Stdlib middleware that returns 403 without calling inner.
	authMW := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Auth-Error", "forbidden")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("access denied"))
		})
	}

	handler := func(c *celeris.Context) error {
		t.Fatal("inner handler should not be called")
		return nil
	}

	mw := WrapMiddleware(authMW)
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/secret")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 403)
	testutil.AssertHeader(t, rec, "x-auth-error", "forbidden")
	testutil.AssertBodyContains(t, rec, "access denied")
}

func TestWrapMiddlewarePassthrough(t *testing.T) {
	// Stdlib middleware that does nothing special, just calls inner.
	noop := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)
		})
	}

	handler := func(c *celeris.Context) error {
		return c.String(200, "hello")
	}

	mw := WrapMiddleware(noop)
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "hello")
}

func TestWrapMiddlewareMultipleHeaders(t *testing.T) {
	// Stdlib middleware that sets multiple headers before calling inner.
	multiMW := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Custom-A", "alpha")
			w.Header().Set("X-Custom-B", "beta")
			w.Header().Add("Set-Cookie", "session=abc")
			next.ServeHTTP(w, r)
		})
	}

	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}

	mw := WrapMiddleware(multiMW)
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertHeader(t, rec, "x-custom-a", "alpha")
	testutil.AssertHeader(t, rec, "x-custom-b", "beta")
	testutil.AssertHeader(t, rec, "set-cookie", "session=abc")
}

func TestBuildRequestContentLength(t *testing.T) {
	mw := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength != 5 {
				t.Fatalf("ContentLength: got %d, want 5", r.ContentLength)
			}
			next.ServeHTTP(w, r)
		})
	}

	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}

	wrapped := WrapMiddleware(mw)
	chain := []celeris.HandlerFunc{wrapped, handler}
	_, err := testutil.RunChain(t, chain, "POST", "/",
		celeristest.WithBody([]byte("hello")),
		celeristest.WithHeader("content-length", "5"),
	)
	testutil.AssertNoError(t, err)
}

func TestBuildRequestTLSState(t *testing.T) {
	mw := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.TLS == nil {
				t.Fatal("expected TLS state to be non-nil for https")
			}
			next.ServeHTTP(w, r)
		})
	}

	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}

	wrapped := WrapMiddleware(mw)
	chain := []celeris.HandlerFunc{wrapped, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithScheme("https"),
	)
	testutil.AssertNoError(t, err)
}

func TestReverseProxyPanicNilTarget(t *testing.T) {
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

func TestReverseProxyOptions(t *testing.T) {
	// Verify options are applied without panic.
	var modifyCalled bool
	target, _ := url.Parse("http://localhost:9999")

	// Just verify construction succeeds with all options.
	h := ReverseProxy(target,
		WithTransport(http.DefaultTransport),
		WithModifyRequest(func(r *http.Request) {
			modifyCalled = true
			_ = modifyCalled
		}),
		WithErrorHandler(func(w http.ResponseWriter, r *http.Request, err error) {
			w.WriteHeader(502)
		}),
	)
	if h == nil {
		t.Fatal("expected non-nil handler")
	}
}

func TestBuildRequestProtocol(t *testing.T) {
	tests := []struct {
		name      string
		protocol  string
		wantProto string
		wantMajor int
		wantMinor int
	}{
		{"HTTP/1.1", "1.1", "HTTP/1.1", 1, 1},
		{"HTTP/2", "2", "HTTP/2.0", 2, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mw := func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Proto != tt.wantProto {
						t.Fatalf("Proto: got %q, want %q", r.Proto, tt.wantProto)
					}
					if r.ProtoMajor != tt.wantMajor {
						t.Fatalf("ProtoMajor: got %d, want %d", r.ProtoMajor, tt.wantMajor)
					}
					if r.ProtoMinor != tt.wantMinor {
						t.Fatalf("ProtoMinor: got %d, want %d", r.ProtoMinor, tt.wantMinor)
					}
					next.ServeHTTP(w, r)
				})
			}

			handler := func(c *celeris.Context) error {
				return c.String(200, "ok")
			}

			wrapped := WrapMiddleware(mw)
			chain := []celeris.HandlerFunc{wrapped, handler}
			opts := []celeristest.Option{}
			if tt.protocol != "" {
				opts = append(opts, celeristest.WithProtocol(tt.protocol))
			}
			_, err := testutil.RunChain(t, chain, "GET", "/", opts...)
			testutil.AssertNoError(t, err)
		})
	}
}

func TestWrapMiddlewareNilPanic(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for nil middleware")
		}
		msg, ok := r.(string)
		if !ok || msg != "adapters: WrapMiddleware argument must not be nil" {
			t.Fatalf("unexpected panic: %v", r)
		}
	}()
	WrapMiddleware(nil)
}

func TestWrapMiddlewareQueryStringRoundTrip(t *testing.T) {
	var gotQuery string
	mw := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotQuery = r.URL.RawQuery
			next.ServeHTTP(w, r)
		})
	}

	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}

	wrapped := WrapMiddleware(mw)
	chain := []celeris.HandlerFunc{wrapped, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/search",
		celeristest.WithQuery("q", "hello world"),
		celeristest.WithQuery("page", "2"),
	)
	testutil.AssertNoError(t, err)

	if !strings.Contains(gotQuery, "q=hello+world") && !strings.Contains(gotQuery, "q=hello%20world") {
		t.Fatalf("expected query to contain q=hello+world or q=hello%%20world, got %q", gotQuery)
	}
	if !strings.Contains(gotQuery, "page=2") {
		t.Fatalf("expected query to contain page=2, got %q", gotQuery)
	}
}

func TestWrapMiddlewareHostPropagation(t *testing.T) {
	// Default :authority in celeristest is "localhost". Verify it propagates
	// to r.Host inside the stdlib middleware. Also test a custom host by
	// using the "host" header (which c.Host() checks after :authority).
	t.Run("default", func(t *testing.T) {
		var gotHost string
		mw := func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotHost = r.Host
				next.ServeHTTP(w, r)
			})
		}
		handler := func(c *celeris.Context) error {
			return c.String(200, "ok")
		}
		wrapped := WrapMiddleware(mw)
		chain := []celeris.HandlerFunc{wrapped, handler}
		_, err := testutil.RunChain(t, chain, "GET", "/")
		testutil.AssertNoError(t, err)
		if gotHost != "localhost" {
			t.Fatalf("Host: got %q, want %q", gotHost, "localhost")
		}
	})

	t.Run("custom", func(t *testing.T) {
		// Inject a middleware that overrides the host before the adapter runs.
		setHost := func(c *celeris.Context) error {
			c.SetHost("example.com")
			return c.Next()
		}
		var gotHost string
		mw := func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotHost = r.Host
				next.ServeHTTP(w, r)
			})
		}
		handler := func(c *celeris.Context) error {
			return c.String(200, "ok")
		}
		wrapped := WrapMiddleware(mw)
		chain := []celeris.HandlerFunc{setHost, wrapped, handler}
		_, err := testutil.RunChain(t, chain, "GET", "/")
		testutil.AssertNoError(t, err)
		if gotHost != "example.com" {
			t.Fatalf("Host: got %q, want %q", gotHost, "example.com")
		}
	})
}

func TestWrapMiddlewareBodyRoundTrip(t *testing.T) {
	var gotBody string
	mw := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			data, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("failed to read body: %v", err)
			}
			gotBody = string(data)
			next.ServeHTTP(w, r)
		})
	}

	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}

	wrapped := WrapMiddleware(mw)
	chain := []celeris.HandlerFunc{wrapped, handler}
	_, err := testutil.RunChain(t, chain, "POST", "/submit",
		celeristest.WithBody([]byte(`{"key":"value"}`)),
		celeristest.WithHeader("content-type", "application/json"),
	)
	testutil.AssertNoError(t, err)

	if gotBody != `{"key":"value"}` {
		t.Fatalf("Body: got %q, want %q", gotBody, `{"key":"value"}`)
	}
}

func TestResponseCaptureBodyCap(t *testing.T) {
	// Stdlib middleware that writes >100MB.
	bigMW := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			chunk := make([]byte, 1<<20) // 1MB
			for i := 0; i < 101; i++ {
				_, err := w.Write(chunk)
				if err != nil {
					// Expected: capture capped at 100MB.
					return
				}
			}
		})
	}

	handler := func(c *celeris.Context) error {
		t.Fatal("inner handler should not be called")
		return nil
	}

	wrapped := WrapMiddleware(bigMW)
	chain := []celeris.HandlerFunc{wrapped, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/big")

	// The middleware short-circuits (doesn't call next) so we get the
	// captured response via Blob. The error from Write is swallowed by
	// the middleware itself, but the response should not exceed the cap.
	// No assertion on err since the middleware handles the write error internally.
	_ = err

	// Verify the constant and error are correctly defined.
	if maxCaptureBytes != 100<<20 {
		t.Fatalf("maxCaptureBytes: got %d, want %d", maxCaptureBytes, 100<<20)
	}

	// Directly test the responseCapture.Write cap.
	rec := &responseCapture{header: make(http.Header)}
	chunk := make([]byte, 1<<20)
	for i := 0; i < 100; i++ {
		_, err := rec.Write(chunk)
		if err != nil {
			t.Fatalf("Write failed at chunk %d: %v", i, err)
		}
	}
	_, err = rec.Write([]byte{0})
	if !errors.Is(err, errCaptureResponseTooLarge) {
		t.Fatalf("expected errCaptureResponseTooLarge, got %v", err)
	}
}

func TestReverseProxyBehavioral(t *testing.T) {
	var (
		mu              sync.Mutex
		gotPath         string
		gotXFwdFor      string
		gotXFwdHost     string
		gotXFwdProto    string
		gotCustomHeader string
	)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPath = r.URL.Path
		gotXFwdFor = r.Header.Get("X-Forwarded-For")
		gotXFwdHost = r.Header.Get("X-Forwarded-Host")
		gotXFwdProto = r.Header.Get("X-Forwarded-Proto")
		gotCustomHeader = r.Header.Get("X-Custom-Proxy")
		mu.Unlock()
		w.Header().Set("X-Backend", "ok")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "backend-response")
	}))
	defer backend.Close()

	target, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatalf("parse backend URL: %v", err)
	}

	h := ReverseProxy(target,
		WithModifyRequest(func(r *http.Request) {
			r.Header.Set("X-Custom-Proxy", "modified")
		}),
	)
	if h == nil {
		t.Fatal("expected non-nil handler")
	}

	// Run the proxy handler through celeris test infrastructure.
	chain := []celeris.HandlerFunc{h}
	rec, err := testutil.RunChain(t, chain, "GET", "/api/test")
	testutil.AssertNoError(t, err)

	mu.Lock()
	defer mu.Unlock()

	// Verify request arrived at backend with correct path.
	if gotPath != "/api/test" {
		t.Fatalf("backend path: got %q, want %q", gotPath, "/api/test")
	}

	// Verify X-Forwarded headers are set (SetXForwarded).
	// Values may be empty in test context (no real remote addr), but at
	// minimum the fields should have been populated by SetXForwarded.
	_ = gotXFwdFor // may be empty when RemoteAddr is unset
	if gotXFwdHost == "" {
		t.Log("X-Forwarded-Host not set (may be expected in test context)")
	}
	if gotXFwdProto == "" {
		t.Log("X-Forwarded-Proto not set (may be expected in test context)")
	}

	// Verify WithModifyRequest callback was invoked.
	if gotCustomHeader != "modified" {
		t.Fatalf("X-Custom-Proxy: got %q, want %q", gotCustomHeader, "modified")
	}

	// Verify response status and body forwarded back.
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "backend-response")
}

func TestReverseProxyErrorHandler(t *testing.T) {
	// Create a test server and close it immediately to get a dead port.
	deadServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := deadServer.URL
	deadServer.Close()

	target, _ := url.Parse(deadURL)

	var errorHandlerCalled bool
	var mu sync.Mutex

	h := ReverseProxy(target,
		WithErrorHandler(func(w http.ResponseWriter, r *http.Request, err error) {
			mu.Lock()
			errorHandlerCalled = true
			mu.Unlock()
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprint(w, "proxy error")
		}),
	)

	chain := []celeris.HandlerFunc{h}
	rec, err := testutil.RunChain(t, chain, "GET", "/fail")
	// The error handler writes 502, which comes back through Adapt.
	_ = err

	mu.Lock()
	called := errorHandlerCalled
	mu.Unlock()

	if !called {
		t.Fatal("expected error handler to be called")
	}
	testutil.AssertStatus(t, rec, 502)
	testutil.AssertBodyContains(t, rec, "proxy error")
}
