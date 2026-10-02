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

// BenchmarkStaticServeRange serves a single byte range of a cached file
// (celeris#435: the range is decided by internal/httprange).
func BenchmarkStaticServeRange(b *testing.B) {
	mapFS := fstest.MapFS{
		"style.css": {Data: []byte("body{margin:0}"), ModTime: time.Unix(1_700_000_000, 0)},
	}
	mw := New(Config{FS: mapFS, MaxAge: time.Hour})
	noop := func(_ *celeris.Context) error { return nil }
	opts := []celeristest.Option{
		celeristest.WithHandlers(mw, noop),
		celeristest.WithHeader("range", "bytes=2-9"),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/style.css", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

// BenchmarkStaticServeRangeIfRange is BenchmarkStaticServeRange with an
// If-Range date that matches, so the range is served (celeris#435).
func BenchmarkStaticServeRangeIfRange(b *testing.B) {
	mod := time.Unix(1_700_000_000, 0)
	mapFS := fstest.MapFS{
		"style.css": {Data: []byte("body{margin:0}"), ModTime: mod},
	}
	mw := New(Config{FS: mapFS, MaxAge: time.Hour})
	noop := func(_ *celeris.Context) error { return nil }
	opts := []celeristest.Option{
		celeristest.WithHandlers(mw, noop),
		celeristest.WithHeader("range", "bytes=2-9"),
		celeristest.WithHeader("if-range", mod.UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT")),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/style.css", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}
