package static

import (
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

// Benchmarks for celeris#888 item 36 (RULE 74 A/B): static marks the response
// of a GET for a strong-tagged fs.FS file that names an encoding, for
// compress. Each row is a cache-hit request over fsBench846's a.css, with and
// without Accept-Encoding, so the cost of the mark (and of the check that
// decides to make it) shows against the same request that makes none.

func BenchmarkStaticFS200NoAE888(b *testing.B) {
	benchFSConditional846(b, New(Config{FS: fsBench846()}), "/a.css")
}

func BenchmarkStaticFS200AE888(b *testing.B) {
	benchFSConditional846(b, New(Config{FS: fsBench846()}), "/a.css", "accept-encoding", "gzip")
}

func BenchmarkStaticFS304NoAE888(b *testing.B) {
	benchFSConditional846(b, New(Config{FS: fsBench846()}), "/a.css", "if-none-match", "*")
}

func BenchmarkStaticFS304AE888(b *testing.B) {
	benchFSConditional846(b, New(Config{FS: fsBench846()}), "/a.css", "accept-encoding", "gzip", "if-none-match", "*")
}

// BenchmarkStaticFS304RangeAE888: a revalidation that carries a Range, whose
// outcome (a part or the whole) decides whether the 304 is marked.
func BenchmarkStaticFS304RangeAE888(b *testing.B) {
	benchFSConditional846(b, New(Config{FS: fsBench846()}), "/b.css", "accept-encoding", "gzip", "range", "bytes=0-9", "if-none-match", "*")
}

// With compress in front (it has set its key), the response is marked: the
// cost of the mark itself, over the same requests as above.

func benchFSActive888(b *testing.B, hdr ...string) {
	b.Helper()
	mw := New(Config{FS: fsBench846()})
	active := func(c *celeris.Context) error { c.Set("celeris.compress.active", true); return c.Next() }
	noop := func(_ *celeris.Context) error { return nil }
	opts := []celeristest.Option{celeristest.WithHandlers(active, mw, noop)}
	for i := 0; i+1 < len(hdr); i += 2 {
		opts = append(opts, celeristest.WithHeader(hdr[i], hdr[i+1]))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/a.css", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkStaticFS200Active888(b *testing.B) { benchFSActive888(b, "accept-encoding", "gzip") }
func BenchmarkStaticFS304Active888(b *testing.B) {
	benchFSActive888(b, "accept-encoding", "gzip", "if-none-match", "*")
}
