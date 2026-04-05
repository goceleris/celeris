package redirect

import (
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
	"github.com/goceleris/celeris/middleware/internal/testutil"
	"github.com/goceleris/celeris/protocol/h2/stream"
)

func okHandler(c *celeris.Context) error {
	return c.String(200, "ok")
}

// newEmptyHostContext creates a test context with no :authority or host header,
// causing c.Host() to return "". The returned cleanup function must be deferred.
func newEmptyHostContext(t *testing.T, method, path string, chain []celeris.HandlerFunc) (*celeris.Context, *emptyHostRec) {
	t.Helper()
	s := stream.NewStream(1)
	s.Headers = append(s.Headers,
		[2]string{":method", method},
		[2]string{":path", path},
		[2]string{":scheme", "http"},
	)
	rec := &emptyHostRec{}
	s.ResponseWriter = &emptyHostWriter{rec: rec}
	ctx := celeris.AcquireTestContext(s)
	celeris.SetTestStartTime(ctx, time.Now())
	celeris.SetTestHandlers(ctx, chain)
	t.Cleanup(func() {
		celeris.ReleaseTestContext(ctx)
		stream.ResetForPool(s)
	})
	return ctx, rec
}

type emptyHostRec struct {
	mu         sync.Mutex
	statusCode int
	headers    [][2]string
}

func (r *emptyHostRec) status() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.statusCode
}

type emptyHostWriter struct {
	rec *emptyHostRec
}

func (w *emptyHostWriter) WriteResponse(_ *stream.Stream, status int, headers [][2]string, _ []byte) error {
	w.rec.mu.Lock()
	w.rec.statusCode = status
	w.rec.headers = headers
	w.rec.mu.Unlock()
	return nil
}

func emptyHostHasHeader(rec *emptyHostRec, key string) bool {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, h := range rec.headers {
		if h[0] == key {
			return true
		}
	}
	return false
}

// --- HTTPSRedirect ---

func TestHTTPSRedirectHTTPToHTTPS(t *testing.T) {
	mw := HTTPSRedirect()
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/hello")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 301)
	testutil.AssertHeader(t, rec, "location", "https://localhost/hello")
}

func TestHTTPSRedirectHTTPSNoRedirect(t *testing.T) {
	mw := HTTPSRedirect()
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/hello",
		celeristest.WithHeader("x-forwarded-proto", "https"))
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertNoHeader(t, rec, "location")
}

func TestHTTPSRedirectPreservesHostPathQuery(t *testing.T) {
	mw := HTTPSRedirect()
	ctx, rec := celeristest.NewContextT(t, "GET", "/api/v1",
		celeristest.WithQuery("page", "2"))
	ctx.SetHost("example.com")
	err := mw(ctx)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 301)
	testutil.AssertHeader(t, rec, "location", "https://example.com/api/v1?page=2")
}

func TestHTTPSRedirectCustomCode(t *testing.T) {
	mw := HTTPSRedirect(Config{Code: 308})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "POST", "/submit")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 308)
	testutil.AssertHeader(t, rec, "location", "https://localhost/submit")
}

func TestHTTPSRedirectEmptyHost(t *testing.T) {
	mw := HTTPSRedirect()
	chain := []celeris.HandlerFunc{mw, okHandler}
	ctx, rec := newEmptyHostContext(t, "GET", "/page", chain)
	err := ctx.Next()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.status() != 200 {
		t.Fatalf("status: got %d, want 200", rec.status())
	}
	if emptyHostHasHeader(rec, "location") {
		t.Fatal("expected no location header")
	}
}

// --- WWWRedirect ---

func TestWWWRedirectNonWWWToWWW(t *testing.T) {
	mw := WWWRedirect()
	ctx, rec := celeristest.NewContextT(t, "GET", "/page")
	ctx.SetHost("example.com")
	err := mw(ctx)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 301)
	testutil.AssertHeader(t, rec, "location", "http://www.example.com/page")
}

func TestWWWRedirectAlreadyWWW(t *testing.T) {
	mw := WWWRedirect()
	chain := []celeris.HandlerFunc{mw, okHandler}
	ctx, rec := celeristest.NewContextT(t, "GET", "/page",
		celeristest.WithHandlers(chain...))
	ctx.SetHost("www.example.com")
	err := ctx.Next()
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertNoHeader(t, rec, "location")
}

func TestWWWRedirectPreservesQuery(t *testing.T) {
	mw := WWWRedirect()
	ctx, rec := celeristest.NewContextT(t, "GET", "/search",
		celeristest.WithQuery("q", "test"))
	ctx.SetHost("example.com")
	err := mw(ctx)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 301)
	testutil.AssertHeader(t, rec, "location", "http://www.example.com/search?q=test")
}

func TestWWWRedirectCustomCode(t *testing.T) {
	mw := WWWRedirect(Config{Code: 307})
	ctx, rec := celeristest.NewContextT(t, "POST", "/submit")
	ctx.SetHost("example.com")
	err := mw(ctx)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 307)
	testutil.AssertHeader(t, rec, "location", "http://www.example.com/submit")
}

func TestWWWRedirectEmptyHost(t *testing.T) {
	mw := WWWRedirect()
	chain := []celeris.HandlerFunc{mw, okHandler}
	ctx, rec := newEmptyHostContext(t, "GET", "/page", chain)
	err := ctx.Next()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.status() != 200 {
		t.Fatalf("status: got %d, want 200", rec.status())
	}
	if emptyHostHasHeader(rec, "location") {
		t.Fatal("expected no location header")
	}
}

func TestWWWRedirectHostWithPort(t *testing.T) {
	mw := WWWRedirect()
	ctx, rec := celeristest.NewContextT(t, "GET", "/page")
	ctx.SetHost("example.com:8080")
	err := mw(ctx)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 301)
	testutil.AssertHeader(t, rec, "location", "http://www.example.com:8080/page")
}

// --- NonWWWRedirect ---

func TestNonWWWRedirectWWWToNonWWW(t *testing.T) {
	mw := NonWWWRedirect()
	ctx, rec := celeristest.NewContextT(t, "GET", "/page")
	ctx.SetHost("www.example.com")
	err := mw(ctx)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 301)
	testutil.AssertHeader(t, rec, "location", "http://example.com/page")
}

func TestNonWWWRedirectAlreadyNonWWW(t *testing.T) {
	mw := NonWWWRedirect()
	chain := []celeris.HandlerFunc{mw, okHandler}
	ctx, rec := celeristest.NewContextT(t, "GET", "/page",
		celeristest.WithHandlers(chain...))
	ctx.SetHost("example.com")
	err := ctx.Next()
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertNoHeader(t, rec, "location")
}

func TestNonWWWRedirectPreservesQuery(t *testing.T) {
	mw := NonWWWRedirect()
	ctx, rec := celeristest.NewContextT(t, "GET", "/search",
		celeristest.WithQuery("q", "go"))
	ctx.SetHost("www.example.com")
	err := mw(ctx)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 301)
	testutil.AssertHeader(t, rec, "location", "http://example.com/search?q=go")
}

func TestNonWWWRedirectCustomCode(t *testing.T) {
	mw := NonWWWRedirect(Config{Code: 302})
	ctx, rec := celeristest.NewContextT(t, "GET", "/page")
	ctx.SetHost("www.example.com")
	err := mw(ctx)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 302)
}

func TestNonWWWRedirectEmptyHost(t *testing.T) {
	mw := NonWWWRedirect()
	chain := []celeris.HandlerFunc{mw, okHandler}
	ctx, rec := newEmptyHostContext(t, "GET", "/page", chain)
	err := ctx.Next()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.status() != 200 {
		t.Fatalf("status: got %d, want 200", rec.status())
	}
	if emptyHostHasHeader(rec, "location") {
		t.Fatal("expected no location header")
	}
}

func TestNonWWWRedirectHostWithPort(t *testing.T) {
	mw := NonWWWRedirect()
	ctx, rec := celeristest.NewContextT(t, "GET", "/page")
	ctx.SetHost("www.example.com:443")
	err := mw(ctx)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 301)
	testutil.AssertHeader(t, rec, "location", "http://example.com:443/page")
}

// --- TrailingSlashRedirect ---

func TestTrailingSlashRedirectAddsSlash(t *testing.T) {
	mw := TrailingSlashRedirect()
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/foo")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 301)
	testutil.AssertHeader(t, rec, "location", "http://localhost/foo/")
}

func TestTrailingSlashRedirectAlreadyHasSlash(t *testing.T) {
	mw := TrailingSlashRedirect()
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/foo/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertNoHeader(t, rec, "location")
}

func TestTrailingSlashRedirectRootNoRedirect(t *testing.T) {
	mw := TrailingSlashRedirect()
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertNoHeader(t, rec, "location")
}

func TestTrailingSlashRedirectPreservesQuery(t *testing.T) {
	mw := TrailingSlashRedirect()
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/foo",
		celeristest.WithQuery("bar", "1"))
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 301)
	testutil.AssertHeader(t, rec, "location", "http://localhost/foo/?bar=1")
}

func TestTrailingSlashRedirectCustomCode(t *testing.T) {
	mw := TrailingSlashRedirect(Config{Code: 308})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "POST", "/api")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 308)
}

func TestTrailingSlashRedirectEmptyHost(t *testing.T) {
	mw := TrailingSlashRedirect()
	chain := []celeris.HandlerFunc{mw, okHandler}
	ctx, rec := newEmptyHostContext(t, "GET", "/foo", chain)
	err := ctx.Next()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.status() != 200 {
		t.Fatalf("status: got %d, want 200", rec.status())
	}
	if emptyHostHasHeader(rec, "location") {
		t.Fatal("expected no location header")
	}
}

// --- RemoveTrailingSlashRedirect ---

func TestRemoveTrailingSlashRedirectStripsSlash(t *testing.T) {
	mw := RemoveTrailingSlashRedirect()
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/foo/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 301)
	testutil.AssertHeader(t, rec, "location", "http://localhost/foo")
}

func TestRemoveTrailingSlashRedirectNoSlash(t *testing.T) {
	mw := RemoveTrailingSlashRedirect()
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/foo")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertNoHeader(t, rec, "location")
}

func TestRemoveTrailingSlashRedirectRootNoRedirect(t *testing.T) {
	mw := RemoveTrailingSlashRedirect()
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertNoHeader(t, rec, "location")
}

func TestRemoveTrailingSlashPreservesQuery(t *testing.T) {
	mw := RemoveTrailingSlashRedirect()
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/foo/",
		celeristest.WithQuery("bar", "1"))
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 301)
	testutil.AssertHeader(t, rec, "location", "http://localhost/foo?bar=1")
}

func TestRemoveTrailingSlashRedirectEmptyHost(t *testing.T) {
	mw := RemoveTrailingSlashRedirect()
	chain := []celeris.HandlerFunc{mw, okHandler}
	ctx, rec := newEmptyHostContext(t, "GET", "/foo/", chain)
	err := ctx.Next()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.status() != 200 {
		t.Fatalf("status: got %d, want 200", rec.status())
	}
	if emptyHostHasHeader(rec, "location") {
		t.Fatal("expected no location header")
	}
}

// --- SkipPaths / Skip function ---

func TestSkipPathsBypassesRedirect(t *testing.T) {
	mw := HTTPSRedirect(Config{
		SkipPaths: []string{"/health", "/ready"},
	})
	chain := []celeris.HandlerFunc{mw, okHandler}

	rec, err := testutil.RunChain(t, chain, "GET", "/health")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertNoHeader(t, rec, "location")

	rec, err = testutil.RunChain(t, chain, "GET", "/ready")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertNoHeader(t, rec, "location")
}

func TestSkipFunctionBypassesRedirect(t *testing.T) {
	mw := HTTPSRedirect(Config{
		Skip: func(c *celeris.Context) bool {
			return c.Path() == "/skip-me"
		},
	})

	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/skip-me")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertNoHeader(t, rec, "location")

	rec, err = testutil.RunMiddlewareWithMethod(t, mw, "GET", "/do-not-skip")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 301)
	testutil.AssertHeader(t, rec, "location", "https://localhost/do-not-skip")
}

// --- Validate ---

func TestValidatePanicsOnInvalidCode(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for invalid code")
		}
		msg, ok := r.(string)
		if !ok || msg != "redirect: Code must be 300-308" {
			t.Fatalf("unexpected panic: %v", r)
		}
	}()
	HTTPSRedirect(Config{Code: 200})
}

func TestValidatePanicsOnCode299(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for code 299")
		}
	}()
	HTTPSRedirect(Config{Code: 299})
}

func TestValidatePanicsOnCode309(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for code 309")
		}
	}()
	HTTPSRedirect(Config{Code: 309})
}

func TestValidateAcceptsBoundary300(t *testing.T) {
	mw := HTTPSRedirect(Config{Code: 300})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/page")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 300)
}

func TestValidateAcceptsBoundary308(t *testing.T) {
	mw := HTTPSRedirect(Config{Code: 308})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/page")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 308)
}

// --- HTTPSWWWRedirect ---

func TestHTTPSWWWRedirectHTTPNonWWW(t *testing.T) {
	mw := HTTPSWWWRedirect()
	ctx, rec := celeristest.NewContextT(t, "GET", "/page")
	ctx.SetHost("example.com")
	err := mw(ctx)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 301)
	testutil.AssertHeader(t, rec, "location", "https://www.example.com/page")
}

func TestHTTPSWWWRedirectHTTPWithWWW(t *testing.T) {
	mw := HTTPSWWWRedirect()
	ctx, rec := celeristest.NewContextT(t, "GET", "/page")
	ctx.SetHost("www.example.com")
	err := mw(ctx)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 301)
	testutil.AssertHeader(t, rec, "location", "https://www.example.com/page")
}

func TestHTTPSWWWRedirectHTTPSNonWWW(t *testing.T) {
	mw := HTTPSWWWRedirect()
	ctx, rec := celeristest.NewContextT(t, "GET", "/page",
		celeristest.WithHeader("x-forwarded-proto", "https"))
	ctx.SetHost("example.com")
	err := mw(ctx)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 301)
	testutil.AssertHeader(t, rec, "location", "https://www.example.com/page")
}

func TestHTTPSWWWRedirectPassThrough(t *testing.T) {
	mw := HTTPSWWWRedirect()
	chain := []celeris.HandlerFunc{mw, okHandler}
	ctx, rec := celeristest.NewContextT(t, "GET", "/page",
		celeristest.WithHeader("x-forwarded-proto", "https"),
		celeristest.WithHandlers(chain...))
	ctx.SetHost("www.example.com")
	err := ctx.Next()
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertNoHeader(t, rec, "location")
}

func TestHTTPSWWWRedirectPreservesQuery(t *testing.T) {
	mw := HTTPSWWWRedirect()
	ctx, rec := celeristest.NewContextT(t, "GET", "/search",
		celeristest.WithQuery("q", "go"))
	ctx.SetHost("example.com")
	err := mw(ctx)
	testutil.AssertNoError(t, err)
	testutil.AssertHeader(t, rec, "location", "https://www.example.com/search?q=go")
}

func TestHTTPSWWWRedirectEmptyHost(t *testing.T) {
	mw := HTTPSWWWRedirect()
	chain := []celeris.HandlerFunc{mw, okHandler}
	ctx, rec := newEmptyHostContext(t, "GET", "/page", chain)
	err := ctx.Next()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.status() != 200 {
		t.Fatalf("status: got %d, want 200", rec.status())
	}
	if emptyHostHasHeader(rec, "location") {
		t.Fatal("expected no location header")
	}
}

func TestHTTPSWWWRedirectCustomCode(t *testing.T) {
	mw := HTTPSWWWRedirect(Config{Code: 308})
	ctx, rec := celeristest.NewContextT(t, "POST", "/submit")
	ctx.SetHost("example.com")
	err := mw(ctx)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 308)
}

// --- HTTPSNonWWWRedirect ---

func TestHTTPSNonWWWRedirectHTTPWithWWW(t *testing.T) {
	mw := HTTPSNonWWWRedirect()
	ctx, rec := celeristest.NewContextT(t, "GET", "/page")
	ctx.SetHost("www.example.com")
	err := mw(ctx)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 301)
	testutil.AssertHeader(t, rec, "location", "https://example.com/page")
}

func TestHTTPSNonWWWRedirectHTTPNonWWW(t *testing.T) {
	mw := HTTPSNonWWWRedirect()
	ctx, rec := celeristest.NewContextT(t, "GET", "/page")
	ctx.SetHost("example.com")
	err := mw(ctx)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 301)
	testutil.AssertHeader(t, rec, "location", "https://example.com/page")
}

func TestHTTPSNonWWWRedirectHTTPSWithWWW(t *testing.T) {
	mw := HTTPSNonWWWRedirect()
	ctx, rec := celeristest.NewContextT(t, "GET", "/page",
		celeristest.WithHeader("x-forwarded-proto", "https"))
	ctx.SetHost("www.example.com")
	err := mw(ctx)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 301)
	testutil.AssertHeader(t, rec, "location", "https://example.com/page")
}

func TestHTTPSNonWWWRedirectPassThrough(t *testing.T) {
	mw := HTTPSNonWWWRedirect()
	chain := []celeris.HandlerFunc{mw, okHandler}
	ctx, rec := celeristest.NewContextT(t, "GET", "/page",
		celeristest.WithHeader("x-forwarded-proto", "https"),
		celeristest.WithHandlers(chain...))
	ctx.SetHost("example.com")
	err := ctx.Next()
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertNoHeader(t, rec, "location")
}

func TestHTTPSNonWWWRedirectPreservesQuery(t *testing.T) {
	mw := HTTPSNonWWWRedirect()
	ctx, rec := celeristest.NewContextT(t, "GET", "/search",
		celeristest.WithQuery("q", "go"))
	ctx.SetHost("www.example.com")
	err := mw(ctx)
	testutil.AssertNoError(t, err)
	testutil.AssertHeader(t, rec, "location", "https://example.com/search?q=go")
}

func TestHTTPSNonWWWRedirectEmptyHost(t *testing.T) {
	mw := HTTPSNonWWWRedirect()
	chain := []celeris.HandlerFunc{mw, okHandler}
	ctx, rec := newEmptyHostContext(t, "GET", "/page", chain)
	err := ctx.Next()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.status() != 200 {
		t.Fatalf("status: got %d, want 200", rec.status())
	}
	if emptyHostHasHeader(rec, "location") {
		t.Fatal("expected no location header")
	}
}

// --- TrailingSlashRewrite ---

func TestTrailingSlashRewriteAddsSlash(t *testing.T) {
	mw := TrailingSlashRewrite()
	chain := []celeris.HandlerFunc{mw, func(c *celeris.Context) error {
		if c.Path() != "/foo/" {
			t.Fatalf("path: got %q, want %q", c.Path(), "/foo/")
		}
		return c.String(200, "ok")
	}}
	rec, err := testutil.RunChain(t, chain, "GET", "/foo")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertNoHeader(t, rec, "location")
}

func TestTrailingSlashRewriteAlreadyHasSlash(t *testing.T) {
	mw := TrailingSlashRewrite()
	chain := []celeris.HandlerFunc{mw, func(c *celeris.Context) error {
		if c.Path() != "/foo/" {
			t.Fatalf("path: got %q, want %q", c.Path(), "/foo/")
		}
		return c.String(200, "ok")
	}}
	rec, err := testutil.RunChain(t, chain, "GET", "/foo/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func TestTrailingSlashRewriteRootNoChange(t *testing.T) {
	mw := TrailingSlashRewrite()
	chain := []celeris.HandlerFunc{mw, func(c *celeris.Context) error {
		if c.Path() != "/" {
			t.Fatalf("path: got %q, want %q", c.Path(), "/")
		}
		return c.String(200, "ok")
	}}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func TestTrailingSlashRewriteSkipPath(t *testing.T) {
	mw := TrailingSlashRewrite(Config{SkipPaths: []string{"/api"}})
	chain := []celeris.HandlerFunc{mw, func(c *celeris.Context) error {
		if c.Path() != "/api" {
			t.Fatalf("path: got %q, want %q", c.Path(), "/api")
		}
		return c.String(200, "ok")
	}}
	rec, err := testutil.RunChain(t, chain, "GET", "/api")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

// --- RemoveTrailingSlashRewrite ---

func TestRemoveTrailingSlashRewriteStripsSlash(t *testing.T) {
	mw := RemoveTrailingSlashRewrite()
	chain := []celeris.HandlerFunc{mw, func(c *celeris.Context) error {
		if c.Path() != "/foo" {
			t.Fatalf("path: got %q, want %q", c.Path(), "/foo")
		}
		return c.String(200, "ok")
	}}
	rec, err := testutil.RunChain(t, chain, "GET", "/foo/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertNoHeader(t, rec, "location")
}

func TestRemoveTrailingSlashRewriteNoSlash(t *testing.T) {
	mw := RemoveTrailingSlashRewrite()
	chain := []celeris.HandlerFunc{mw, func(c *celeris.Context) error {
		if c.Path() != "/foo" {
			t.Fatalf("path: got %q, want %q", c.Path(), "/foo")
		}
		return c.String(200, "ok")
	}}
	rec, err := testutil.RunChain(t, chain, "GET", "/foo")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func TestRemoveTrailingSlashRewriteRootNoChange(t *testing.T) {
	mw := RemoveTrailingSlashRewrite()
	chain := []celeris.HandlerFunc{mw, func(c *celeris.Context) error {
		if c.Path() != "/" {
			t.Fatalf("path: got %q, want %q", c.Path(), "/")
		}
		return c.String(200, "ok")
	}}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func TestRemoveTrailingSlashRewriteSkipPath(t *testing.T) {
	mw := RemoveTrailingSlashRewrite(Config{SkipPaths: []string{"/api/"}})
	chain := []celeris.HandlerFunc{mw, func(c *celeris.Context) error {
		if c.Path() != "/api/" {
			t.Fatalf("path: got %q, want %q", c.Path(), "/api/")
		}
		return c.String(200, "ok")
	}}
	rec, err := testutil.RunChain(t, chain, "GET", "/api/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

// --- Skip tests for non-HTTPS constructors ---

func TestWWWRedirectSkipPaths(t *testing.T) {
	mw := WWWRedirect(Config{SkipPaths: []string{"/health"}})

	t.Run("skipped", func(t *testing.T) {
		chain := []celeris.HandlerFunc{mw, okHandler}
		ctx, rec := celeristest.NewContextT(t, "GET", "/health",
			celeristest.WithHandlers(chain...))
		ctx.SetHost("example.com")
		err := ctx.Next()
		testutil.AssertNoError(t, err)
		testutil.AssertStatus(t, rec, 200)
		testutil.AssertNoHeader(t, rec, "location")
	})

	t.Run("not skipped", func(t *testing.T) {
		ctx, rec := celeristest.NewContextT(t, "GET", "/other")
		ctx.SetHost("example.com")
		err := mw(ctx)
		testutil.AssertNoError(t, err)
		testutil.AssertStatus(t, rec, 301)
		testutil.AssertHeader(t, rec, "location", "http://www.example.com/other")
	})
}

func TestTrailingSlashRedirectSkipPaths(t *testing.T) {
	mw := TrailingSlashRedirect(Config{SkipPaths: []string{"/health"}})

	t.Run("skipped", func(t *testing.T) {
		chain := []celeris.HandlerFunc{mw, okHandler}
		rec, err := testutil.RunChain(t, chain, "GET", "/health")
		testutil.AssertNoError(t, err)
		testutil.AssertStatus(t, rec, 200)
		testutil.AssertNoHeader(t, rec, "location")
	})

	t.Run("not skipped", func(t *testing.T) {
		rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/other")
		testutil.AssertNoError(t, err)
		testutil.AssertStatus(t, rec, 301)
	})
}

// --- Custom code tests ---

func TestRemoveTrailingSlashRedirectCustomCode(t *testing.T) {
	mw := RemoveTrailingSlashRedirect(Config{Code: 308})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "POST", "/api/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 308)
	testutil.AssertHeader(t, rec, "location", "http://localhost/api")
}

func TestHTTPSNonWWWRedirectCustomCode(t *testing.T) {
	mw := HTTPSNonWWWRedirect(Config{Code: 308})
	ctx, rec := celeristest.NewContextT(t, "POST", "/submit")
	ctx.SetHost("www.example.com")
	err := mw(ctx)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 308)
	testutil.AssertHeader(t, rec, "location", "https://example.com/submit")
}
