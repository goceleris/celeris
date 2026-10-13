package static

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

// Benchmarks for celeris#888 items 33 and 35 (RULE 74 A/B).

// BenchmarkStaticServeFromRootCompressQ0 serves the .gz variant of a Root
// file to a client that refuses br with q=0: the gate parses the q-values.
func BenchmarkStaticServeFromRootCompressQ0(b *testing.B) {
	benchRoot846(b, rootBench846(b, true), "accept-encoding", "br;q=0, gzip")
}

// BenchmarkStaticServeFromRootCompressNoVariant: Compress on, the client
// accepts gzip and br, and the file has no sibling (both gates run, both stats
// fail).
func BenchmarkStaticServeFromRootCompressNoVariant(b *testing.B) {
	dir := b.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.js"), make([]byte, 4096), 0o600); err != nil {
		b.Fatal(err)
	}
	benchRoot846(b, New(Config{Root: dir, Compress: true}), "accept-encoding", "gzip, br")
}

// BenchmarkStaticFSCompressVariantQ0 is BenchmarkStaticFSCompressVariant for
// a client that refuses br with q=0.
func BenchmarkStaticFSCompressVariantQ0(b *testing.B) {
	benchFSConditional846(b, New(Config{FS: fsBench846(), Compress: true}), "/b.css", "accept-encoding", "br;q=0, gzip")
}

// BenchmarkStaticFSCompressNoVariantBrGzip is a cache-hit 200 with no variant
// for a client that accepts both encodings.
func BenchmarkStaticFSCompressNoVariantBrGzip(b *testing.B) {
	benchFSConditional846(b, New(Config{FS: fsBench846(), Compress: true}), "/a.css", "accept-encoding", "gzip, br")
}

// rootDeepBench888 writes <dir>/a/b/c/app.js (4 KiB) and returns the middleware
// serving the directory: the Root 304 check costs one Lstat per path element.
func rootDeepBench888(b *testing.B) celeris.HandlerFunc {
	b.Helper()
	dir := b.TempDir()
	mod := time.Unix(1_700_000_000, 0)
	p := filepath.Join(dir, "a", "b", "c", "app.js")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(p, make([]byte, 4096), 0o600); err != nil {
		b.Fatal(err)
	}
	if err := os.Chtimes(p, mod, mod); err != nil {
		b.Fatal(err)
	}
	return New(Config{Root: dir, MaxAge: time.Hour})
}

func benchPath888(b *testing.B, mw celeris.HandlerFunc, path string, hdr ...string) {
	b.Helper()
	noop := func(_ *celeris.Context) error { return nil }
	opts := []celeristest.Option{celeristest.WithHandlers(mw, noop)}
	for i := 0; i+1 < len(hdr); i += 2 {
		opts = append(opts, celeristest.WithHeader(hdr[i], hdr[i+1]))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", path, opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

func benchRootDeep888(b *testing.B, hdr ...string) {
	b.Helper()
	benchPath888(b, rootDeepBench888(b), "/a/b/c/app.js", hdr...)
}

// BenchmarkStaticServeFromRootNotModifiedDeep answers an If-None-Match that
// holds for a file four elements below the Root.
func BenchmarkStaticServeFromRootNotModifiedDeep(b *testing.B) {
	benchRootDeep888(b, "if-none-match", `W/"6553f100-1000"`)
}

// BenchmarkStaticServeFromRootModifiedSinceDeep: the same with If-Modified-Since.
func BenchmarkStaticServeFromRootModifiedSinceDeep(b *testing.B) {
	benchRootDeep888(b, "if-modified-since", "Wed, 15 Nov 2023 00:00:00 GMT")
}

// BenchmarkStaticServeFromRootConditionalMissDeep: the conditional request does
// not hold, so no containment check is made.
func BenchmarkStaticServeFromRootConditionalMissDeep(b *testing.B) {
	benchRootDeep888(b, "if-none-match", `"nomatch"`)
}
