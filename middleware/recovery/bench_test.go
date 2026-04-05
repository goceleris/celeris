package recovery

import (
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

func BenchmarkRecoveryNoPanic(b *testing.B) {
	mw := New(Config{DisableLogStack: true})
	noop := func(_ *celeris.Context) error { return nil }
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/bench", celeristest.WithHandlers(mw, noop))
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkRecoveryWithPanic(b *testing.B) {
	mw := New(Config{DisableLogStack: true})
	panicker := func(_ *celeris.Context) error { panic("bench") }
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/bench", celeristest.WithHandlers(mw, panicker))
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}
