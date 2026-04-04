package timeout

import (
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

func BenchmarkTimeout(b *testing.B) {
	mw := New(Config{Timeout: 5 * time.Second})
	noop := func(_ *celeris.Context) error { return nil }
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/", celeristest.WithHandlers(mw, noop))
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}
