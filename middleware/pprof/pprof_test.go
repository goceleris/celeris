package pprof

import (
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"

	"github.com/goceleris/celeris/middleware/internal/testutil"
)

func openAuth() func(*celeris.Context) bool {
	return func(_ *celeris.Context) bool { return true }
}

func okHandler(c *celeris.Context) error {
	return c.String(200, "ok")
}

func TestPprofIndex(t *testing.T) {
	mw := New(Config{AuthFunc: openAuth()})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/debug/pprof/",
		celeristest.WithRemoteAddr("127.0.0.1:12345"))
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "profile")
}

func TestPprofHeap(t *testing.T) {
	mw := New(Config{AuthFunc: openAuth()})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/debug/pprof/heap",
		celeristest.WithRemoteAddr("127.0.0.1:12345"))
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if len(rec.Body) == 0 {
		t.Fatal("expected non-empty heap profile body")
	}
}

func TestPprofDeniedRemote(t *testing.T) {
	mw := New()
	_, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/debug/pprof/heap",
		celeristest.WithRemoteAddr("1.2.3.4:9999"))
	testutil.AssertHTTPError(t, err, 403)
}

func TestPprofDeniedIPv6Remote(t *testing.T) {
	mw := New()
	_, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/debug/pprof/heap",
		celeristest.WithRemoteAddr("2001:db8::1:9999"))
	testutil.AssertHTTPError(t, err, 403)
}

func TestPprofAllowIPv6Loopback(t *testing.T) {
	mw := New()
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/debug/pprof/heap",
		celeristest.WithRemoteAddr("[::1]:12345"))
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func TestPprofCustomAuth(t *testing.T) {
	mw := New(Config{
		AuthFunc: func(_ *celeris.Context) bool { return true },
	})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/debug/pprof/heap")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func TestPprofCustomPrefix(t *testing.T) {
	mw := New(Config{Prefix: "/profiling", AuthFunc: openAuth()})

	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/profiling/heap")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	// Original prefix should pass through.
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err = testutil.RunChain(t, chain, "GET", "/debug/pprof/heap")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "ok")
}

func TestPprofPassthrough(t *testing.T) {
	mw := New(Config{AuthFunc: openAuth()})
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/api/users")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "ok")
}

func TestPprofNotFoundSubpath(t *testing.T) {
	mw := New(Config{AuthFunc: openAuth()})
	_, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/debug/pprof/nonexistent")
	testutil.AssertHTTPError(t, err, 404)
}

func TestPprofBarePrefix(t *testing.T) {
	mw := New(Config{AuthFunc: openAuth()})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/debug/pprof")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "profile")
}

func TestValidatePanicRootPrefix(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for Prefix=/")
		}
	}()
	New(Config{Prefix: "/"})
}

func TestPprofSkipPaths(t *testing.T) {
	mw := New(Config{
		AuthFunc:  openAuth(),
		SkipPaths: []string{"/debug/pprof/heap"},
	})
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/debug/pprof/heap")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "ok")
}

func TestPprofSkipFunc(t *testing.T) {
	mw := New(Config{
		AuthFunc: openAuth(),
		Skip: func(c *celeris.Context) bool {
			return c.Header("x-skip") == "true"
		},
	})

	// Without skip header: pprof serves the profile.
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/debug/pprof/heap")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if len(rec.Body) == 0 {
		t.Fatal("expected non-empty heap profile body")
	}

	// With skip header: falls through to next handler.
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err = testutil.RunChain(t, chain, "GET", "/debug/pprof/heap",
		celeristest.WithHeader("x-skip", "true"))
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "ok")
}

func TestDefaultConfig(t *testing.T) {
	mw := New()
	// Loopback should work with default config.
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/debug/pprof/heap",
		celeristest.WithRemoteAddr("127.0.0.1:12345"))
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func TestValidatePanicBadPrefix(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for Prefix not starting with /")
		}
		msg, ok := r.(string)
		if !ok || msg != "pprof: Prefix must start with /" {
			t.Fatalf("unexpected panic message: %v", r)
		}
	}()
	New(Config{Prefix: "no-slash"})
}
