package bodylimit

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"

	"github.com/goceleris/celeris/middleware/internal/testutil"
)

func TestAutoSkipBodylessMethods(t *testing.T) {
	mw := New(Config{MaxBytes: 10})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	for _, method := range []string{"GET", "HEAD", "DELETE", "OPTIONS", "TRACE", "CONNECT"} {
		t.Run(method, func(t *testing.T) {
			chain := []celeris.HandlerFunc{mw, handler}
			rec, err := testutil.RunChain(t, chain, method, "/",
				celeristest.WithHeader("content-length", "999999"),
			)
			testutil.AssertNoError(t, err)
			testutil.AssertStatus(t, rec, 200)
		})
	}
}

func TestPOSTNotAutoSkipped(t *testing.T) {
	mw := New(Config{MaxBytes: 10})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "POST", "/",
		celeristest.WithHeader("content-length", "999999"),
	)
	testutil.AssertHTTPError(t, err, 413)
}

func TestSkipPathsBodyLimit(t *testing.T) {
	mw := New(Config{
		MaxBytes:  10,
		SkipPaths: []string{"/upload/large"},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "POST", "/upload/large",
		celeristest.WithHeader("content-length", "999999"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func TestSkipPathsNonMatchStillChecks(t *testing.T) {
	mw := New(Config{
		MaxBytes:  10,
		SkipPaths: []string{"/upload/large"},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "POST", "/upload/small",
		celeristest.WithHeader("content-length", "999999"),
	)
	testutil.AssertHTTPError(t, err, 413)
}

func TestErrorHandlerContentLength(t *testing.T) {
	var receivedErr error
	mw := New(Config{
		MaxBytes: 10,
		ErrorHandler: func(_ *celeris.Context, err error) error {
			receivedErr = err
			return celeris.NewHTTPError(429, "rate limited")
		},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "POST", "/",
		celeristest.WithHeader("content-length", "999999"),
	)
	testutil.AssertHTTPError(t, err, 429)
	if !errors.Is(receivedErr, ErrBodyTooLarge) {
		t.Fatalf("expected ErrBodyTooLarge, got %v", receivedErr)
	}
}

func TestErrorHandlerOnBodyBytes(t *testing.T) {
	var called bool
	mw := New(Config{
		MaxBytes: 10,
		ErrorHandler: func(_ *celeris.Context, _ error) error {
			called = true
			return celeris.NewHTTPError(413, "custom body too big")
		},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	body := make([]byte, 20)
	_, err := testutil.RunChain(t, chain, "POST", "/",
		celeristest.WithBody(body),
	)
	testutil.AssertHTTPError(t, err, 413)
	if !called {
		t.Fatal("expected ErrorHandler to be called for body bytes check")
	}
}

func TestRequestUnderLimitPasses(t *testing.T) {
	mw := New(Config{MaxBytes: 1024})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "POST", "/upload",
		celeristest.WithHeader("content-length", "512"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "ok")
}

func TestRequestOverLimitReturns413(t *testing.T) {
	mw := New(Config{MaxBytes: 1024})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "POST", "/upload",
		celeristest.WithHeader("content-length", "2048"),
	)
	testutil.AssertHTTPError(t, err, 413)
}

func TestRequestNoContentLengthPasses(t *testing.T) {
	mw := New(Config{MaxBytes: 1024})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "POST", "/upload")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func TestRequestExactlyAtLimitPasses(t *testing.T) {
	mw := New(Config{MaxBytes: 1024})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "POST", "/upload",
		celeristest.WithHeader("content-length", "1024"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func TestRequestOneOverLimitRejects(t *testing.T) {
	mw := New(Config{MaxBytes: 1024})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "POST", "/upload",
		celeristest.WithHeader("content-length", "1025"),
	)
	testutil.AssertHTTPError(t, err, 413)
}

func TestCustomMaxBytes(t *testing.T) {
	mw := New(Config{MaxBytes: 100})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "POST", "/upload",
		celeristest.WithHeader("content-length", "200"),
	)
	testutil.AssertHTTPError(t, err, 413)
}

func TestSkipBypassesCheck(t *testing.T) {
	mw := New(Config{
		MaxBytes: 100,
		Skip:     func(_ *celeris.Context) bool { return true },
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "skipped")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "POST", "/upload",
		celeristest.WithHeader("content-length", "999999"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "skipped")
}

func TestDefaultConfig4MB(t *testing.T) {
	if defaultConfig.MaxBytes != 4*1024*1024 {
		t.Fatalf("default MaxBytes: got %d, want %d", defaultConfig.MaxBytes, 4*1024*1024)
	}
}

func TestApplyDefaultsFixesInvalid(t *testing.T) {
	for _, input := range []int64{0, -1, -999} {
		cfg := applyDefaults(Config{MaxBytes: input})
		if cfg.MaxBytes != 4*1024*1024 {
			t.Fatalf("applyDefaults(MaxBytes=%d): got %d, want 4MB", input, cfg.MaxBytes)
		}
	}
}

func TestZeroContentLengthPasses(t *testing.T) {
	mw := New(Config{MaxBytes: 1024})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "POST", "/upload",
		celeristest.WithHeader("content-length", "0"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func TestDefaultConfigNoArgs(t *testing.T) {
	mw := New()
	handler := func(c *celeris.Context) error {
		return c.String(200, "default")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "POST", "/upload",
		celeristest.WithHeader("content-length", "1024"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func TestGETRequestPasses(t *testing.T) {
	mw := New(Config{MaxBytes: 1024})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func FuzzBodyLimitContentLength(f *testing.F) {
	f.Add("0")
	f.Add("1024")
	f.Add("5000000")
	f.Add("-1")
	f.Add("abc")
	f.Add("")
	f.Fuzz(func(t *testing.T, cl string) {
		mw := New(Config{MaxBytes: 4096})
		handler := func(c *celeris.Context) error {
			return c.String(200, "ok")
		}
		chain := []celeris.HandlerFunc{mw, handler}
		opts := []celeristest.Option{}
		if cl != "" {
			opts = append(opts, celeristest.WithHeader("content-length", cl))
		}
		// Must not panic regardless of Content-Length value.
		_, _ = testutil.RunChain(t, chain, "POST", "/fuzz", opts...)
	})
}

func TestActualBodyOverLimit(t *testing.T) {
	mw := New(Config{MaxBytes: 10})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	body := make([]byte, 20)
	_, err := testutil.RunChain(t, chain, "POST", "/upload",
		celeristest.WithBody(body),
	)
	testutil.AssertHTTPError(t, err, 413)
}

func TestActualBodyUnderLimitPasses(t *testing.T) {
	mw := New(Config{MaxBytes: 100})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	body := make([]byte, 50)
	rec, err := testutil.RunChain(t, chain, "POST", "/upload",
		celeristest.WithBody(body),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func TestLyingContentLengthRejected(t *testing.T) {
	mw := New(Config{MaxBytes: 10})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	body := make([]byte, 20)
	_, err := testutil.RunChain(t, chain, "POST", "/upload",
		celeristest.WithBody(body),
		celeristest.WithHeader("content-length", "5"),
	)
	testutil.AssertHTTPError(t, err, 413)
}

func TestNoContentLengthLargeBodyRejected(t *testing.T) {
	mw := New(Config{MaxBytes: 10})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	body := make([]byte, 20)
	_, err := testutil.RunChain(t, chain, "POST", "/upload",
		celeristest.WithBody(body),
	)
	testutil.AssertHTTPError(t, err, 413)
}

func TestLimitStringMB(t *testing.T) {
	mw := New(Config{Limit: "1MB"})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "POST", "/upload",
		celeristest.WithHeader("content-length", "1024"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func TestLimitStringKB(t *testing.T) {
	mw := New(Config{Limit: "10KB"})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "POST", "/upload",
		celeristest.WithHeader("content-length", "20480"),
	)
	testutil.AssertHTTPError(t, err, 413)
}

func TestLimitStringFloat(t *testing.T) {
	mw := New(Config{Limit: "1.5MB"})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "POST", "/upload",
		celeristest.WithHeader("content-length", "1048576"), // 1MB exactly, under 1.5MB
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func TestLimitStringOverridesMaxBytes(t *testing.T) {
	mw := New(Config{Limit: "1KB", MaxBytes: 999999})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "POST", "/upload",
		celeristest.WithHeader("content-length", "2048"),
	)
	testutil.AssertHTTPError(t, err, 413)
}

func TestLimitStringInvalidPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for invalid Limit")
		}
	}()
	New(Config{Limit: "notasize"})
}

func TestLimitStringEmptyPanics(_ *testing.T) {
	// Empty Limit string should NOT panic — it means "use MaxBytes".
	// This is different from a non-empty invalid string.
	mw := New(Config{Limit: "", MaxBytes: 1024})
	_ = mw
}

func TestLimitStringB(t *testing.T) {
	mw := New(Config{Limit: "512B"})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "POST", "/upload",
		celeristest.WithHeader("content-length", "1024"),
	)
	testutil.AssertHTTPError(t, err, 413)
}

func TestLimitStringGB(t *testing.T) {
	mw := New(Config{Limit: "1GB"})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "POST", "/upload",
		celeristest.WithHeader("content-length", "1048576"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func TestLimitStringCaseInsensitive(t *testing.T) {
	mw := New(Config{Limit: "10mb"})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "POST", "/upload",
		celeristest.WithHeader("content-length", "1024"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func TestTBParsing(t *testing.T) {
	mw := New(Config{Limit: "1TB"})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	// 1GB is well under 1TB.
	rec, err := testutil.RunChain(t, chain, "POST", "/upload",
		celeristest.WithHeader("content-length", "1073741824"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	// 2TB exceeds 1TB.
	_, err = testutil.RunChain(t, chain, "POST", "/upload",
		celeristest.WithHeader("content-length", "2199023255552"),
	)
	testutil.AssertHTTPError(t, err, 413)
}

func TestConcurrentBodyLimit(t *testing.T) {
	mw := New(Config{MaxBytes: 1024})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				rec, err := testutil.RunChain(t, chain, "POST", "/upload",
					celeristest.WithHeader("content-length", "512"))
				if err != nil {
					t.Errorf("unexpected error: %v", err)
					return
				}
				if rec.StatusCode != 200 {
					t.Errorf("unexpected status: %d", rec.StatusCode)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestBodyLimitRejectNoExtraAlloc(t *testing.T) {
	mw := New(Config{MaxBytes: 512})
	// Measure baseline allocs from context setup alone.
	baseline := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			ctx, _ := celeristest.NewContext("POST", "/",
				celeristest.WithHeader("content-length", "1024"),
			)
			celeristest.ReleaseContext(ctx)
		}
	})
	// Measure allocs with middleware reject path.
	withMW := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			ctx, _ := celeristest.NewContext("POST", "/",
				celeristest.WithHeader("content-length", "1024"),
			)
			_ = mw(ctx)
			celeristest.ReleaseContext(ctx)
		}
	})
	// The middleware reject path should add at most 1 alloc (for the HTTPError).
	extra := withMW.AllocsPerOp() - baseline.AllocsPerOp()
	if extra > 1 {
		t.Fatalf("bodylimit reject: expected <=1 extra alloc over baseline, got %d (baseline=%d, total=%d)",
			extra, baseline.AllocsPerOp(), withMW.AllocsPerOp())
	}
}

// --- ContentLengthRequired tests ---

func TestContentLengthRequiredRejectsAbsentCL(t *testing.T) {
	mw := New(Config{
		MaxBytes:              1024,
		ContentLengthRequired: true,
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "POST", "/upload")
	testutil.AssertHTTPError(t, err, 411)
}

func TestContentLengthRequiredRejectsInvalidCL(t *testing.T) {
	mw := New(Config{
		MaxBytes:              1024,
		ContentLengthRequired: true,
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "POST", "/upload",
		celeristest.WithHeader("content-length", "bogus"),
	)
	testutil.AssertHTTPError(t, err, 411)
}

func TestContentLengthRequiredAllowsValidCL(t *testing.T) {
	mw := New(Config{
		MaxBytes:              1024,
		ContentLengthRequired: true,
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "POST", "/upload",
		celeristest.WithHeader("content-length", "512"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func TestContentLengthRequiredSkipsBodylessMethods(t *testing.T) {
	mw := New(Config{
		MaxBytes:              10,
		ContentLengthRequired: true,
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	for _, method := range []string{"GET", "HEAD", "DELETE", "OPTIONS", "TRACE", "CONNECT"} {
		t.Run(method, func(t *testing.T) {
			chain := []celeris.HandlerFunc{mw, handler}
			rec, err := testutil.RunChain(t, chain, method, "/")
			testutil.AssertNoError(t, err)
			testutil.AssertStatus(t, rec, 200)
		})
	}
}

func TestContentLengthRequiredErrorHandler(t *testing.T) {
	var receivedErr error
	mw := New(Config{
		MaxBytes:              1024,
		ContentLengthRequired: true,
		ErrorHandler: func(_ *celeris.Context, err error) error {
			receivedErr = err
			return celeris.NewHTTPError(400, "provide content-length")
		},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "POST", "/upload")
	testutil.AssertHTTPError(t, err, 400)
	if !errors.Is(receivedErr, ErrLengthRequired) {
		t.Fatalf("expected ErrLengthRequired, got %v", receivedErr)
	}
}

func TestContentLengthRequiredDisabledByDefault(t *testing.T) {
	mw := New(Config{MaxBytes: 1024})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "POST", "/upload")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func TestContentLengthRequiredWithCLZero(t *testing.T) {
	mw := New(Config{
		MaxBytes:              1024,
		ContentLengthRequired: true,
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "POST", "/upload",
		celeristest.WithHeader("content-length", "0"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func TestContentLengthRequiredRespectSkipPaths(t *testing.T) {
	mw := New(Config{
		MaxBytes:              1024,
		ContentLengthRequired: true,
		SkipPaths:             []string{"/health"},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "POST", "/health")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func TestContentLengthRequiredRespectSkipFunc(t *testing.T) {
	mw := New(Config{
		MaxBytes:              1024,
		ContentLengthRequired: true,
		Skip:                  func(_ *celeris.Context) bool { return true },
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "POST", "/upload")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

// --- parseSize unit tests ---

func TestParseSizeUnits(t *testing.T) {
	tests := []struct {
		input string
		want  int64
	}{
		{"1PB", 1 << 50},
		{"1EB", 1 << 60},
		{"1KiB", 1024},
		{"1MiB", 1 << 20},
		{"1GiB", 1 << 30},
		{"1TiB", 1 << 40},
		{"1PiB", 1 << 50},
		{"1EiB", 1 << 60},
		{"10mib", 10 << 20},
		{"1.5GiB", int64(1.5 * float64(int64(1)<<30))},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			n, err := parseSize(tt.input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if n != tt.want {
				t.Fatalf("%s: got %d, want %d", tt.input, n, tt.want)
			}
		})
	}
}

func TestPBLimitMiddleware(t *testing.T) {
	mw := New(Config{Limit: "1PB"})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "POST", "/upload",
		celeristest.WithHeader("content-length", "1073741824"), // 1GB, under 1PB
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func TestIECLimitMiddleware(t *testing.T) {
	mw := New(Config{Limit: "10KiB"})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "POST", "/upload",
		celeristest.WithHeader("content-length", "20480"), // 20KB > 10KiB
	)
	testutil.AssertHTTPError(t, err, 413)
}

func TestErrLengthRequiredSentinel(t *testing.T) {
	if ErrLengthRequired == nil {
		t.Fatal("ErrLengthRequired should not be nil")
	}
	var httpErr *celeris.HTTPError
	if !errors.As(ErrLengthRequired, &httpErr) {
		t.Fatal("ErrLengthRequired should be an HTTPError")
	}
	if httpErr.Code != 411 {
		t.Fatalf("ErrLengthRequired code: got %d, want 411", httpErr.Code)
	}
}

func TestParseSizeRejectsNegativeMB(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for negative size string")
		}
		msg, ok := r.(string)
		if !ok || msg != "bodylimit: Limit must not be negative" {
			t.Fatalf("unexpected panic message: %v", r)
		}
	}()
	New(Config{Limit: "-1MB"})
}

func TestParseSizeRejectsNegativeFractionalGB(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for negative fractional size string")
		}
		msg, ok := r.(string)
		if !ok || msg != "bodylimit: Limit must not be negative" {
			t.Fatalf("unexpected panic message: %v", r)
		}
	}()
	New(Config{Limit: "-0.5GB"})
}

// --- parseSize error tests ---

func TestParseSizeErrors(t *testing.T) {
	tests := []struct {
		input   string
		wantErr string
	}{
		{"notasize", "bodylimit: invalid Limit format"},
		{"", "bodylimit: empty Limit string"},
		{"-1MB", "bodylimit: Limit must not be negative"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			_, err := parseSize(tt.input)
			if err == nil {
				t.Fatalf("expected error for %q", tt.input)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q should contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestParseSizeReturnsNilOnValid(t *testing.T) {
	n, err := parseSize("10MB")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 10*1024*1024 {
		t.Fatalf("10MB: got %d, want %d", n, 10*1024*1024)
	}
}
