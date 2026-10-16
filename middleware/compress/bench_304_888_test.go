package compress

import (
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
	"github.com/goceleris/celeris/middleware/etag"
	"github.com/goceleris/celeris/middleware/static"
)

// Benchmarks for celeris#888 item 36 (RULE 74 A/B): compress in front of
// static over an fs.FS and in front of etag, a 304 and a 200, with and without
// an encoding negotiated.

func benchChain888(b *testing.B, hs []celeris.HandlerFunc, path string, hdr ...string) {
	b.Helper()
	opts := []celeristest.Option{celeristest.WithHandlers(hs...)}
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

func staticChain888() []celeris.HandlerFunc {
	fs := fstest.MapFS{"a.txt": {Data: []byte(strings.Repeat("0123456789", 500)), ModTime: time.Unix(1_700_000_000, 0)}}
	return []celeris.HandlerFunc{New(), static.New(static.Config{FS: fs})}
}

func etagChain888() []celeris.HandlerFunc {
	body := []byte(strings.Repeat("0123456789", 500))
	return []celeris.HandlerFunc{New(), etag.New(etag.Config{Strong: true}), func(c *celeris.Context) error {
		return c.Blob(200, "text/plain", body)
	}}
}

func BenchmarkCompressStaticFS200AE888(b *testing.B) {
	benchChain888(b, staticChain888(), "/a.txt", "accept-encoding", "gzip")
}
func BenchmarkCompressStaticFS304AE888(b *testing.B) {
	benchChain888(b, staticChain888(), "/a.txt", "accept-encoding", "gzip", "if-none-match", "*")
}
func BenchmarkCompressStaticFS304NoAE888(b *testing.B) {
	benchChain888(b, staticChain888(), "/a.txt", "if-none-match", "*")
}
func BenchmarkCompressETag200AE888(b *testing.B) {
	benchChain888(b, etagChain888(), "/", "accept-encoding", "gzip")
}
func BenchmarkCompressETag304AE888(b *testing.B) {
	benchChain888(b, etagChain888(), "/", "accept-encoding", "gzip", "if-none-match", "*")
}

// The common fast path of compress: it negotiates an encoding and then does
// not encode, because the body is under MinLength (a JSON error, a small API
// answer). The only new work of celeris#888 item 36 on it is the context key
// compress sets (a map store, and the clear of a non-empty map at release);
// the no-Accept-Encoding row is the same chain without it.
func smallBodyChain888() []celeris.HandlerFunc {
	body := []byte(`{"ok":true,"id":42}`)
	return []celeris.HandlerFunc{New(), func(c *celeris.Context) error {
		return c.Blob(200, "application/json", body)
	}}
}

func BenchmarkCompressSmallBodyAE888(b *testing.B) {
	benchChain888(b, smallBodyChain888(), "/", "accept-encoding", "gzip")
}
func BenchmarkCompressSmallBodyNoAE888(b *testing.B) {
	benchChain888(b, smallBodyChain888(), "/")
}
