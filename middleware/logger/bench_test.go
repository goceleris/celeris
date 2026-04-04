package logger

import (
	"io"
	"log/slog"
	"testing"

	"github.com/goceleris/celeris/celeristest"
)

func BenchmarkLogger(b *testing.B) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mw := New(Config{Output: log})
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/bench")
		_ = mw(ctx)
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkLoggerFastHandler(b *testing.B) {
	log := slog.New(NewFastHandler(io.Discard, nil))
	mw := New(Config{Output: log})
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/bench")
		_ = mw(ctx)
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkLoggerSkipPath(b *testing.B) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mw := New(Config{Output: log, SkipPaths: []string{"/health"}})
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/health")
		_ = mw(ctx)
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkLoggerFastHandlerColor(b *testing.B) {
	log := slog.New(NewFastHandler(io.Discard, &FastHandlerOptions{Color: true}))
	mw := New(Config{Output: log})
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/bench")
		_ = mw(ctx)
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkLoggerAllFields(b *testing.B) {
	log := slog.New(NewFastHandler(io.Discard, nil))
	mw := New(Config{
		Output:         log,
		LogHost:        true,
		LogUserAgent:   true,
		LogReferer:     true,
		LogRoute:       true,
		LogPID:         true,
		LogQueryParams: true,
		LogBytesIn:     true,
		LogScheme:      true,
	})
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/bench?q=hello",
			celeristest.WithHeader("user-agent", "Bench/1.0"),
			celeristest.WithHeader("referer", "https://example.com"),
			celeristest.WithHeader("content-length", "42"),
		)
		_ = mw(ctx)
		celeristest.ReleaseContext(ctx)
	}
}
