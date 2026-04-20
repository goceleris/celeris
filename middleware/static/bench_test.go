package static

import (
	"testing"
	"testing/fstest"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

func BenchmarkStaticPassthrough(b *testing.B) {
	mapFS := fstest.MapFS{
		"index.html": {Data: []byte("<html></html>")},
	}
	mw := New(Config{FS: mapFS})
	noop := func(_ *celeris.Context) error { return nil }
	opts := []celeristest.Option{
		celeristest.WithHandlers(mw, noop),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("POST", "/data", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkStaticServeFromFS(b *testing.B) {
	mapFS := fstest.MapFS{
		"style.css": {Data: []byte("body{margin:0}")},
	}
	mw := New(Config{FS: mapFS})
	noop := func(_ *celeris.Context) error { return nil }
	opts := []celeristest.Option{
		celeristest.WithHandlers(mw, noop),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/style.css", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

// BenchmarkStaticServeWithCacheHeaders exercises setCacheHeaders (ETag +
// Cache-Control). Without ModTime the default bench skips that branch.
func BenchmarkStaticServeWithCacheHeaders(b *testing.B) {
	mapFS := fstest.MapFS{
		"style.css": {Data: []byte("body{margin:0}"), ModTime: time.Unix(1_700_000_000, 0)},
	}
	mw := New(Config{FS: mapFS, MaxAge: time.Hour})
	noop := func(_ *celeris.Context) error { return nil }
	opts := []celeristest.Option{
		celeristest.WithHandlers(mw, noop),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/style.css", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}
