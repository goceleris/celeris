package rewrite

import (
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

func BenchmarkRewriteNoMatch(b *testing.B) {
	mw := New(Config{
		Rules: []Rule{
			{Pattern: `^/users/(\d+)/posts$`, Replacement: "/api/v2/users/$1/posts"},
		},
	})
	handler := func(c *celeris.Context) error {
		return c.NoContent(200)
	}
	opts := []celeristest.Option{celeristest.WithHandlers(mw, handler)}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/other", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkRewriteMatch(b *testing.B) {
	mw := New(Config{
		Rules: []Rule{
			{Pattern: `^/users/(\d+)/posts$`, Replacement: "/api/v2/users/$1/posts"},
		},
	})
	handler := func(c *celeris.Context) error {
		return c.NoContent(200)
	}
	opts := []celeristest.Option{celeristest.WithHandlers(mw, handler)}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/users/42/posts", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}
