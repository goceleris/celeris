package bodylimit

import (
	"testing"

	"github.com/goceleris/celeris/celeristest"
)

func BenchmarkBodyLimitAllow(b *testing.B) {
	mw := New()
	opts := []celeristest.Option{celeristest.WithHeader("content-length", "1024")}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("POST", "/", opts...)
		_ = mw(ctx)
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkBodyLimitDeny(b *testing.B) {
	mw := New(Config{MaxBytes: 1024})
	opts := []celeristest.Option{celeristest.WithHeader("content-length", "999999")}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("POST", "/", opts...)
		_ = mw(ctx)
		celeristest.ReleaseContext(ctx)
	}
}
