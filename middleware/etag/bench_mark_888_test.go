package etag

import (
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

// Benchmarks for celeris#888 item 36 (RULE 74 A/B): etag marks a strong tag
// for compress when the request names an encoding.

func benchStrong888(b *testing.B, hdr ...string) { benchStrongActive888(b, false, hdr...) }

func benchStrongActive888(b *testing.B, active bool, hdr ...string) {
	b.Helper()
	body := []byte("hello world, hello world, hello world, hello world")
	handler := func(c *celeris.Context) error { return c.Blob(200, "text/plain", body) }
	front := func(c *celeris.Context) error {
		if active {
			c.Set("celeris.compress.active", true) // compress in front
		}
		return c.Next()
	}
	opts := []celeristest.Option{celeristest.WithHandlers(front, New(Config{Strong: true}), handler)}
	for i := 0; i+1 < len(hdr); i += 2 {
		opts = append(opts, celeristest.WithHeader(hdr[i], hdr[i+1]))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkETagStrong200NoAE888(b *testing.B) { benchStrong888(b) }
func BenchmarkETagStrong200AE888(b *testing.B)   { benchStrong888(b, "accept-encoding", "gzip") }
func BenchmarkETagStrong304NoAE888(b *testing.B) { benchStrong888(b, "if-none-match", "*") }
func BenchmarkETagStrong304AE888(b *testing.B) {
	benchStrong888(b, "accept-encoding", "gzip", "if-none-match", "*")
}

func BenchmarkETagStrong200Active888(b *testing.B) {
	benchStrongActive888(b, true, "accept-encoding", "gzip")
}
func BenchmarkETagStrong304Active888(b *testing.B) {
	benchStrongActive888(b, true, "accept-encoding", "gzip", "if-none-match", "*")
}
