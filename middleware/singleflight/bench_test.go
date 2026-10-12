package singleflight

import (
	"strconv"
	"strings"
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

func BenchmarkSingleflightMiss(b *testing.B) {
	mw := New()
	handler := func(c *celeris.Context) error {
		return c.Blob(200, "text/plain", []byte("hello"))
	}
	opts := []celeristest.Option{celeristest.WithHandlers(mw, handler)}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/bench", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkSingleflightSkip(b *testing.B) {
	mw := New(Config{SkipPaths: []string{"/skip"}})
	handler := func(c *celeris.Context) error {
		return c.Blob(200, "text/plain", []byte("hello"))
	}
	opts := []celeristest.Option{celeristest.WithHandlers(mw, handler)}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/skip", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

// BenchmarkDefaultKeyBrowser is the default key of a browser's conditional
// request: a blank-separated Accept-Encoding and two validators (celeris#888
// items 23 and 24).
func BenchmarkDefaultKeyBrowser(b *testing.B) {
	c, _ := celeristest.NewContext("GET", "/api/items",
		celeristest.WithQuery("page", "2"),
		celeristest.WithHeader("accept-encoding", "gzip, deflate, br, zstd"),
		celeristest.WithHeader("if-none-match", `"v1"`),
		celeristest.WithHeader("if-modified-since", "Mon, 05 Oct 2026 10:00:00 GMT"))
	defer celeristest.ReleaseContext(c)
	b.ReportAllocs()
	for b.Loop() {
		_ = defaultKeyFunc(c)
	}
}

// BenchmarkDefaultKeyPlain is the default key of a request that carries none
// of the headers it holds.
func BenchmarkDefaultKeyPlain(b *testing.B) {
	c, _ := celeristest.NewContext("GET", "/api/items")
	defer celeristest.ReleaseContext(c)
	b.ReportAllocs()
	for b.Loop() {
		_ = defaultKeyFunc(c)
	}
}

// BenchmarkDefaultKeyManyHeaders is the default key of a request that carries
// thirty headers, none of the ones the key holds but a browser's
// Accept-Encoding: every header lookup is a linear scan of them, so this is
// the case where the number of lookups the key makes shows (celeris#888 item
// 23 moved the preconditions from the bypass check into the key).
func BenchmarkDefaultKeyManyHeaders(b *testing.B) {
	opts := []celeristest.Option{celeristest.WithHeader("accept-encoding", "gzip, deflate, br, zstd")}
	for i := range 29 {
		opts = append(opts, celeristest.WithHeader("x-hdr-"+strconv.Itoa(i), "value"))
	}
	c, _ := celeristest.NewContext("GET", "/api/items", opts...)
	defer celeristest.ReleaseContext(c)
	b.ReportAllocs()
	for b.Loop() {
		_ = defaultKeyFunc(c)
	}
}

// BenchmarkSingleflightMissManyHeaders is the whole middleware (the key, the
// Range lookup of the bypass check, the call) for a request with thirty
// headers, the end-to-end counterpart of BenchmarkDefaultKeyManyHeaders: the
// default key makes seven header lookups where main's made three plus five in
// the bypass check, so only this number says what the move cost.
func BenchmarkSingleflightMissManyHeaders(b *testing.B) {
	opts := []celeristest.Option{celeristest.WithHeader("accept-encoding", "gzip, deflate, br, zstd")}
	for i := range 29 {
		opts = append(opts, celeristest.WithHeader("x-hdr-"+strconv.Itoa(i), "value"))
	}
	opts = append(opts, celeristest.WithHandlers(New(), func(c *celeris.Context) error {
		return c.Blob(200, "text/plain", []byte("hello"))
	}))
	b.ReportAllocs()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/api/items", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

// BenchmarkDefaultKeyLongAcceptEncoding is the default key of a request whose
// Accept-Encoding is 64 KiB of "a,a,a,..." (the most a native engine accepts
// in a head, and the dearest value of that size for the negotiator's parser):
// the key must cost about the copy of the value, as main's did, not the
// parse.
func BenchmarkDefaultKeyLongAcceptEncoding(b *testing.B) {
	c, _ := celeristest.NewContext("GET", "/api/items", celeristest.WithHeader("accept-encoding", strings.Repeat("a,", 32<<10)))
	defer celeristest.ReleaseContext(c)
	b.ReportAllocs()
	for b.Loop() {
		_ = defaultKeyFunc(c)
	}
}
