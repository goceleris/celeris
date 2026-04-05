package requestid

import (
	"testing"

	"github.com/goceleris/celeris/celeristest"
)

func BenchmarkRequestIDGenerate(b *testing.B) {
	mw := New()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/")
		_ = mw(ctx)
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkRequestIDPropagate(b *testing.B) {
	mw := New()
	opts := []celeristest.Option{celeristest.WithHeader("x-request-id", "existing-id")}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/", opts...)
		_ = mw(ctx)
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkUUIDGeneration(b *testing.B) {
	g := newBufferedGenerator()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = g.UUID()
	}
}

func BenchmarkCounterGenerator(b *testing.B) {
	gen := CounterGenerator("req")
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = gen()
	}
}
