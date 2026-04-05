package ratelimit

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"

	"github.com/goceleris/celeris/middleware/internal/fnv1a"
	"github.com/goceleris/celeris/middleware/internal/testutil"
)

func okHandler(c *celeris.Context) error {
	return c.String(200, "ok")
}

func contextHeader(c *celeris.Context, key string) string {
	for _, h := range c.ResponseHeaders() {
		if h[0] == key {
			return h[1]
		}
	}
	return ""
}

func TestAllowUnderLimit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mw := New(Config{RPS: 100, Burst: 5, CleanupInterval: time.Hour, CleanupContext: ctx})

	for range 5 {
		_, err := testutil.RunMiddleware(t, mw, celeristest.WithHeader("x-forwarded-for", "1.2.3.4"))
		testutil.AssertNoError(t, err)
	}
}

func TestBlockOverLimit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mw := New(Config{RPS: 0.001, Burst: 3, CleanupInterval: time.Hour, CleanupContext: ctx})

	// Consume all 3 tokens.
	for range 3 {
		_, err := testutil.RunMiddleware(t, mw, celeristest.WithHeader("x-forwarded-for", "10.0.0.1"))
		testutil.AssertNoError(t, err)
	}

	// 4th request should be blocked.
	_, err := testutil.RunMiddleware(t, mw, celeristest.WithHeader("x-forwarded-for", "10.0.0.1"))
	testutil.AssertHTTPError(t, err, 429)
}

func TestRateLimitHeaders(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mw := New(Config{RPS: 100, Burst: 10, CleanupInterval: time.Hour, CleanupContext: ctx})

	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithHeader("x-forwarded-for", "1.2.3.4"))
	testutil.AssertNoError(t, err)
	testutil.AssertHeader(t, rec, "x-ratelimit-limit", "10")
	testutil.AssertHeader(t, rec, "x-ratelimit-remaining", "9")

	got := rec.Header("x-ratelimit-reset")
	if got == "" {
		t.Fatal("x-ratelimit-reset header missing")
	}
}

func TestHeadersOnDeny(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mw := New(Config{RPS: 0.001, Burst: 1, CleanupInterval: time.Hour, CleanupContext: ctx})

	// Exhaust the single token.
	_, _ = testutil.RunMiddleware(t, mw, celeristest.WithHeader("x-forwarded-for", "1.2.3.4"))

	// Denied request: middleware returns error before c.Next(), so headers are on context but not flushed to recorder.
	c, _ := celeristest.NewContextT(t, "GET", "/", celeristest.WithHeader("x-forwarded-for", "1.2.3.4"))
	err := mw(c)
	testutil.AssertHTTPError(t, err, 429)

	if got := contextHeader(c, "x-ratelimit-limit"); got != "1" {
		t.Fatalf("x-ratelimit-limit: got %q, want %q", got, "1")
	}
	if got := contextHeader(c, "x-ratelimit-remaining"); got != "0" {
		t.Fatalf("x-ratelimit-remaining: got %q, want %q", got, "0")
	}
}

func TestDifferentKeysGetSeparateBuckets(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mw := New(Config{RPS: 0.001, Burst: 1, CleanupInterval: time.Hour, CleanupContext: ctx})

	// IP A uses its token.
	_, err := testutil.RunMiddleware(t, mw, celeristest.WithHeader("x-forwarded-for", "10.0.0.1"))
	testutil.AssertNoError(t, err)

	// IP A is now blocked.
	_, err = testutil.RunMiddleware(t, mw, celeristest.WithHeader("x-forwarded-for", "10.0.0.1"))
	testutil.AssertHTTPError(t, err, 429)

	// IP B should still be allowed (separate bucket).
	_, err = testutil.RunMiddleware(t, mw, celeristest.WithHeader("x-forwarded-for", "10.0.0.2"))
	testutil.AssertNoError(t, err)
}

func TestSkipPaths(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mw := New(Config{
		RPS:             0.001,
		Burst:           1,
		SkipPaths:       []string{"/health"},
		CleanupInterval: time.Hour,
		CleanupContext:  ctx,
	})

	// Exhaust tokens on default path.
	_, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/", celeristest.WithHeader("x-forwarded-for", "1.2.3.4"))
	testutil.AssertNoError(t, err)
	_, err = testutil.RunMiddlewareWithMethod(t, mw, "GET", "/", celeristest.WithHeader("x-forwarded-for", "1.2.3.4"))
	testutil.AssertHTTPError(t, err, 429)

	// /health should bypass rate limiting entirely (no headers set on context).
	c, _ := celeristest.NewContextT(t, "GET", "/health", celeristest.WithHeader("x-forwarded-for", "1.2.3.4"))
	err = mw(c)
	testutil.AssertNoError(t, err)
	if got := contextHeader(c, "x-ratelimit-limit"); got != "" {
		t.Fatalf("expected no x-ratelimit-limit on skipped path, got %q", got)
	}
}

func TestSkipFunction(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mw := New(Config{
		RPS:   0.001,
		Burst: 1,
		Skip: func(c *celeris.Context) bool {
			return c.Header("x-internal") == "true"
		},
		CleanupInterval: time.Hour,
		CleanupContext:  ctx,
	})

	// Exhaust tokens.
	_, err := testutil.RunMiddleware(t, mw, celeristest.WithHeader("x-forwarded-for", "1.2.3.4"))
	testutil.AssertNoError(t, err)
	_, err = testutil.RunMiddleware(t, mw, celeristest.WithHeader("x-forwarded-for", "1.2.3.4"))
	testutil.AssertHTTPError(t, err, 429)

	// Skipped request should pass through (no headers set).
	c, _ := celeristest.NewContextT(t, "GET", "/",
		celeristest.WithHeader("x-forwarded-for", "1.2.3.4"),
		celeristest.WithHeader("x-internal", "true"),
	)
	err = mw(c)
	testutil.AssertNoError(t, err)
	if got := contextHeader(c, "x-ratelimit-limit"); got != "" {
		t.Fatalf("expected no x-ratelimit-limit on skipped request, got %q", got)
	}
}

func TestTokenRefill(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 1000 RPS = 1 token per ms, burst of 1.
	mw := New(Config{RPS: 1000, Burst: 1, CleanupInterval: time.Hour, CleanupContext: ctx})

	// Use the token.
	_, err := testutil.RunMiddleware(t, mw, celeristest.WithHeader("x-forwarded-for", "1.2.3.4"))
	testutil.AssertNoError(t, err)

	// Should be blocked now.
	_, err = testutil.RunMiddleware(t, mw, celeristest.WithHeader("x-forwarded-for", "1.2.3.4"))
	testutil.AssertHTTPError(t, err, 429)

	// Wait for refill (at 1000 RPS, 5ms gives ~5 tokens).
	time.Sleep(5 * time.Millisecond)

	// Should be allowed again.
	_, err = testutil.RunMiddleware(t, mw, celeristest.WithHeader("x-forwarded-for", "1.2.3.4"))
	testutil.AssertNoError(t, err)
}

func TestCustomKeyFunc(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mw := New(Config{
		RPS:   0.001,
		Burst: 1,
		KeyFunc: func(c *celeris.Context) string {
			return c.Header("x-api-key")
		},
		CleanupInterval: time.Hour,
		CleanupContext:  ctx,
	})

	// Key "abc" uses its token.
	_, err := testutil.RunMiddleware(t, mw, celeristest.WithHeader("x-api-key", "abc"))
	testutil.AssertNoError(t, err)

	// Key "abc" is blocked.
	_, err = testutil.RunMiddleware(t, mw, celeristest.WithHeader("x-api-key", "abc"))
	testutil.AssertHTTPError(t, err, 429)

	// Key "xyz" is separate, should be allowed.
	_, err = testutil.RunMiddleware(t, mw, celeristest.WithHeader("x-api-key", "xyz"))
	testutil.AssertNoError(t, err)
}

func TestCustomBurstAndRPS(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mw := New(Config{RPS: 0.001, Burst: 5, CleanupInterval: time.Hour, CleanupContext: ctx})

	// Should allow exactly 5 requests.
	for range 5 {
		_, err := testutil.RunMiddleware(t, mw, celeristest.WithHeader("x-forwarded-for", "1.2.3.4"))
		testutil.AssertNoError(t, err)
	}

	// 6th should fail.
	_, err := testutil.RunMiddleware(t, mw, celeristest.WithHeader("x-forwarded-for", "1.2.3.4"))
	testutil.AssertHTTPError(t, err, 429)
}

func TestHTTP429Error(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mw := New(Config{RPS: 0.001, Burst: 1, CleanupInterval: time.Hour, CleanupContext: ctx})

	_, _ = testutil.RunMiddleware(t, mw, celeristest.WithHeader("x-forwarded-for", "1.2.3.4"))
	_, err := testutil.RunMiddleware(t, mw, celeristest.WithHeader("x-forwarded-for", "1.2.3.4"))
	testutil.AssertError(t, err)
	testutil.AssertHTTPError(t, err, 429)
}

func TestConcurrentAccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mw := New(Config{RPS: 0.001, Burst: 10, CleanupInterval: time.Hour, CleanupContext: ctx})

	var (
		wg       sync.WaitGroup
		allowed  atomic.Int64
		rejected atomic.Int64
	)

	const goroutines = 50
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			c, _ := celeristest.NewContext("GET", "/", celeristest.WithHeader("x-forwarded-for", "1.2.3.4"))
			err := mw(c)
			celeristest.ReleaseContext(c)
			if err != nil {
				rejected.Add(1)
			} else {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()

	a := allowed.Load()
	r := rejected.Load()
	if a+r != goroutines {
		t.Fatalf("total requests: got %d, want %d", a+r, goroutines)
	}
	if a > 10 {
		t.Fatalf("allowed %d requests, expected <= 10 (burst)", a)
	}
	if a < 1 {
		t.Fatal("expected at least 1 allowed request")
	}
}

func TestDefaultConfig(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mw := New(Config{CleanupContext: ctx})

	// Default burst is 20, should allow 20 requests.
	for range 20 {
		_, err := testutil.RunMiddleware(t, mw, celeristest.WithHeader("x-forwarded-for", "1.2.3.4"))
		testutil.AssertNoError(t, err)
	}
}

func TestRemainingDecreases(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mw := New(Config{RPS: 0.001, Burst: 3, CleanupInterval: time.Hour, CleanupContext: ctx})

	chain := []celeris.HandlerFunc{mw, okHandler}

	// First request: remaining should be burst-1 = 2.
	rec, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithHeader("x-forwarded-for", "1.2.3.4"))
	testutil.AssertNoError(t, err)
	testutil.AssertHeader(t, rec, "x-ratelimit-remaining", "2")

	// Second: remaining = 1.
	rec, err = testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithHeader("x-forwarded-for", "1.2.3.4"))
	testutil.AssertNoError(t, err)
	testutil.AssertHeader(t, rec, "x-ratelimit-remaining", "1")

	// Third: remaining = 0.
	rec, err = testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithHeader("x-forwarded-for", "1.2.3.4"))
	testutil.AssertNoError(t, err)
	testutil.AssertHeader(t, rec, "x-ratelimit-remaining", "0")
}

func TestApplyDefaultsFixesZeroBurst(t *testing.T) {
	cfg := applyDefaults(Config{Burst: 0, RPS: 1})
	if cfg.Burst != defaultConfig.Burst {
		t.Fatalf("expected default burst %d, got %d", defaultConfig.Burst, cfg.Burst)
	}
}

func TestCleanupContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately
	// Should not panic or hang.
	mw := New(Config{RPS: 10, Burst: 10, CleanupInterval: time.Millisecond, CleanupContext: ctx})
	_, err := testutil.RunMiddleware(t, mw, celeristest.WithHeader("x-forwarded-for", "1.2.3.4"))
	testutil.AssertNoError(t, err)
}

func TestNextPow2(t *testing.T) {
	cases := [][2]int{{0, 1}, {1, 1}, {2, 2}, {3, 4}, {4, 4}, {5, 8}, {7, 8}, {8, 8}, {9, 16}}
	for _, tc := range cases {
		got := fnv1a.NextPow2(tc[0])
		if got != tc[1] {
			t.Errorf("NextPow2(%d) = %d, want %d", tc[0], got, tc[1])
		}
	}
}

func TestFnv1aDeterministic(t *testing.T) {
	a := fnv1a.Hash("test-key")
	b := fnv1a.Hash("test-key")
	if a != b {
		t.Fatalf("fnv1a not deterministic: %d != %d", a, b)
	}
	c := fnv1a.Hash("different-key")
	if a == c {
		t.Fatal("fnv1a collision on different keys")
	}
}

func TestFormatResetSeconds(t *testing.T) {
	now := int64(0)
	reset := int64(time.Second) // 1 second ahead
	got := formatResetSeconds(reset, now)
	if got != "1" {
		t.Fatalf("formatResetSeconds: got %q, want %q", got, "1")
	}

	// Negative (reset in the past) should clamp to 0.
	got = formatResetSeconds(now, reset)
	if got != "0" {
		t.Fatalf("formatResetSeconds negative: got %q, want %q", got, "0")
	}
}

func TestDisableHeaders(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mw := New(Config{RPS: 100, Burst: 10, DisableHeaders: true, CleanupInterval: time.Hour, CleanupContext: ctx})

	c, _ := celeristest.NewContextT(t, "GET", "/", celeristest.WithHeader("x-forwarded-for", "1.2.3.4"))
	err := mw(c)
	testutil.AssertNoError(t, err)

	if got := contextHeader(c, "x-ratelimit-limit"); got != "" {
		t.Fatalf("expected no x-ratelimit-limit with DisableHeaders, got %q", got)
	}
	if got := contextHeader(c, "x-ratelimit-remaining"); got != "" {
		t.Fatalf("expected no x-ratelimit-remaining with DisableHeaders, got %q", got)
	}
	if got := contextHeader(c, "x-ratelimit-reset"); got != "" {
		t.Fatalf("expected no x-ratelimit-reset with DisableHeaders, got %q", got)
	}
}

func TestDisableHeadersOnDeny(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mw := New(Config{RPS: 0.001, Burst: 1, DisableHeaders: true, CleanupInterval: time.Hour, CleanupContext: ctx})

	c, _ := celeristest.NewContextT(t, "GET", "/", celeristest.WithHeader("x-forwarded-for", "1.2.3.4"))
	_ = mw(c)

	c2, _ := celeristest.NewContextT(t, "GET", "/", celeristest.WithHeader("x-forwarded-for", "1.2.3.4"))
	err := mw(c2)
	testutil.AssertHTTPError(t, err, 429)

	if got := contextHeader(c2, "retry-after"); got != "" {
		t.Fatalf("expected no retry-after with DisableHeaders, got %q", got)
	}
}

func TestLimitReachedCallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	called := false
	mw := New(Config{
		RPS:   0.001,
		Burst: 1,
		LimitReached: func(c *celeris.Context) error {
			called = true
			return c.String(429, "custom rate limit response")
		},
		CleanupInterval: time.Hour,
		CleanupContext:  ctx,
	})

	_, _ = testutil.RunMiddleware(t, mw, celeristest.WithHeader("x-forwarded-for", "1.2.3.4"))

	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/", celeristest.WithHeader("x-forwarded-for", "1.2.3.4"))
	testutil.AssertNoError(t, err)
	if !called {
		t.Fatal("expected LimitReached callback to be called")
	}
	testutil.AssertStatus(t, rec, 429)
	testutil.AssertBodyContains(t, rec, "custom rate limit response")
}

func TestLimitReachedNilUsesDefault(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mw := New(Config{RPS: 0.001, Burst: 1, CleanupInterval: time.Hour, CleanupContext: ctx})

	_, _ = testutil.RunMiddleware(t, mw, celeristest.WithHeader("x-forwarded-for", "1.2.3.4"))
	_, err := testutil.RunMiddleware(t, mw, celeristest.WithHeader("x-forwarded-for", "1.2.3.4"))
	testutil.AssertHTTPError(t, err, 429)
}

func TestRetryAfterHeaderOnDeny(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mw := New(Config{RPS: 0.001, Burst: 1, CleanupInterval: time.Hour, CleanupContext: ctx})

	c, _ := celeristest.NewContextT(t, "GET", "/", celeristest.WithHeader("x-forwarded-for", "1.2.3.4"))
	_ = mw(c)

	c2, _ := celeristest.NewContextT(t, "GET", "/", celeristest.WithHeader("x-forwarded-for", "1.2.3.4"))
	err := mw(c2)
	testutil.AssertHTTPError(t, err, 429)

	retryAfter := contextHeader(c2, "retry-after")
	if retryAfter == "" {
		t.Fatal("expected retry-after header on denied request")
	}
}

func TestRetryAfterNotSetOnAllowed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mw := New(Config{RPS: 100, Burst: 10, CleanupInterval: time.Hour, CleanupContext: ctx})

	c, _ := celeristest.NewContextT(t, "GET", "/", celeristest.WithHeader("x-forwarded-for", "1.2.3.4"))
	err := mw(c)
	testutil.AssertNoError(t, err)

	if got := contextHeader(c, "retry-after"); got != "" {
		t.Fatalf("expected no retry-after on allowed request, got %q", got)
	}
}

func TestLimitReachedHeadersStillSet(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var retryAfter string
	var limitHeader string
	mw := New(Config{
		RPS:   0.001,
		Burst: 1,
		LimitReached: func(c *celeris.Context) error {
			retryAfter = contextHeader(c, "retry-after")
			limitHeader = contextHeader(c, "x-ratelimit-limit")
			return celeris.NewHTTPError(429, "custom")
		},
		CleanupInterval: time.Hour,
		CleanupContext:  ctx,
	})

	c, _ := celeristest.NewContextT(t, "GET", "/", celeristest.WithHeader("x-forwarded-for", "1.2.3.4"))
	_ = mw(c) // consume token

	c2, _ := celeristest.NewContextT(t, "GET", "/", celeristest.WithHeader("x-forwarded-for", "1.2.3.4"))
	err := mw(c2)
	testutil.AssertHTTPError(t, err, 429)

	if limitHeader != "1" {
		t.Fatalf("expected x-ratelimit-limit=1 before LimitReached, got %q", limitHeader)
	}
	if retryAfter == "" {
		t.Fatal("expected retry-after header before LimitReached")
	}
}

func TestParseRate(t *testing.T) {
	tests := []struct {
		input   string
		rps     float64
		burst   int
		wantErr bool
	}{
		{"5-S", 5, 5, false},
		{"100-M", 100.0 / 60, 100, false},
		{"1000-H", 1000.0 / 3600, 1000, false},
		{"86400-D", 1, 86400, false},
		{"10-s", 10, 10, false}, // case insensitive
		{"", 0, 0, true},
		{"100", 0, 0, true},
		{"abc-M", 0, 0, true},
		{"-5-S", 0, 0, true},
		{"100-X", 0, 0, true},
		{"0-M", 0, 0, true},
	}
	for _, tc := range tests {
		rps, burst, err := ParseRate(tc.input)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseRate(%q): expected error", tc.input)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseRate(%q): unexpected error: %v", tc.input, err)
			continue
		}
		if burst != tc.burst {
			t.Errorf("ParseRate(%q): burst=%d, want %d", tc.input, burst, tc.burst)
		}
		diff := rps - tc.rps
		if diff < -0.001 || diff > 0.001 {
			t.Errorf("ParseRate(%q): rps=%f, want %f", tc.input, rps, tc.rps)
		}
	}
}

func TestRateConfigField(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mw := New(Config{Rate: "5-S", CleanupInterval: time.Hour, CleanupContext: ctx})
	// Should allow 5 requests (burst=5).
	for range 5 {
		_, err := testutil.RunMiddleware(t, mw, celeristest.WithHeader("x-forwarded-for", "1.2.3.4"))
		testutil.AssertNoError(t, err)
	}
	// 6th should be blocked.
	_, err := testutil.RunMiddleware(t, mw, celeristest.WithHeader("x-forwarded-for", "1.2.3.4"))
	testutil.AssertHTTPError(t, err, 429)
}

func FuzzKeyExtraction(f *testing.F) {
	f.Add("192.168.1.1")
	f.Add("")
	f.Add("::1")
	f.Add("very-long-key-" + string(make([]byte, 1000)))

	ctx, cancel := context.WithCancel(context.Background())
	f.Cleanup(cancel)
	mw := New(Config{RPS: 1e9, Burst: 1e9, CleanupInterval: time.Hour, CleanupContext: ctx})

	f.Fuzz(func(_ *testing.T, ip string) {
		c, _ := celeristest.NewContext("GET", "/", celeristest.WithHeader("x-forwarded-for", ip))
		_ = mw(c)
		celeristest.ReleaseContext(c)
	})
}

// --- Store interface tests ---

type mockStore struct {
	mu        sync.Mutex
	tokens    map[string]int
	burst     int
	allowErr  error
	undoCalls int
}

func newMockStore(burst int) *mockStore {
	return &mockStore{
		tokens: make(map[string]int),
		burst:  burst,
	}
}

func (s *mockStore) Allow(key string) (bool, int, time.Time, error) {
	if s.allowErr != nil {
		return false, 0, time.Time{}, s.allowErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tokens[key]; !ok {
		s.tokens[key] = s.burst
	}
	if s.tokens[key] <= 0 {
		return false, 0, time.Now().Add(time.Second), nil
	}
	s.tokens[key]--
	return true, s.tokens[key], time.Now().Add(time.Second), nil
}

func (s *mockStore) Undo(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.undoCalls++
	if s.tokens[key] < s.burst {
		s.tokens[key]++
	}
	return nil
}

func TestStoreBasicAllow(t *testing.T) {
	store := newMockStore(3)
	mw := New(Config{
		Store:   store,
		KeyFunc: func(_ *celeris.Context) string { return "test" },
	})

	for range 3 {
		_, err := testutil.RunMiddleware(t, mw)
		testutil.AssertNoError(t, err)
	}

	// 4th should be blocked.
	_, err := testutil.RunMiddleware(t, mw)
	testutil.AssertHTTPError(t, err, 429)
}

func TestStoreHeaders(t *testing.T) {
	store := newMockStore(5)
	mw := New(Config{
		Store:   store,
		Burst:   5,
		KeyFunc: func(_ *celeris.Context) string { return "test" },
	})

	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)

	testutil.AssertHeader(t, rec, "x-ratelimit-limit", "5")
	got := rec.Header("x-ratelimit-remaining")
	if got != "4" {
		t.Fatalf("x-ratelimit-remaining: got %q, want %q", got, "4")
	}
	if rec.Header("x-ratelimit-reset") == "" {
		t.Fatal("expected x-ratelimit-reset header")
	}
}

func TestStoreDisableHeaders(t *testing.T) {
	store := newMockStore(5)
	mw := New(Config{
		Store:          store,
		DisableHeaders: true,
		KeyFunc:        func(_ *celeris.Context) string { return "test" },
	})

	c, _ := celeristest.NewContextT(t, "GET", "/")
	err := mw(c)
	testutil.AssertNoError(t, err)

	if got := contextHeader(c, "x-ratelimit-remaining"); got != "" {
		t.Fatalf("expected no x-ratelimit-remaining with DisableHeaders, got %q", got)
	}
}

func TestStoreError(t *testing.T) {
	store := newMockStore(5)
	store.allowErr = errors.New("connection refused")
	mw := New(Config{
		Store:   store,
		KeyFunc: func(_ *celeris.Context) string { return "test" },
	})

	_, err := testutil.RunMiddleware(t, mw)
	if err == nil {
		t.Fatal("expected error from store")
	}
	if !strings.Contains(err.Error(), "store error") {
		t.Fatalf("expected store error message, got: %v", err)
	}
}

func TestStoreDenyRetryAfter(t *testing.T) {
	store := newMockStore(1)
	mw := New(Config{
		Store:   store,
		KeyFunc: func(_ *celeris.Context) string { return "test" },
	})

	// Exhaust token.
	c, _ := celeristest.NewContextT(t, "GET", "/")
	_ = mw(c)

	// Denied request.
	c2, _ := celeristest.NewContextT(t, "GET", "/")
	err := mw(c2)
	testutil.AssertHTTPError(t, err, 429)

	if got := contextHeader(c2, "retry-after"); got == "" {
		t.Fatal("expected retry-after header on denied request")
	}
}

func TestStoreLimitReached(t *testing.T) {
	store := newMockStore(1)
	called := false
	mw := New(Config{
		Store: store,
		LimitReached: func(c *celeris.Context) error {
			called = true
			return c.String(429, "custom")
		},
		KeyFunc: func(_ *celeris.Context) string { return "test" },
	})

	_, _ = testutil.RunMiddleware(t, mw)
	_, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/")
	testutil.AssertNoError(t, err)
	if !called {
		t.Fatal("expected LimitReached callback")
	}
}

func TestStoreSkipPaths(t *testing.T) {
	store := newMockStore(1)
	mw := New(Config{
		Store:     store,
		SkipPaths: []string{"/health"},
		KeyFunc:   func(_ *celeris.Context) string { return "test" },
	})

	// Exhaust token.
	_, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/")
	testutil.AssertNoError(t, err)

	_, err = testutil.RunMiddlewareWithMethod(t, mw, "GET", "/")
	testutil.AssertHTTPError(t, err, 429)

	// /health should bypass.
	_, err = testutil.RunMiddlewareWithMethod(t, mw, "GET", "/health")
	testutil.AssertNoError(t, err)
}

func TestStoreSkipFunction(t *testing.T) {
	store := newMockStore(1)
	mw := New(Config{
		Store: store,
		Skip: func(c *celeris.Context) bool {
			return c.Header("x-internal") == "true"
		},
		KeyFunc: func(c *celeris.Context) string { return "test" },
	})

	// Exhaust token.
	_, err := testutil.RunMiddleware(t, mw)
	testutil.AssertNoError(t, err)
	_, err = testutil.RunMiddleware(t, mw)
	testutil.AssertHTTPError(t, err, 429)

	// Skipped request should pass.
	_, err = testutil.RunMiddleware(t, mw, celeristest.WithHeader("x-internal", "true"))
	testutil.AssertNoError(t, err)
}

// --- Store without Undo (no StoreUndo interface) ---

type mockStoreNoUndo struct {
	tokens map[string]int
	mu     sync.Mutex
	burst  int
}

func newMockStoreNoUndo(burst int) *mockStoreNoUndo {
	return &mockStoreNoUndo{tokens: make(map[string]int), burst: burst}
}

func (s *mockStoreNoUndo) Allow(key string) (bool, int, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tokens[key]; !ok {
		s.tokens[key] = s.burst
	}
	if s.tokens[key] <= 0 {
		return false, 0, time.Now().Add(time.Second), nil
	}
	s.tokens[key]--
	return true, s.tokens[key], time.Now().Add(time.Second), nil
}

func TestStoreWithoutUndoSkipFailed(t *testing.T) {
	store := newMockStoreNoUndo(2)
	mw := New(Config{
		Store:              store,
		SkipFailedRequests: true,
		KeyFunc:            func(c *celeris.Context) string { return "test" },
	})

	// Handler returns 500. Without Undo, token should NOT be refunded.
	failHandler := func(c *celeris.Context) error {
		return celeris.NewHTTPError(500, "fail")
	}
	chain := []celeris.HandlerFunc{mw, failHandler}

	_, _ = testutil.RunChain(t, chain, "GET", "/")
	_, _ = testutil.RunChain(t, chain, "GET", "/")

	// Both tokens consumed (no undo). 3rd should be blocked.
	_, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertHTTPError(t, err, 429)
}

// --- SkipFailedRequests / SkipSuccessfulRequests tests (built-in limiter) ---

func TestSkipFailedRequests(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mw := New(Config{
		RPS:                0.001,
		Burst:              2,
		SkipFailedRequests: true,
		CleanupInterval:    time.Hour,
		CleanupContext:     ctx,
		KeyFunc:            func(c *celeris.Context) string { return "test" },
	})

	failHandler := func(c *celeris.Context) error {
		return celeris.NewHTTPError(500, "fail")
	}
	chain := []celeris.HandlerFunc{mw, failHandler}

	// Failed requests should not count: send many, tokens should be refunded.
	for range 5 {
		_, err := testutil.RunChain(t, chain, "GET", "/")
		if err == nil {
			t.Fatal("expected error from failHandler")
		}
		testutil.AssertHTTPError(t, err, 500)
	}

	// Now send a successful request — should still be allowed (tokens were refunded).
	_, err := testutil.RunMiddleware(t, mw, celeristest.WithHeader("x-forwarded-for", "1.2.3.4"))
	testutil.AssertNoError(t, err)
}

func TestSkipFailedRequestsCountsSuccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mw := New(Config{
		RPS:                0.001,
		Burst:              2,
		SkipFailedRequests: true,
		CleanupInterval:    time.Hour,
		CleanupContext:     ctx,
		KeyFunc:            func(c *celeris.Context) string { return "test" },
	})

	chain := []celeris.HandlerFunc{mw, okHandler}

	// Successful requests SHOULD count.
	for range 2 {
		_, err := testutil.RunChain(t, chain, "GET", "/")
		testutil.AssertNoError(t, err)
	}

	// 3rd should be blocked.
	_, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertHTTPError(t, err, 429)
}

func TestSkipSuccessfulRequests(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mw := New(Config{
		RPS:                    0.001,
		Burst:                  2,
		SkipSuccessfulRequests: true,
		CleanupInterval:        time.Hour,
		CleanupContext:         ctx,
		KeyFunc:                func(c *celeris.Context) string { return "test" },
	})

	chain := []celeris.HandlerFunc{mw, okHandler}

	// Successful requests should not count: send many, tokens should be refunded.
	for range 5 {
		_, err := testutil.RunChain(t, chain, "GET", "/")
		testutil.AssertNoError(t, err)
	}
}

func TestSkipSuccessfulRequestsCountsFailures(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mw := New(Config{
		RPS:                    0.001,
		Burst:                  2,
		SkipSuccessfulRequests: true,
		CleanupInterval:        time.Hour,
		CleanupContext:         ctx,
		KeyFunc:                func(c *celeris.Context) string { return "test" },
	})

	failHandler := func(c *celeris.Context) error {
		return celeris.NewHTTPError(500, "fail")
	}
	chain := []celeris.HandlerFunc{mw, failHandler}

	// Failed requests SHOULD count.
	for range 2 {
		_, err := testutil.RunChain(t, chain, "GET", "/")
		testutil.AssertHTTPError(t, err, 500)
	}

	// 3rd should be rate limited.
	_, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertHTTPError(t, err, 429)
}

func TestSkipFailedRequestsWith400(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mw := New(Config{
		RPS:                0.001,
		Burst:              1,
		SkipFailedRequests: true,
		CleanupInterval:    time.Hour,
		CleanupContext:     ctx,
		KeyFunc:            func(c *celeris.Context) string { return "test" },
	})

	badReqHandler := func(c *celeris.Context) error {
		return celeris.NewHTTPError(400, "bad request")
	}
	chain := []celeris.HandlerFunc{mw, badReqHandler}

	// 400 is >= 400, so should be skipped (refunded).
	for range 3 {
		_, err := testutil.RunChain(t, chain, "GET", "/")
		testutil.AssertHTTPError(t, err, 400)
	}
}

func TestSkipFailedRequestsStatusFromContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mw := New(Config{
		RPS:                0.001,
		Burst:              1,
		SkipFailedRequests: true,
		CleanupInterval:    time.Hour,
		CleanupContext:     ctx,
		KeyFunc:            func(c *celeris.Context) string { return "test" },
	})

	// Handler that sets status on context (via c.String) then returns nil error.
	handler404 := func(c *celeris.Context) error {
		return c.String(404, "not found")
	}
	chain := []celeris.HandlerFunc{mw, handler404}

	// 404 status set on context (>= 400), should be refunded.
	for range 3 {
		_, err := testutil.RunChain(t, chain, "GET", "/")
		testutil.AssertNoError(t, err)
	}
}

// --- Store + SkipFailedRequests ---

func TestStoreSkipFailedRequests(t *testing.T) {
	store := newMockStore(2)
	mw := New(Config{
		Store:              store,
		SkipFailedRequests: true,
		KeyFunc:            func(c *celeris.Context) string { return "test" },
	})

	failHandler := func(c *celeris.Context) error {
		return celeris.NewHTTPError(500, "fail")
	}
	chain := []celeris.HandlerFunc{mw, failHandler}

	// Failed requests should be refunded via store.Undo.
	for range 5 {
		_, err := testutil.RunChain(t, chain, "GET", "/")
		testutil.AssertHTTPError(t, err, 500)
	}

	if store.undoCalls != 5 {
		t.Fatalf("expected 5 undo calls, got %d", store.undoCalls)
	}
}

func TestStoreSkipSuccessfulRequests(t *testing.T) {
	store := newMockStore(2)
	mw := New(Config{
		Store:                  store,
		SkipSuccessfulRequests: true,
		KeyFunc:                func(c *celeris.Context) string { return "test" },
	})

	chain := []celeris.HandlerFunc{mw, okHandler}

	// Successful requests should be refunded.
	for range 5 {
		_, err := testutil.RunChain(t, chain, "GET", "/")
		testutil.AssertNoError(t, err)
	}

	if store.undoCalls != 5 {
		t.Fatalf("expected 5 undo calls, got %d", store.undoCalls)
	}
}

// --- Undo method on shardedLimiter ---

func TestShardedLimiterUndo(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l := newShardedLimiter(ctx, 4, 0.001, 2, time.Hour)
	now := time.Now().UnixNano()

	// Consume both tokens.
	l.allow("k", now)
	l.allow("k", now)

	// Should be denied.
	allowed, _, _ := l.allow("k", now)
	if allowed {
		t.Fatal("expected denial after consuming all tokens")
	}

	// Undo one token.
	l.undo("k")

	// Should be allowed again.
	allowed, _, _ = l.allow("k", now)
	if !allowed {
		t.Fatal("expected allow after undo")
	}
}

func TestShardedLimiterUndoCapsAtBurst(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l := newShardedLimiter(ctx, 4, 0.001, 2, time.Hour)
	now := time.Now().UnixNano()

	// Consume one token (bucket has 1 left).
	l.allow("k", now)

	// Undo twice — should cap at burst (2).
	l.undo("k")
	l.undo("k")

	// Consume 2 tokens — both should succeed.
	allowed1, _, _ := l.allow("k", now)
	allowed2, _, _ := l.allow("k", now)
	if !allowed1 || !allowed2 {
		t.Fatal("expected both allows after capped undo")
	}

	// 3rd should fail (only had burst=2).
	allowed3, _, _ := l.allow("k", now)
	if allowed3 {
		t.Fatal("expected denial — undo should not exceed burst")
	}
}

func TestShardedLimiterUndoNonexistentKey(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l := newShardedLimiter(ctx, 4, 10, 5, time.Hour)

	// Undo on nonexistent key should not panic.
	l.undo("nonexistent")
}

func FuzzConcurrentAccess(f *testing.F) {
	f.Add("key1", 5)
	f.Add("key2", 10)
	f.Add("", 1)

	ctx, cancel := context.WithCancel(context.Background())
	f.Cleanup(cancel)
	l := newShardedLimiter(ctx, 4, 1e6, 1e6, time.Hour)

	f.Fuzz(func(_ *testing.T, key string, n int) {
		if n < 1 || n > 100 {
			return
		}
		now := time.Now().UnixNano()
		var wg sync.WaitGroup
		wg.Add(n)
		for range n {
			go func() {
				defer wg.Done()
				l.allow(key, now)
			}()
		}
		wg.Wait()
	})
}

// --- DisableKeyRedaction removed (issue #14) ---
// The DisableKeyRedaction field and RedactKey method were dead code
// (never wired into the middleware). They have been removed.

// --- Sliding Window Tests ---

func TestSlidingWindowBasicAllow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mw := New(Config{
		RPS:             0.001,
		Burst:           5,
		SlidingWindow:   true,
		CleanupInterval: time.Hour,
		CleanupContext:  ctx,
	})

	for range 5 {
		_, err := testutil.RunMiddleware(t, mw, celeristest.WithHeader("x-forwarded-for", "1.2.3.4"))
		testutil.AssertNoError(t, err)
	}
}

func TestSlidingWindowBlockOverLimit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mw := New(Config{
		RPS:             0.001,
		Burst:           3,
		SlidingWindow:   true,
		CleanupInterval: time.Hour,
		CleanupContext:  ctx,
	})

	for range 3 {
		_, err := testutil.RunMiddleware(t, mw, celeristest.WithHeader("x-forwarded-for", "10.0.0.1"))
		testutil.AssertNoError(t, err)
	}

	_, err := testutil.RunMiddleware(t, mw, celeristest.WithHeader("x-forwarded-for", "10.0.0.1"))
	testutil.AssertHTTPError(t, err, 429)
}

func TestSlidingWindowHeaders(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mw := New(Config{
		RPS:             100,
		Burst:           10,
		SlidingWindow:   true,
		CleanupInterval: time.Hour,
		CleanupContext:  ctx,
	})

	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithHeader("x-forwarded-for", "1.2.3.4"))
	testutil.AssertNoError(t, err)
	testutil.AssertHeader(t, rec, "x-ratelimit-limit", "10")

	remaining := rec.Header("x-ratelimit-remaining")
	if remaining == "" {
		t.Fatal("expected x-ratelimit-remaining header")
	}
}

func TestSlidingWindowDifferentKeys(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mw := New(Config{
		RPS:             0.001,
		Burst:           1,
		SlidingWindow:   true,
		CleanupInterval: time.Hour,
		CleanupContext:  ctx,
	})

	_, err := testutil.RunMiddleware(t, mw, celeristest.WithHeader("x-forwarded-for", "10.0.0.1"))
	testutil.AssertNoError(t, err)

	_, err = testutil.RunMiddleware(t, mw, celeristest.WithHeader("x-forwarded-for", "10.0.0.1"))
	testutil.AssertHTTPError(t, err, 429)

	_, err = testutil.RunMiddleware(t, mw, celeristest.WithHeader("x-forwarded-for", "10.0.0.2"))
	testutil.AssertNoError(t, err)
}

func TestSlidingWindowUndo(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mw := New(Config{
		RPS:                0.001,
		Burst:              2,
		SlidingWindow:      true,
		SkipFailedRequests: true,
		CleanupInterval:    time.Hour,
		CleanupContext:     ctx,
		KeyFunc:            func(c *celeris.Context) string { return "test" },
	})

	failHandler := func(c *celeris.Context) error {
		return celeris.NewHTTPError(500, "fail")
	}
	chain := []celeris.HandlerFunc{mw, failHandler}

	// Failed requests should be refunded.
	for range 5 {
		_, err := testutil.RunChain(t, chain, "GET", "/")
		testutil.AssertHTTPError(t, err, 500)
	}

	// Should still be able to make a successful request.
	_, err := testutil.RunMiddleware(t, mw)
	testutil.AssertNoError(t, err)
}

func TestSlidingWindowConcurrent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mw := New(Config{
		RPS:             0.001,
		Burst:           10,
		SlidingWindow:   true,
		CleanupInterval: time.Hour,
		CleanupContext:  ctx,
	})

	var (
		wg       sync.WaitGroup
		allowed  atomic.Int64
		rejected atomic.Int64
	)

	const goroutines = 50
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			c, _ := celeristest.NewContext("GET", "/", celeristest.WithHeader("x-forwarded-for", "1.2.3.4"))
			err := mw(c)
			celeristest.ReleaseContext(c)
			if err != nil {
				rejected.Add(1)
			} else {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()

	a := allowed.Load()
	r := rejected.Load()
	if a+r != goroutines {
		t.Fatalf("total requests: got %d, want %d", a+r, goroutines)
	}
	if a > 10 {
		t.Fatalf("allowed %d requests, expected <= 10 (limit)", a)
	}
	if a < 1 {
		t.Fatal("expected at least 1 allowed request")
	}
}

func TestSlidingWindowWithRate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mw := New(Config{
		Rate:            "5-S",
		SlidingWindow:   true,
		CleanupInterval: time.Hour,
		CleanupContext:  ctx,
	})

	for range 5 {
		_, err := testutil.RunMiddleware(t, mw, celeristest.WithHeader("x-forwarded-for", "1.2.3.4"))
		testutil.AssertNoError(t, err)
	}

	_, err := testutil.RunMiddleware(t, mw, celeristest.WithHeader("x-forwarded-for", "1.2.3.4"))
	testutil.AssertHTTPError(t, err, 429)
}

// --- Sliding Window Limiter Unit Tests ---

func TestSlidingWindowLimiterAllow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l := newSlidingWindowLimiter(ctx, 4, 1, 5, time.Hour) // 1 RPS, limit 5

	now := time.Now().UnixNano()
	for range 5 {
		allowed, _, _ := l.allow("k", now)
		if !allowed {
			t.Fatal("expected allow within limit")
		}
	}

	allowed, _, _ := l.allow("k", now)
	if allowed {
		t.Fatal("expected denial over limit")
	}
}

func TestSlidingWindowLimiterUndo(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l := newSlidingWindowLimiter(ctx, 4, 0.001, 2, time.Hour)

	now := time.Now().UnixNano()
	l.allow("k", now)
	l.allow("k", now)

	allowed, _, _ := l.allow("k", now)
	if allowed {
		t.Fatal("expected denial after consuming all tokens")
	}

	l.undo("k")

	allowed, _, _ = l.allow("k", now)
	if !allowed {
		t.Fatal("expected allow after undo")
	}
}

func TestSlidingWindowLimiterUndoNonexistent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l := newSlidingWindowLimiter(ctx, 4, 10, 5, time.Hour)
	l.undo("nonexistent") // should not panic
}

func TestSlidingWindowLimiterWindowAdvance(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 5 per second, limit 5 (window = 5s)
	l := newSlidingWindowLimiter(ctx, 4, 1, 5, time.Hour)

	now := time.Now().UnixNano()
	for range 5 {
		allowed, _, _ := l.allow("k", now)
		if !allowed {
			t.Fatal("expected allow within first window")
		}
	}

	// Denied in same window.
	allowed, _, _ := l.allow("k", now)
	if allowed {
		t.Fatal("expected denial at limit")
	}

	// Advance well past two windows — should reset.
	futureNow := now + 2*l.windowNano + int64(time.Second)
	for range 5 {
		allowed, _, _ := l.allow("k", futureNow)
		if !allowed {
			t.Fatal("expected allow after window reset")
		}
	}
}

// --- RateFunc Tests ---

func TestRateFuncBasic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mw := New(Config{
		RPS:   100,
		Burst: 100,
		RateFunc: func(c *celeris.Context) (string, error) {
			if c.Header("x-tier") == "premium" {
				return "10-S", nil
			}
			return "2-S", nil
		},
		CleanupInterval: time.Hour,
		CleanupContext:  ctx,
		KeyFunc:         func(c *celeris.Context) string { return c.Header("x-user") },
	})

	// Basic user gets 2 per second.
	for range 2 {
		_, err := testutil.RunMiddleware(t, mw,
			celeristest.WithHeader("x-user", "basic"),
			celeristest.WithHeader("x-tier", "basic"))
		testutil.AssertNoError(t, err)
	}
	_, err := testutil.RunMiddleware(t, mw,
		celeristest.WithHeader("x-user", "basic"),
		celeristest.WithHeader("x-tier", "basic"))
	testutil.AssertHTTPError(t, err, 429)

	// Premium user gets 10 per second.
	for range 10 {
		_, err := testutil.RunMiddleware(t, mw,
			celeristest.WithHeader("x-user", "premium"),
			celeristest.WithHeader("x-tier", "premium"))
		testutil.AssertNoError(t, err)
	}
	_, err = testutil.RunMiddleware(t, mw,
		celeristest.WithHeader("x-user", "premium"),
		celeristest.WithHeader("x-tier", "premium"))
	testutil.AssertHTTPError(t, err, 429)
}

func TestRateFuncFallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mw := New(Config{
		RPS:   0.001,
		Burst: 3,
		RateFunc: func(_ *celeris.Context) (string, error) {
			return "", nil // empty string = fallback to static
		},
		CleanupInterval: time.Hour,
		CleanupContext:  ctx,
	})

	for range 3 {
		_, err := testutil.RunMiddleware(t, mw, celeristest.WithHeader("x-forwarded-for", "1.2.3.4"))
		testutil.AssertNoError(t, err)
	}
	_, err := testutil.RunMiddleware(t, mw, celeristest.WithHeader("x-forwarded-for", "1.2.3.4"))
	testutil.AssertHTTPError(t, err, 429)
}

func TestRateFuncError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mw := New(Config{
		RPS:   100,
		Burst: 100,
		RateFunc: func(_ *celeris.Context) (string, error) {
			return "", errors.New("config lookup failed")
		},
		CleanupInterval: time.Hour,
		CleanupContext:  ctx,
	})

	_, err := testutil.RunMiddleware(t, mw, celeristest.WithHeader("x-forwarded-for", "1.2.3.4"))
	if err == nil {
		t.Fatal("expected error from RateFunc")
	}
	if !strings.Contains(err.Error(), "RateFunc error") {
		t.Fatalf("expected RateFunc error message, got: %v", err)
	}
}

func TestRateFuncHeaders(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mw := New(Config{
		RPS:   100,
		Burst: 100,
		RateFunc: func(_ *celeris.Context) (string, error) {
			return "5-S", nil
		},
		CleanupInterval: time.Hour,
		CleanupContext:  ctx,
		KeyFunc:         func(c *celeris.Context) string { return "test" },
	})

	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	// RateFunc returns "5-S" which has burst=5, so limit header should be "5".
	testutil.AssertHeader(t, rec, "x-ratelimit-limit", "5")
}

func TestRateFuncWithSlidingWindow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mw := New(Config{
		RPS:           100,
		Burst:         100,
		SlidingWindow: true,
		RateFunc: func(_ *celeris.Context) (string, error) {
			return "3-S", nil
		},
		CleanupInterval: time.Hour,
		CleanupContext:  ctx,
		KeyFunc:         func(c *celeris.Context) string { return "test" },
	})

	for range 3 {
		_, err := testutil.RunMiddleware(t, mw)
		testutil.AssertNoError(t, err)
	}
	_, err := testutil.RunMiddleware(t, mw)
	testutil.AssertHTTPError(t, err, 429)
}

func TestRateFuncCachesLimiters(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	callCount := 0
	mw := New(Config{
		RPS:   100,
		Burst: 100,
		RateFunc: func(_ *celeris.Context) (string, error) {
			callCount++
			return "5-S", nil
		},
		CleanupInterval: time.Hour,
		CleanupContext:  ctx,
		KeyFunc:         func(c *celeris.Context) string { return "test" },
	})

	// Call multiple times — should reuse cached limiter.
	for range 3 {
		_, err := testutil.RunMiddleware(t, mw)
		testutil.AssertNoError(t, err)
	}

	if callCount != 3 {
		t.Fatalf("expected RateFunc called 3 times, got %d", callCount)
	}
}

// --- Issue #1: Unbounded dynamic limiter map ---

func TestDynamicLimiterMapBounded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	counter := atomic.Int64{}
	mw := New(Config{
		RPS:   100,
		Burst: 100,
		RateFunc: func(_ *celeris.Context) (string, error) {
			n := counter.Add(1)
			return strings.Replace("NNN-S", "NNN", strings.Repeat("1", int(n)), 1), nil
		},
		CleanupInterval:    time.Hour,
		CleanupContext:     ctx,
		KeyFunc:            func(c *celeris.Context) string { return "test" },
		MaxDynamicLimiters: 3,
	})

	// First 3 unique rates should succeed.
	for range 3 {
		_, err := testutil.RunMiddleware(t, mw)
		testutil.AssertNoError(t, err)
	}

	// 4th unique rate should fail with dynamic limiter exhausted error.
	_, err := testutil.RunMiddleware(t, mw)
	if err == nil {
		t.Fatal("expected error when dynamic limiter map full")
	}
	if !strings.Contains(err.Error(), "dynamic limiter map full") {
		t.Fatalf("expected dynamic limiter exhausted error, got: %v", err)
	}
}

// --- Issue #2: Dynamic limiter goroutine leak ---

func TestDynamicLimiterCleanupContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	mw := New(Config{
		RPS:   100,
		Burst: 100,
		RateFunc: func(_ *celeris.Context) (string, error) {
			return "5-S", nil
		},
		CleanupInterval: time.Millisecond,
		CleanupContext:  ctx,
		KeyFunc:         func(c *celeris.Context) string { return "test" },
	})

	// Create a dynamic limiter.
	_, err := testutil.RunMiddleware(t, mw)
	testutil.AssertNoError(t, err)

	// Cancel context — should stop cleanup goroutines (no leak).
	cancel()
	time.Sleep(5 * time.Millisecond) // allow goroutines to exit
}

// --- Issue #4: Sliding window remaining=1 on deny ---

func TestSlidingWindowRemainingZeroOnDeny(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l := newSlidingWindowLimiter(ctx, 4, 0.001, 2, time.Hour)

	now := time.Now().UnixNano()
	l.allow("k", now) // 1 of 2
	l.allow("k", now) // 2 of 2

	// Denied: remaining must be 0, not 1.
	allowed, remaining, _ := l.allow("k", now)
	if allowed {
		t.Fatal("expected denial")
	}
	if remaining != 0 {
		t.Fatalf("expected remaining=0 on deny, got %d", remaining)
	}
}

// --- Issue #9: ParseRate panics from RateFunc ---

func TestRateFuncInvalidRateStringDoesNotPanic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mw := New(Config{
		RPS:   100,
		Burst: 100,
		RateFunc: func(_ *celeris.Context) (string, error) {
			return "not-a-valid-rate", nil // would panic in ParseRate
		},
		CleanupInterval: time.Hour,
		CleanupContext:  ctx,
		KeyFunc:         func(c *celeris.Context) string { return "test" },
	})

	// Should return an error, not panic.
	_, err := testutil.RunMiddleware(t, mw)
	if err == nil {
		t.Fatal("expected error from invalid RateFunc rate string")
	}
	if !strings.Contains(err.Error(), "invalid rate") {
		t.Fatalf("expected 'invalid rate' error, got: %v", err)
	}
}

// --- Issue #10: applyDefaults preserves user Burst when Rate is set ---

func TestApplyDefaultsPreservesUserBurst(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Rate "5-S" would set burst=5, but user explicitly sets Burst=10.
	mw := New(Config{
		Rate:            "5-S",
		Burst:           10,
		CleanupInterval: time.Hour,
		CleanupContext:  ctx,
	})

	// Should allow 10 requests (user's Burst), not 5.
	for range 10 {
		_, err := testutil.RunMiddleware(t, mw, celeristest.WithHeader("x-forwarded-for", "1.2.3.4"))
		testutil.AssertNoError(t, err)
	}
	_, err := testutil.RunMiddleware(t, mw, celeristest.WithHeader("x-forwarded-for", "1.2.3.4"))
	testutil.AssertHTTPError(t, err, 429)
}

// --- Issue #13: Sliding window math.Floor consistency ---

func TestSlidingWindowFloorConsistency(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Use a window where the weighted count is fractional.
	l := newSlidingWindowLimiter(ctx, 4, 1, 5, time.Hour)

	now := time.Now().UnixNano()
	// Fill current window.
	for range 5 {
		l.allow("k", now)
	}

	// Advance halfway into the next window.
	halfWindow := now + l.windowNano + l.windowNano/2
	// prevCount=5, fraction=0.5, weight=5*0.5+0 = 2.5
	// math.Floor(2.5) = 2, so remaining = 5 - 2 - 1 = 2
	allowed, remaining, _ := l.allow("k", halfWindow)
	if !allowed {
		t.Fatal("expected allow at half-window")
	}
	if remaining != 2 {
		t.Fatalf("expected remaining=2 at half-window after one request, got %d", remaining)
	}
}

// --- v1.2.4 celeristest option tests ---

func TestClientIPWithTrustedProxies(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var capturedKey string
	mw := New(Config{
		RPS:             100,
		Burst:           100,
		CleanupInterval: time.Hour,
		CleanupContext:  ctx,
		KeyFunc: func(c *celeris.Context) string {
			key := c.ClientIP()
			capturedKey = key
			return key
		},
	})

	_, err := testutil.RunMiddleware(t, mw,
		celeristest.WithTrustedProxies("10.0.0.0/8"),
		celeristest.WithHeader("x-forwarded-for", "203.0.113.50, 10.0.0.1"),
		celeristest.WithRemoteAddr("10.0.0.1:12345"),
	)
	testutil.AssertNoError(t, err)

	if capturedKey != "203.0.113.50" {
		t.Fatalf("expected rate limit key to be real client IP 203.0.113.50, got %q", capturedKey)
	}
}

// --- ValidateConfig tests ---

func TestValidateConfigValid(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{"zero value", Config{}},
		{"explicit RPS/Burst", Config{RPS: 10, Burst: 20}},
		{"Rate string", Config{Rate: "100-M"}},
		{"Rate with explicit Burst", Config{Rate: "100-M", Burst: 50}},
		{"all fields", Config{RPS: 5, Burst: 10, Shards: 4, CleanupInterval: time.Second, MaxDynamicLimiters: 100}},
		{"zero RPS uses default", Config{RPS: 0}},
		{"zero Burst uses default", Config{Burst: 0}},
		{"zero Shards uses default", Config{Shards: 0}},
		{"zero CleanupInterval uses default", Config{CleanupInterval: 0}},
		{"zero MaxDynamicLimiters uses default", Config{MaxDynamicLimiters: 0}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateConfig(tc.cfg); err != nil {
				t.Fatalf("ValidateConfig returned unexpected error: %v", err)
			}
		})
	}
}

func TestValidateConfigInvalid(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantSub string
	}{
		{"bad Rate format", Config{Rate: "not-valid"}, "Rate:"},
		{"bad Rate unit", Config{Rate: "100-X"}, "Rate:"},
		{"bad Rate count", Config{Rate: "abc-M"}, "Rate:"},
		{"empty Rate count", Config{Rate: "-S"}, "Rate:"},
		{"negative RPS", Config{RPS: -1}, "RPS must be non-negative"},
		{"negative Burst", Config{Burst: -1}, "Burst must be non-negative"},
		{"negative Shards", Config{Shards: -1}, "Shards must be non-negative"},
		{"negative CleanupInterval", Config{CleanupInterval: -time.Second}, "CleanupInterval must be non-negative"},
		{"negative MaxDynamicLimiters", Config{MaxDynamicLimiters: -1}, "MaxDynamicLimiters must be non-negative"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateConfig(tc.cfg)
			if err == nil {
				t.Fatal("ValidateConfig returned nil, want error")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not contain %q", err, tc.wantSub)
			}
		})
	}
}

func TestValidateConfigMultipleErrors(t *testing.T) {
	cfg := Config{
		Rate:               "bad",
		Burst:              -1,
		Shards:             -2,
		CleanupInterval:    -time.Minute,
		MaxDynamicLimiters: -5,
	}
	err := ValidateConfig(cfg)
	if err == nil {
		t.Fatal("ValidateConfig returned nil, want error")
	}
	msg := err.Error()
	for _, sub := range []string{"Rate:", "Burst", "Shards", "CleanupInterval", "MaxDynamicLimiters"} {
		if !strings.Contains(msg, sub) {
			t.Errorf("error %q missing expected substring %q", msg, sub)
		}
	}
}

func TestValidateConfigRateOverridesRPS(t *testing.T) {
	// When Rate is set, RPS is derived from Rate; a negative RPS should not
	// cause an error because Rate takes precedence.
	cfg := Config{Rate: "100-M", RPS: -5}
	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig returned unexpected error: %v", err)
	}
}

func TestValidateConfigNaNRPS(t *testing.T) {
	cfg := Config{RPS: math.NaN()}
	err := ValidateConfig(cfg)
	if err == nil {
		t.Fatal("ValidateConfig returned nil for NaN RPS, want error")
	}
	if !strings.Contains(err.Error(), "NaN") {
		t.Fatalf("error %q does not mention NaN", err)
	}
}
