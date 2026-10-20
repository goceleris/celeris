package compress

import (
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

// Benchmarks for celeris#888 items 44, 45 and 40 (RULE 74 A/B): the cost of
// negotiating the encoding for the Accept-Encoding forms clients send. The body
// is under MinLength, so nothing is encoded: what is timed is the negotiation
// and the buffer round trip around it.
func benchAE888(b *testing.B, ae string) {
	b.Helper()
	mw := New(Config{MinLength: 1 << 20})
	h := func(c *celeris.Context) error { return c.Blob(200, "text/plain", []byte("hello")) }
	opts := []celeristest.Option{celeristest.WithHandlers(mw, h), celeristest.WithHeader("accept-encoding", ae)}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkCompressNegotiateBrowser888(b *testing.B) { benchAE888(b, "gzip, deflate, br, zstd") }
func BenchmarkCompressNegotiateGzip888(b *testing.B)    { benchAE888(b, "gzip") }
func BenchmarkCompressNegotiateWeighted888(b *testing.B) {
	benchAE888(b, "br;q=0.9, gzip;q=0.8, *;q=0.1")
}
func BenchmarkCompressNegotiateStarQ0888(b *testing.B)  { benchAE888(b, "gzip, *;q=0") }
func BenchmarkCompressNegotiateRefused888(b *testing.B) { benchAE888(b, "br;q=0, gzip;q=0, zstd;q=0") }
