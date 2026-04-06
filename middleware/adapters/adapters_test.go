package adapters

import (
	"net/http"
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
