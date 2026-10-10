package static

import (
	"os"
	"path/filepath"
	"strconv"
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

// rootBench846 writes app.js (4 KiB) and app.js.gz (1 KiB) under a temp dir
// and returns the middleware serving it (celeris#846: the Root path's
// validators and 304 come from the file as it is opened, and from the
// pre-compressed variant when one is served).
func rootBench846(b *testing.B, compress bool) celeris.HandlerFunc {
	b.Helper()
	dir := b.TempDir()
	mod := time.Unix(1_700_000_000, 0)
	for name, n := range map[string]int{"app.js": 4096, "app.js.gz": 1024} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, make([]byte, n), 0o600); err != nil {
			b.Fatal(err)
		}
		if err := os.Chtimes(p, mod, mod); err != nil {
			b.Fatal(err)
		}
	}
	return New(Config{Root: dir, MaxAge: time.Hour, Compress: compress})
}

func benchRoot846(b *testing.B, mw celeris.HandlerFunc, hdr ...string) {
	b.Helper()
	noop := func(_ *celeris.Context) error { return nil }
	opts := []celeristest.Option{celeristest.WithHandlers(mw, noop)}
	for i := 0; i+1 < len(hdr); i += 2 {
		opts = append(opts, celeristest.WithHeader(hdr[i], hdr[i+1]))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/app.js", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

// BenchmarkStaticServeFromRoot serves a file of the Root directory.
func BenchmarkStaticServeFromRoot(b *testing.B) { benchRoot846(b, rootBench846(b, false)) }

// BenchmarkStaticServeFromRootNotModified answers an If-None-Match that holds.
func BenchmarkStaticServeFromRootNotModified(b *testing.B) {
	benchRoot846(b, rootBench846(b, false), "if-none-match", `W/"6553f100-1000"`)
}

// BenchmarkStaticServeFromRootCompress serves the .gz variant of a Root file.
func BenchmarkStaticServeFromRootCompress(b *testing.B) {
	benchRoot846(b, rootBench846(b, true), "accept-encoding", "gzip")
}

// BenchmarkStaticServeFromRootRange serves a single range of a Root file.
func BenchmarkStaticServeFromRootRange(b *testing.B) {
	benchRoot846(b, rootBench846(b, false), "range", "bytes=2-9")
}

// BenchmarkStaticFSColdFill is the first request for a file of an fs.FS: it
// reads the file and, since celeris#846, hashes it for the strong ETag. Each
// iteration is a new middleware, so the cache is empty.
func BenchmarkStaticFSColdFill(b *testing.B) {
	for _, size := range []int{1 << 10, 64 << 10, 1 << 20} {
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			mapFS := fstest.MapFS{"app.js": {Data: make([]byte, size), ModTime: time.Unix(1_700_000_000, 0)}}
			noop := func(_ *celeris.Context) error { return nil }
			b.ReportAllocs()
			b.SetBytes(int64(size))
			for b.Loop() {
				mw := New(Config{FS: mapFS})
				ctx, _ := celeristest.NewContext("GET", "/app.js", celeristest.WithHandlers(mw, noop))
				_ = ctx.Next()
				celeristest.ReleaseContext(ctx)
			}
		})
	}
}

// Benchmarks for the paths with Compress and a conditional request (celeris#846
// review): with Compress a request that accepts an encoding tries the .br/.gz
// variants before the 304, because the 304 is about the representation that
// would be sent. These time that cost where no variant exists.

func benchFSConditional846(b *testing.B, mw celeris.HandlerFunc, path string, hdr ...string) {
	b.Helper()
	noop := func(_ *celeris.Context) error { return nil }
	opts := []celeristest.Option{celeristest.WithHandlers(mw, noop)}
	for i := 0; i+1 < len(hdr); i += 2 {
		opts = append(opts, celeristest.WithHeader(hdr[i], hdr[i+1]))
	}
	ctx, _ := celeristest.NewContext("GET", path, opts...) // fills the cache
	_ = ctx.Next()
	celeristest.ReleaseContext(ctx)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", path, opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

func fsBench846() fstest.MapFS {
	mt := time.Unix(1_700_000_000, 0)
	return fstest.MapFS{
		"a.css":    {Data: []byte("body{margin:0}"), ModTime: mt},
		"b.css":    {Data: make([]byte, 4096), ModTime: mt},
		"b.css.gz": {Data: make([]byte, 1024), ModTime: mt},
	}
}

// BenchmarkStaticFSCompressNotModifiedNoVariant is a cache-hit 304 of an fs.FS
// file, Compress on, no variant on disk.
func BenchmarkStaticFSCompressNotModifiedNoVariant(b *testing.B) {
	benchFSConditional846(b, New(Config{FS: fsBench846(), Compress: true}), "/a.css", "accept-encoding", "gzip", "if-none-match", "*")
}

// BenchmarkStaticFSCompressNoVariant is a cache-hit 200, Compress on, no variant.
func BenchmarkStaticFSCompressNoVariant(b *testing.B) {
	benchFSConditional846(b, New(Config{FS: fsBench846(), Compress: true}), "/a.css", "accept-encoding", "gzip")
}

// BenchmarkStaticFSCompressVariant serves an fs.FS variant (read per request).
func BenchmarkStaticFSCompressVariant(b *testing.B) {
	benchFSConditional846(b, New(Config{FS: fsBench846(), Compress: true}), "/b.css", "accept-encoding", "gzip")
}

// BenchmarkStaticDirFSCompressNotModifiedNoVariant is the same 304 over
// os.DirFS, where the two failed variant opens are real system calls.
func BenchmarkStaticDirFSCompressNotModifiedNoVariant(b *testing.B) {
	dir := b.TempDir()
	mt := time.Unix(1_700_000_000, 0)
	p := filepath.Join(dir, "a.css")
	if err := os.WriteFile(p, []byte("body{margin:0}"), 0o600); err != nil {
		b.Fatal(err)
	}
	if err := os.Chtimes(p, mt, mt); err != nil {
		b.Fatal(err)
	}
	benchFSConditional846(b, New(Config{FS: os.DirFS(dir), Compress: true}), "/a.css", "accept-encoding", "gzip", "if-none-match", "*")
}

// BenchmarkStaticRootConditionalMiss is a Root request whose If-None-Match
// does not hold: it is answered with the file, the validators set once.
func BenchmarkStaticRootConditionalMiss(b *testing.B) {
	benchRoot846(b, rootBench846(b, false), "if-none-match", `"nomatch"`)
}
