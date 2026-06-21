package ratelimit

import (
	"context"
	"testing"
	"time"

	"github.com/goceleris/celeris/celeristest"
)

func BenchmarkRateLimitAllow(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mw := New(Config{RPS: 1e9, Burst: 1e9, CleanupContext: ctx})
	opts := []celeristest.Option{celeristest.WithHeader("x-forwarded-for", "1.2.3.4")}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		c, _ := celeristest.NewContext("GET", "/", opts...)
		_ = mw(c)
		celeristest.ReleaseContext(c)
	}
}

// BenchmarkRateLimitAllowRealistic locks in the under-limit fast path's
// zero-allocation property for a realistic limiter (Burst within the
// cached-int range). The x-ratelimit-* header values resolve through the
// smallInts cache and SetHeaderTrust appends into the Context's inline
// header buffer, so the common "request allowed" path allocates nothing.
// (BenchmarkRateLimitAllow above uses an unrealistic Burst of 1e9, whose
// remaining count overflows the int cache into strconv — that single alloc
// is a benchmark artifact, not the hot path.)
func BenchmarkRateLimitAllowRealistic(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mw := New(Config{RPS: 1e9, Burst: 100, CleanupContext: ctx})
	opts := []celeristest.Option{celeristest.WithHeader("x-forwarded-for", "1.2.3.4")}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		c, _ := celeristest.NewContext("GET", "/", opts...)
		_ = mw(c)
		celeristest.ReleaseContext(c)
	}
}

func BenchmarkRateLimitDeny(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mw := New(Config{RPS: 0.001, Burst: 1, CleanupContext: ctx})
	opts := []celeristest.Option{celeristest.WithHeader("x-forwarded-for", "1.2.3.4")}
	// Exhaust the single token.
	c, _ := celeristest.NewContext("GET", "/", opts...)
	_ = mw(c)
	celeristest.ReleaseContext(c)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		c, _ := celeristest.NewContext("GET", "/", opts...)
		_ = mw(c)
		celeristest.ReleaseContext(c)
	}
}

func BenchmarkShardedLimiter(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l := newShardedLimiter(ctx, 16, 1e9, 1e9, time.Minute)
	now := time.Now().UnixNano()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		l.allow("key", now)
	}
}
