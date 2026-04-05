package benchcmp

import (
	"context"
	"crypto/subtle"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"

	"github.com/goceleris/celeris/middleware/basicauth"
	"github.com/goceleris/celeris/middleware/bodylimit"
	"github.com/goceleris/celeris/middleware/cors"
	"github.com/goceleris/celeris/middleware/csrf"
	"github.com/goceleris/celeris/middleware/healthcheck"
	"github.com/goceleris/celeris/middleware/keyauth"
	"github.com/goceleris/celeris/middleware/logger"
	"github.com/goceleris/celeris/middleware/ratelimit"
	"github.com/goceleris/celeris/middleware/recovery"
	"github.com/goceleris/celeris/middleware/requestid"
	"github.com/goceleris/celeris/middleware/secure"
	"github.com/goceleris/celeris/middleware/timeout"
)

var noop = func(c *celeris.Context) error { return nil }

func BenchmarkLogger_Celeris(b *testing.B) {
	log := slog.New(logger.NewFastHandler(io.Discard, nil))
	mw := logger.New(logger.Config{Output: log})
	opts := []celeristest.Option{celeristest.WithHandlers(mw, noop)}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/bench", opts...)
		ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkLogger_Celeris_TextHandler(b *testing.B) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mw := logger.New(logger.Config{Output: log})
	opts := []celeristest.Option{celeristest.WithHandlers(mw, noop)}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/bench", opts...)
		ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkRecovery_Celeris(b *testing.B) {
	mw := recovery.New(recovery.Config{DisableLogStack: true})
	opts := []celeristest.Option{celeristest.WithHandlers(mw, noop)}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/bench", opts...)
		ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkCORS_Celeris_Preflight(b *testing.B) {
	mw := cors.New()
	opts := []celeristest.Option{
		celeristest.WithHeader("origin", "http://example.com"),
		celeristest.WithHeader("access-control-request-method", "GET"),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("OPTIONS", "/bench", opts...)
		_ = mw(ctx)
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkCORS_Celeris_Simple(b *testing.B) {
	mw := cors.New()
	opts := []celeristest.Option{
		celeristest.WithHeader("origin", "http://example.com"),
		celeristest.WithHandlers(mw, noop),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/bench", opts...)
		ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkRateLimit_Celeris(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mw := ratelimit.New(ratelimit.Config{
		RPS: 1e9, Burst: 1e9,
		CleanupContext: ctx,
	})
	opts := []celeristest.Option{
		celeristest.WithHeader("x-forwarded-for", "1.2.3.4"),
		celeristest.WithHandlers(mw, noop),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		c, _ := celeristest.NewContext("GET", "/bench", opts...)
		c.Next()
		celeristest.ReleaseContext(c)
	}
}

func BenchmarkRequestID_Celeris(b *testing.B) {
	mw := requestid.New()
	opts := []celeristest.Option{celeristest.WithHandlers(mw, noop)}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/bench", opts...)
		ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkTimeout_Celeris(b *testing.B) {
	mw := timeout.New(timeout.Config{Timeout: 5 * time.Second})
	opts := []celeristest.Option{celeristest.WithHandlers(mw, noop)}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/bench", opts...)
		ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkBodyLimit_Celeris(b *testing.B) {
	mw := bodylimit.New()
	opts := []celeristest.Option{
		celeristest.WithHeader("content-length", "1024"),
		celeristest.WithHandlers(mw, noop),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("POST", "/bench", opts...)
		ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkSecure_Celeris(b *testing.B) {
	mw := secure.New()
	opts := []celeristest.Option{celeristest.WithHandlers(mw, noop)}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/bench", opts...)
		ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkKeyAuth_Celeris(b *testing.B) {
	mw := keyauth.New(keyauth.Config{
		Validator: keyauth.StaticKeys("test-api-key"),
	})
	opts := []celeristest.Option{
		celeristest.WithHeader("x-api-key", "test-api-key"),
		celeristest.WithHandlers(mw, noop),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/bench", opts...)
		ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkCSRF_Celeris(b *testing.B) {
	mw := csrf.New()
	opts := []celeristest.Option{celeristest.WithHandlers(mw, noop)}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/bench", opts...)
		ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkBasicAuth_Celeris(b *testing.B) {
	mw := basicauth.New(basicauth.Config{
		Validator: func(user, pass string) bool {
			return subtle.ConstantTimeCompare([]byte(user), []byte("admin")) == 1 &&
				subtle.ConstantTimeCompare([]byte(pass), []byte("secret")) == 1
		},
	})
	opts := []celeristest.Option{
		celeristest.WithBasicAuth("admin", "secret"),
		celeristest.WithHandlers(mw, noop),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/bench", opts...)
		ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkHealthcheck_Celeris(b *testing.B) {
	mw := healthcheck.New()
	opts := []celeristest.Option{celeristest.WithHandlers(mw, noop)}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/health", opts...)
		ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}
