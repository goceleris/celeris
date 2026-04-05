package requestid

import (
	"regexp"
	"strings"
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"

	"github.com/goceleris/celeris/middleware/internal/testutil"
)

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func okHandler(c *celeris.Context) error {
	return c.String(200, "ok")
}

func TestGeneratesIDWhenAbsent(t *testing.T) {
	mw := New()
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	id := rec.Header("x-request-id")
	if id == "" {
		t.Fatal("expected x-request-id header to be set")
	}
	if !uuidRe.MatchString(id) {
		t.Fatalf("expected UUID v4 format, got %q", id)
	}
}

func TestPropagatesExistingHeader(t *testing.T) {
	mw := New()
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithHeader("x-request-id", "existing-id"))
	testutil.AssertNoError(t, err)
	testutil.AssertHeader(t, rec, "x-request-id", "existing-id")
}

func TestSetsResponseHeader(t *testing.T) {
	mw := New()
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	id := rec.Header("x-request-id")
	if id == "" {
		t.Fatal("expected response header x-request-id")
	}
}

func TestSetsContextStore(t *testing.T) {
	mw := New()
	var stored string
	var found bool
	handler := func(c *celeris.Context) error {
		val, ok := c.Get("request_id")
		found = ok
		if ok {
			stored, _ = val.(string)
		}
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	if !found {
		t.Fatal("expected request_id in context store")
	}
	if stored == "" {
		t.Fatal("expected non-empty string in context store")
	}
}

func TestCustomGenerator(t *testing.T) {
	mw := New(Config{
		Generator: func() string { return "custom-123" },
	})
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertHeader(t, rec, "x-request-id", "custom-123")
}

func TestCounterGeneratorMonotonic(t *testing.T) {
	gen := CounterGenerator("req")
	for i := range 100 {
		id := gen()
		expected := "req-" + formatUint(uint64(i+1))
		if id != expected {
			t.Fatalf("iteration %d: got %q, want %q", i, id, expected)
		}
	}
}

func TestCounterGeneratorUnique(t *testing.T) {
	gen := CounterGenerator("test")
	seen := make(map[string]struct{}, 1000)
	for range 1000 {
		id := gen()
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate ID: %s", id)
		}
		seen[id] = struct{}{}
	}
}

func TestUUIDFormat(t *testing.T) {
	g := newBufferedGenerator()
	for range 100 {
		id := g.UUID()
		if len(id) != 36 {
			t.Fatalf("expected length 36, got %d: %q", len(id), id)
		}
		if id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
			t.Fatalf("invalid dash positions: %q", id)
		}
		// Version 4: character at index 14 must be '4'.
		if id[14] != '4' {
			t.Fatalf("expected version 4 at index 14, got %c: %q", id[14], id)
		}
		// Variant: character at index 19 must be 8, 9, a, or b.
		if !strings.ContainsRune("89ab", rune(id[19])) {
			t.Fatalf("expected variant 10xx at index 19, got %c: %q", id[19], id)
		}
	}
}

func TestUUIDUniqueness(t *testing.T) {
	g := newBufferedGenerator()
	seen := make(map[string]struct{}, 1000)
	for range 1000 {
		id := g.UUID()
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate UUID: %s", id)
		}
		seen[id] = struct{}{}
	}
}

func TestCustomHeaderName(t *testing.T) {
	mw := New(Config{Header: "x-trace-id"})
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	id := rec.Header("x-trace-id")
	if id == "" {
		t.Fatal("expected x-trace-id header")
	}
	testutil.AssertNoHeader(t, rec, "x-request-id")
}

func TestCustomHeaderPropagation(t *testing.T) {
	mw := New(Config{Header: "x-trace-id"})
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithHeader("x-trace-id", "trace-abc"))
	testutil.AssertNoError(t, err)
	testutil.AssertHeader(t, rec, "x-trace-id", "trace-abc")
}

func TestSkipFunction(t *testing.T) {
	mw := New(Config{
		Skip: func(c *celeris.Context) bool {
			return c.Path() == "/health"
		},
	})
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/health")
	testutil.AssertNoError(t, err)
	testutil.AssertNoHeader(t, rec, "x-request-id")
}

func TestSkipFunctionAllowsNonSkipped(t *testing.T) {
	mw := New(Config{
		Skip: func(c *celeris.Context) bool {
			return c.Path() == "/health"
		},
	})
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	id := rec.Header("x-request-id")
	if id == "" {
		t.Fatal("expected x-request-id for non-skipped path")
	}
}

func TestCallsNext(t *testing.T) {
	mw := New()
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "ok")
}

func FuzzHeaderPropagation(f *testing.F) {
	f.Add("abc-123")
	f.Add("")
	f.Add("spaces in id")
	f.Add(strings.Repeat("x", 128))
	f.Fuzz(func(t *testing.T, headerVal string) {
		mw := New()
		var opts []celeristest.Option
		if headerVal != "" {
			opts = append(opts, celeristest.WithHeader("x-request-id", headerVal))
		}
		ctx, rec := celeristest.NewContextT(t, "GET", "/",
			append(opts, celeristest.WithHandlers(mw, okHandler))...)
		err := ctx.Next()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		id := rec.Header("x-request-id")
		if id == "" {
			t.Fatal("expected x-request-id to always be set")
		}
		if validID(headerVal) && id != headerVal {
			t.Fatalf("expected propagated value %q, got %q", headerVal, id)
		}
	})
}

func FuzzGeneratorOutputFormat(f *testing.F) {
	f.Add(0)
	f.Add(1)
	f.Add(255)
	f.Fuzz(func(t *testing.T, n int) {
		g := newBufferedGenerator()
		// Use the fuzz input to vary the number of UUIDs generated,
		// exercising buffer refill paths at different offsets.
		count := (n % 512) + 1
		for range count {
			id := g.UUID()
			if !uuidRe.MatchString(id) {
				t.Fatalf("invalid UUID format: %q", id)
			}
		}
	})
}

func TestSkipPathsBypassesID(t *testing.T) {
	mw := New(Config{
		SkipPaths: []string{"/health", "/ready"},
	})
	chain := []celeris.HandlerFunc{mw, okHandler}

	// Skipped path should have no request ID header.
	rec, err := testutil.RunChain(t, chain, "GET", "/health")
	testutil.AssertNoError(t, err)
	testutil.AssertNoHeader(t, rec, "x-request-id")

	// Another skipped path.
	rec, err = testutil.RunChain(t, chain, "GET", "/ready")
	testutil.AssertNoError(t, err)
	testutil.AssertNoHeader(t, rec, "x-request-id")

	// Non-skipped path should generate an ID.
	rec, err = testutil.RunChain(t, chain, "GET", "/api/users")
	testutil.AssertNoError(t, err)
	id := rec.Header("x-request-id")
	if id == "" {
		t.Fatal("expected x-request-id for non-skipped path")
	}
}

func TestSkipPathsExactMatch(t *testing.T) {
	mw := New(Config{
		SkipPaths: []string{"/health"},
	})
	chain := []celeris.HandlerFunc{mw, okHandler}

	// Exact match: /health should be skipped.
	rec, err := testutil.RunChain(t, chain, "GET", "/health")
	testutil.AssertNoError(t, err)
	testutil.AssertNoHeader(t, rec, "x-request-id")

	// /health/ is a distinct path and should NOT be skipped.
	rec, err = testutil.RunChain(t, chain, "GET", "/health/")
	testutil.AssertNoError(t, err)
	id := rec.Header("x-request-id")
	if id == "" {
		t.Fatal("expected x-request-id for /health/ (not in SkipPaths)")
	}
}

func TestFromContextReturnsID(t *testing.T) {
	mw := New()
	var id string
	handler := func(c *celeris.Context) error {
		id = FromContext(c)
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	if id == "" {
		t.Fatal("FromContext returned empty string")
	}
}

func TestFromContextNoMiddleware(t *testing.T) {
	ctx, _ := celeristest.NewContextT(t, "GET", "/")
	if got := FromContext(ctx); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestRejectsNonPrintableASCII(t *testing.T) {
	mw := New()
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithHeader("x-request-id", "bad\x00id"))
	testutil.AssertNoError(t, err)
	id := rec.Header("x-request-id")
	if id == "bad\x00id" {
		t.Fatal("expected invalid ID to be replaced")
	}
	if !uuidRe.MatchString(id) {
		t.Fatalf("expected fresh UUID, got %q", id)
	}
}

func TestRejectsControlChars(t *testing.T) {
	mw := New()
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithHeader("x-request-id", "bad\nid"))
	testutil.AssertNoError(t, err)
	id := rec.Header("x-request-id")
	if id == "bad\nid" {
		t.Fatal("expected invalid ID to be replaced")
	}
}

func TestRejectsTooLongID(t *testing.T) {
	mw := New()
	chain := []celeris.HandlerFunc{mw, okHandler}
	longID := strings.Repeat("a", 129)
	rec, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithHeader("x-request-id", longID))
	testutil.AssertNoError(t, err)
	id := rec.Header("x-request-id")
	if id == longID {
		t.Fatal("expected too-long ID to be replaced")
	}
}

func TestAcceptsMaxLengthID(t *testing.T) {
	mw := New()
	chain := []celeris.HandlerFunc{mw, okHandler}
	maxID := strings.Repeat("a", 128)
	rec, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithHeader("x-request-id", maxID))
	testutil.AssertNoError(t, err)
	testutil.AssertHeader(t, rec, "x-request-id", maxID)
}

func TestAcceptsPrintableASCII(t *testing.T) {
	mw := New()
	chain := []celeris.HandlerFunc{mw, okHandler}
	validChars := "abc-123_XYZ!@#$%^&*() ~"
	rec, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithHeader("x-request-id", validChars))
	testutil.AssertNoError(t, err)
	testutil.AssertHeader(t, rec, "x-request-id", validChars)
}

func TestRejectsHighBytes(t *testing.T) {
	mw := New()
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithHeader("x-request-id", "id\x7Fhigh"))
	testutil.AssertNoError(t, err)
	id := rec.Header("x-request-id")
	if id == "id\x7Fhigh" {
		t.Fatal("expected ID with DEL char to be replaced")
	}
}

// --- Generator retry with fallback tests ---

func TestGeneratorRetryOnEmptyString(t *testing.T) {
	var callCount int
	mw := New(Config{
		Generator: func() string {
			callCount++
			if callCount <= 2 {
				return ""
			}
			return "retry-success"
		},
	})
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertHeader(t, rec, "x-request-id", "retry-success")
	if callCount != 3 {
		t.Fatalf("expected 3 generator calls, got %d", callCount)
	}
}

func TestGeneratorFallbackToUUIDAfterMaxRetries(t *testing.T) {
	var callCount int
	mw := New(Config{
		Generator: func() string {
			callCount++
			return ""
		},
	})
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	id := rec.Header("x-request-id")
	if id == "" {
		t.Fatal("expected fallback UUID, got empty string")
	}
	if !uuidRe.MatchString(id) {
		t.Fatalf("expected fallback UUID format, got %q", id)
	}
	if callCount != 3 {
		t.Fatalf("expected 3 generator calls (1 initial + 2 retries), got %d", callCount)
	}
}

func TestGeneratorSuccessOnFirstCallNoRetry(t *testing.T) {
	var callCount int
	mw := New(Config{
		Generator: func() string {
			callCount++
			return "first-try"
		},
	})
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertHeader(t, rec, "x-request-id", "first-try")
	if callCount != 1 {
		t.Fatalf("expected 1 generator call, got %d", callCount)
	}
}

func TestGeneratorRetrySucceedsOnSecondAttempt(t *testing.T) {
	var callCount int
	mw := New(Config{
		Generator: func() string {
			callCount++
			if callCount == 1 {
				return ""
			}
			return "second-attempt"
		},
	})
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertHeader(t, rec, "x-request-id", "second-attempt")
	if callCount != 2 {
		t.Fatalf("expected 2 generator calls, got %d", callCount)
	}
}

// --- DisableTrustProxy tests ---

func TestDisableTrustProxyDefaultAcceptsInbound(t *testing.T) {
	// Zero-value DisableTrustProxy=false means inbound headers are trusted.
	mw := New()
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithHeader("x-request-id", "from-proxy"))
	testutil.AssertNoError(t, err)
	testutil.AssertHeader(t, rec, "x-request-id", "from-proxy")
}

func TestDisableTrustProxyTrueIgnoresInbound(t *testing.T) {
	mw := New(Config{DisableTrustProxy: true})
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithHeader("x-request-id", "should-be-ignored"))
	testutil.AssertNoError(t, err)
	id := rec.Header("x-request-id")
	if id == "should-be-ignored" {
		t.Fatal("expected inbound header to be ignored when DisableTrustProxy=true")
	}
	if id == "" {
		t.Fatal("expected a fresh ID to be generated")
	}
	if !uuidRe.MatchString(id) {
		t.Fatalf("expected UUID v4 format, got %q", id)
	}
}

func TestDisableTrustProxyTrueNoInboundHeader(t *testing.T) {
	mw := New(Config{DisableTrustProxy: true})
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	id := rec.Header("x-request-id")
	if id == "" {
		t.Fatal("expected a fresh ID to be generated")
	}
	if !uuidRe.MatchString(id) {
		t.Fatalf("expected UUID v4 format, got %q", id)
	}
}

func TestDisableTrustProxyTrueWithCustomGenerator(t *testing.T) {
	mw := New(Config{
		DisableTrustProxy: true,
		Generator:         func() string { return "generated-456" },
	})
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithHeader("x-request-id", "from-proxy"))
	testutil.AssertNoError(t, err)
	testutil.AssertHeader(t, rec, "x-request-id", "generated-456")
}

func TestDisableTrustProxyTrueContextStore(t *testing.T) {
	mw := New(Config{DisableTrustProxy: true})
	var stored string
	handler := func(c *celeris.Context) error {
		stored = FromContext(c)
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithHeader("x-request-id", "spoofed"))
	testutil.AssertNoError(t, err)
	if stored == "spoofed" {
		t.Fatal("context store should not contain spoofed ID")
	}
	if stored == "" {
		t.Fatal("context store should contain a generated ID")
	}
}

// --- FromStdContext tests ---

func TestFromStdContextReturnsID(t *testing.T) {
	mw := New(Config{EnableStdContext: true})
	var stdID string
	handler := func(c *celeris.Context) error {
		stdID = FromStdContext(c.Context())
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	headerID := rec.Header("x-request-id")
	if stdID == "" {
		t.Fatal("FromStdContext returned empty string")
	}
	if stdID != headerID {
		t.Fatalf("FromStdContext %q != response header %q", stdID, headerID)
	}
}

func TestFromStdContextNoMiddleware(t *testing.T) {
	ctx, _ := celeristest.NewContextT(t, "GET", "/")
	if got := FromStdContext(ctx.Context()); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestFromStdContextPropagated(t *testing.T) {
	mw := New(Config{EnableStdContext: true})
	var stdID string
	handler := func(c *celeris.Context) error {
		stdID = FromStdContext(c.Context())
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithHeader("x-request-id", "propagated-std"))
	testutil.AssertNoError(t, err)
	if stdID != "propagated-std" {
		t.Fatalf("FromStdContext got %q, want %q", stdID, "propagated-std")
	}
}
