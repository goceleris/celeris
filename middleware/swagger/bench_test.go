package swagger

import (
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

var benchSpec = []byte(`{"openapi":"3.0.0","info":{"title":"Bench","version":"1.0"},"paths":{}}`)

func BenchmarkSwaggerPassthrough(b *testing.B) {
	mw := New(Config{SpecContent: benchSpec})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	chain := []celeris.HandlerFunc{mw, handler}
	opts := []celeristest.Option{celeristest.WithHandlers(chain...)}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/api/users", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkSwaggerSpec(b *testing.B) {
	mw := New(Config{SpecContent: benchSpec})
	opts := []celeristest.Option{}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/swagger/spec", opts...)
		_ = mw(ctx)
		celeristest.ReleaseContext(ctx)
	}
}
