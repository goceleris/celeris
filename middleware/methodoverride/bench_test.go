package methodoverride

import (
	"strings"
	"testing"

	"github.com/goceleris/celeris/celeristest"
)

func BenchmarkOverrideHeader(b *testing.B) {
	mw := New()
	opts := []celeristest.Option{
		celeristest.WithHeader(strings.ToLower(DefaultHeader), "PUT"),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("POST", "/", opts...)
		_ = mw(ctx)
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkNoOverride(b *testing.B) {
	mw := New()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/")
		_ = mw(ctx)
		celeristest.ReleaseContext(ctx)
	}
}
