package cors

import (
	"testing"

	"github.com/goceleris/celeris/celeristest"
)

func BenchmarkCORSPreflight(b *testing.B) {
	mw := New()
	opts := []celeristest.Option{
		celeristest.WithHeader("origin", "http://example.com"),
		celeristest.WithHeader("access-control-request-method", "POST"),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("OPTIONS", "/", opts...)
		_ = mw(ctx)
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkCORSSimple(b *testing.B) {
	mw := New()
	opts := []celeristest.Option{celeristest.WithHeader("origin", "http://example.com")}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/", opts...)
		_ = mw(ctx)
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkCORSNoOrigin(b *testing.B) {
	mw := New()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/")
		_ = mw(ctx)
		celeristest.ReleaseContext(ctx)
	}
}
