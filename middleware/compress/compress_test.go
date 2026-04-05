package compress

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

// testBody returns a compressible body larger than the default MinLength.
func testBody() []byte {
	return []byte(strings.Repeat("The quick brown fox jumps over the lazy dog. ", 20))
}

func jsonHandler(body []byte) celeris.HandlerFunc {
	return func(c *celeris.Context) error {
		return c.Blob(200, "application/json", body)
	}
}

func decompressGzip(t *testing.T, data []byte) []byte {
	t.Helper()
	r, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer func() { _ = r.Close() }()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("gzip read: %v", err)
	}
	return out
}

func decompressBrotli(t *testing.T, data []byte) []byte {
	t.Helper()
	r := brotli.NewReader(bytes.NewReader(data))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("brotli read: %v", err)
	}
	return out
}

func decompressZstd(t *testing.T, data []byte) []byte {
	t.Helper()
	dec, err := zstd.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("zstd.NewReader: %v", err)
	}
	defer dec.Close()
	out, err := io.ReadAll(dec)
	if err != nil {
		t.Fatalf("zstd read: %v", err)
	}
	return out
}

func decompressDeflate(t *testing.T, data []byte) []byte {
	t.Helper()
	r := flate.NewReader(bytes.NewReader(data))
	defer func() { _ = r.Close() }()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("deflate read: %v", err)
	}
	return out
}

func hasVary(rec *celeristest.ResponseRecorder) bool {
	for _, h := range rec.Headers {
		if h[0] == "vary" && h[1] == "Accept-Encoding" {
			return true
		}
	}
	return false
}

func TestGzipCompression(t *testing.T) {
	body := testBody()
	mw := New(Config{Encodings: []string{"gzip"}})
	handler := jsonHandler(body)

	ctx, rec := celeristest.NewContext("GET", "/data",
		celeristest.WithHeader("accept-encoding", "gzip"),
		celeristest.WithHandlers(mw, handler),
	)
	defer celeristest.ReleaseContext(ctx)

	if err := ctx.Next(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if rec.Header("content-encoding") != "gzip" {
		t.Fatalf("expected content-encoding gzip, got %q", rec.Header("content-encoding"))
	}
	decompressed := decompressGzip(t, rec.Body)
	if !bytes.Equal(decompressed, body) {
		t.Fatalf("decompressed body mismatch")
	}
}

func TestBrotliCompression(t *testing.T) {
	body := testBody()
	mw := New(Config{Encodings: []string{"br"}})
	handler := jsonHandler(body)

	ctx, rec := celeristest.NewContext("GET", "/data",
		celeristest.WithHeader("accept-encoding", "br"),
		celeristest.WithHandlers(mw, handler),
	)
	defer celeristest.ReleaseContext(ctx)

	if err := ctx.Next(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if rec.Header("content-encoding") != "br" {
		t.Fatalf("expected content-encoding br, got %q", rec.Header("content-encoding"))
	}
	decompressed := decompressBrotli(t, rec.Body)
	if !bytes.Equal(decompressed, body) {
		t.Fatalf("decompressed body mismatch")
	}
}

func TestZstdCompression(t *testing.T) {
	body := testBody()
	mw := New(Config{Encodings: []string{"zstd"}})
	handler := jsonHandler(body)

	ctx, rec := celeristest.NewContext("GET", "/data",
		celeristest.WithHeader("accept-encoding", "zstd"),
		celeristest.WithHandlers(mw, handler),
	)
	defer celeristest.ReleaseContext(ctx)

	if err := ctx.Next(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if rec.Header("content-encoding") != "zstd" {
		t.Fatalf("expected content-encoding zstd, got %q", rec.Header("content-encoding"))
	}
	decompressed := decompressZstd(t, rec.Body)
	if !bytes.Equal(decompressed, body) {
		t.Fatalf("decompressed body mismatch")
	}
}

func TestNegotiationGzip(t *testing.T) {
	body := testBody()
	mw := New()
	handler := jsonHandler(body)

	ctx, rec := celeristest.NewContext("GET", "/data",
		celeristest.WithHeader("accept-encoding", "gzip"),
		celeristest.WithHandlers(mw, handler),
	)
	defer celeristest.ReleaseContext(ctx)

	if err := ctx.Next(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Header("content-encoding") != "gzip" {
		t.Fatalf("expected gzip, got %q", rec.Header("content-encoding"))
	}
}

func TestNegotiationBrotli(t *testing.T) {
	body := testBody()
	mw := New()
	handler := jsonHandler(body)

	ctx, rec := celeristest.NewContext("GET", "/data",
		celeristest.WithHeader("accept-encoding", "br"),
		celeristest.WithHandlers(mw, handler),
	)
	defer celeristest.ReleaseContext(ctx)

	if err := ctx.Next(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Header("content-encoding") != "br" {
		t.Fatalf("expected br, got %q", rec.Header("content-encoding"))
	}
}

func TestNegotiationZstd(t *testing.T) {
	body := testBody()
	mw := New()
	handler := jsonHandler(body)

	ctx, rec := celeristest.NewContext("GET", "/data",
		celeristest.WithHeader("accept-encoding", "zstd"),
		celeristest.WithHandlers(mw, handler),
	)
	defer celeristest.ReleaseContext(ctx)

	if err := ctx.Next(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Header("content-encoding") != "zstd" {
		t.Fatalf("expected zstd, got %q", rec.Header("content-encoding"))
	}
}

func TestNoAcceptEncoding(t *testing.T) {
	body := testBody()
	mw := New()
	handler := jsonHandler(body)

	ctx, rec := celeristest.NewContext("GET", "/data",
		celeristest.WithHandlers(mw, handler),
	)
	defer celeristest.ReleaseContext(ctx)

	if err := ctx.Next(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Header("content-encoding") != "" {
		t.Fatalf("expected no content-encoding, got %q", rec.Header("content-encoding"))
	}
	if !bytes.Equal(rec.Body, body) {
		t.Fatalf("body should be uncompressed")
	}
}

func TestAcceptEncodingIdentity(t *testing.T) {
	body := testBody()
	mw := New()
	handler := jsonHandler(body)

	ctx, rec := celeristest.NewContext("GET", "/data",
		celeristest.WithHeader("accept-encoding", "identity"),
		celeristest.WithHandlers(mw, handler),
	)
	defer celeristest.ReleaseContext(ctx)

	if err := ctx.Next(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Header("content-encoding") != "" {
		t.Fatalf("expected no content-encoding, got %q", rec.Header("content-encoding"))
	}
	if !bytes.Equal(rec.Body, body) {
		t.Fatalf("body should be uncompressed")
	}
}

func TestBelowMinLength(t *testing.T) {
	body := []byte("short")
	mw := New()
	handler := jsonHandler(body)

	ctx, rec := celeristest.NewContext("GET", "/data",
		celeristest.WithHeader("accept-encoding", "gzip"),
		celeristest.WithHandlers(mw, handler),
	)
	defer celeristest.ReleaseContext(ctx)

	if err := ctx.Next(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Header("content-encoding") != "" {
		t.Fatalf("expected no compression for short body")
	}
	if !bytes.Equal(rec.Body, body) {
		t.Fatalf("body should be uncompressed")
	}
}

func TestEmptyBody(t *testing.T) {
	mw := New()
	handler := func(c *celeris.Context) error {
		return c.NoContent(204)
	}

	ctx, rec := celeristest.NewContext("GET", "/data",
		celeristest.WithHeader("accept-encoding", "gzip"),
		celeristest.WithHandlers(mw, handler),
	)
	defer celeristest.ReleaseContext(ctx)

	if err := ctx.Next(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Header("content-encoding") != "" {
		t.Fatalf("expected no compression for empty body")
	}
}

func TestNon2xxResponse(t *testing.T) {
	body := testBody()
	mw := New()
	handler := func(c *celeris.Context) error {
		return c.Blob(404, "text/plain", body)
	}

	ctx, rec := celeristest.NewContext("GET", "/data",
		celeristest.WithHeader("accept-encoding", "gzip"),
		celeristest.WithHandlers(mw, handler),
	)
	defer celeristest.ReleaseContext(ctx)

	if err := ctx.Next(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Header("content-encoding") != "" {
		t.Fatalf("expected no compression for 404 response")
	}
	if !bytes.Equal(rec.Body, body) {
		t.Fatalf("body should be uncompressed")
	}
}

func TestHEADRequestSkip(t *testing.T) {
	body := testBody()
	mw := New()
	handler := jsonHandler(body)

	ctx, rec := celeristest.NewContext("HEAD", "/data",
		celeristest.WithHeader("accept-encoding", "gzip"),
		celeristest.WithHandlers(mw, handler),
	)
	defer celeristest.ReleaseContext(ctx)

	if err := ctx.Next(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Header("content-encoding") != "" {
		t.Fatalf("expected no compression for HEAD")
	}
	if !hasVary(rec) {
		t.Fatalf("expected Vary: Accept-Encoding on HEAD response")
	}
}

func TestOPTIONSRequestSkip(t *testing.T) {
	body := testBody()
	mw := New()
	handler := jsonHandler(body)

	ctx, rec := celeristest.NewContext("OPTIONS", "/data",
		celeristest.WithHeader("accept-encoding", "gzip"),
		celeristest.WithHandlers(mw, handler),
	)
	defer celeristest.ReleaseContext(ctx)

	if err := ctx.Next(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Header("content-encoding") != "" {
		t.Fatalf("expected no compression for OPTIONS")
	}
}

func TestAlreadyHasContentEncoding(t *testing.T) {
	body := testBody()
	mw := New()
	handler := func(c *celeris.Context) error {
		c.SetHeader("content-encoding", "br")
		return c.Blob(200, "application/json", body)
	}

	ctx, rec := celeristest.NewContext("GET", "/data",
		celeristest.WithHeader("accept-encoding", "gzip"),
		celeristest.WithHandlers(mw, handler),
	)
	defer celeristest.ReleaseContext(ctx)

	if err := ctx.Next(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Header("content-encoding") != "br" {
		t.Fatalf("expected existing content-encoding br, got %q", rec.Header("content-encoding"))
	}
	if !bytes.Equal(rec.Body, body) {
		t.Fatalf("body should not be re-compressed")
	}
}

func TestExcludedContentTypeImage(t *testing.T) {
	body := testBody()
	mw := New()
	handler := func(c *celeris.Context) error {
		return c.Blob(200, "image/png", body)
	}

	ctx, rec := celeristest.NewContext("GET", "/img",
		celeristest.WithHeader("accept-encoding", "gzip"),
		celeristest.WithHandlers(mw, handler),
	)
	defer celeristest.ReleaseContext(ctx)

	if err := ctx.Next(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Header("content-encoding") != "" {
		t.Fatalf("expected no compression for image/png")
	}
}

func TestExcludedContentTypeVideo(t *testing.T) {
	body := testBody()
	mw := New()
	handler := func(c *celeris.Context) error {
		return c.Blob(200, "video/mp4", body)
	}

	ctx, rec := celeristest.NewContext("GET", "/vid",
		celeristest.WithHeader("accept-encoding", "gzip"),
		celeristest.WithHandlers(mw, handler),
	)
	defer celeristest.ReleaseContext(ctx)

	if err := ctx.Next(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Header("content-encoding") != "" {
		t.Fatalf("expected no compression for video/mp4")
	}
}

func TestNoExcludedContentTypes(t *testing.T) {
	body := testBody()
	mw := New(Config{
		Encodings:            []string{"gzip"},
		ExcludedContentTypes: []string{}, // explicitly empty: no exclusions
	})
	handler := func(c *celeris.Context) error {
		return c.Blob(200, "image/png", body)
	}

	ctx, rec := celeristest.NewContext("GET", "/img",
		celeristest.WithHeader("accept-encoding", "gzip"),
		celeristest.WithHandlers(mw, handler),
	)
	defer celeristest.ReleaseContext(ctx)

	if err := ctx.Next(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Header("content-encoding") != "gzip" {
		t.Fatalf("expected image/png to be compressed when ExcludedContentTypes is empty, got %q", rec.Header("content-encoding"))
	}
	decompressed := decompressGzip(t, rec.Body)
	if !bytes.Equal(decompressed, body) {
		t.Fatalf("decompressed body mismatch")
	}
}

func TestVaryHeaderAdded(t *testing.T) {
	body := testBody()
	mw := New(Config{Encodings: []string{"gzip"}})
	handler := jsonHandler(body)

	ctx, rec := celeristest.NewContext("GET", "/data",
		celeristest.WithHeader("accept-encoding", "gzip"),
		celeristest.WithHandlers(mw, handler),
	)
	defer celeristest.ReleaseContext(ctx)

	if err := ctx.Next(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !hasVary(rec) {
		t.Fatalf("expected Vary: Accept-Encoding header")
	}
}

func TestVaryHeaderNotClobbered(t *testing.T) {
	body := testBody()
	mw := New(Config{Encodings: []string{"gzip"}})
	handler := func(c *celeris.Context) error {
		c.AddHeader("vary", "Origin")
		return c.Blob(200, "application/json", body)
	}

	ctx, rec := celeristest.NewContext("GET", "/data",
		celeristest.WithHeader("accept-encoding", "gzip"),
		celeristest.WithHandlers(mw, handler),
	)
	defer celeristest.ReleaseContext(ctx)

	if err := ctx.Next(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	hasOrigin := false
	hasAcceptEncoding := false
	for _, h := range rec.Headers {
		if h[0] == "vary" {
			if h[1] == "Origin" {
				hasOrigin = true
			}
			if h[1] == "Accept-Encoding" {
				hasAcceptEncoding = true
			}
		}
	}
	if !hasOrigin {
		t.Fatalf("expected Vary: Origin to be preserved")
	}
	if !hasAcceptEncoding {
		t.Fatalf("expected Vary: Accept-Encoding to be added")
	}
}

func TestVaryHeaderOnUncompressedBelowMinLength(t *testing.T) {
	body := []byte("short")
	mw := New(Config{Encodings: []string{"gzip"}})
	handler := jsonHandler(body)

	ctx, rec := celeristest.NewContext("GET", "/data",
		celeristest.WithHeader("accept-encoding", "gzip"),
		celeristest.WithHandlers(mw, handler),
	)
	defer celeristest.ReleaseContext(ctx)

	if err := ctx.Next(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Header("content-encoding") != "" {
		t.Fatalf("expected no compression for short body")
	}
	if !hasVary(rec) {
		t.Fatalf("expected Vary: Accept-Encoding even on uncompressed response")
	}
}

func TestVaryHeaderOnExcludedContentType(t *testing.T) {
	body := testBody()
	mw := New(Config{Encodings: []string{"gzip"}})
	handler := func(c *celeris.Context) error {
		return c.Blob(200, "image/png", body)
	}

	ctx, rec := celeristest.NewContext("GET", "/data",
		celeristest.WithHeader("accept-encoding", "gzip"),
		celeristest.WithHandlers(mw, handler),
	)
	defer celeristest.ReleaseContext(ctx)

	if err := ctx.Next(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Header("content-encoding") != "" {
		t.Fatalf("expected no compression for image/png")
	}
	if !hasVary(rec) {
		t.Fatalf("expected Vary: Accept-Encoding even on excluded content type")
	}
}

func TestVaryHeaderOnNoMatchingEncoding(t *testing.T) {
	body := testBody()
	mw := New(Config{Encodings: []string{"zstd"}})
	handler := jsonHandler(body)

	ctx, rec := celeristest.NewContext("GET", "/data",
		celeristest.WithHeader("accept-encoding", "gzip"),
		celeristest.WithHandlers(mw, handler),
	)
	defer celeristest.ReleaseContext(ctx)

	if err := ctx.Next(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Header("content-encoding") != "" {
		t.Fatalf("expected no compression when encoding not supported")
	}
	if !hasVary(rec) {
		t.Fatalf("expected Vary: Accept-Encoding even when encoding does not match")
	}
}

func TestCompressionExpansion(t *testing.T) {
	// Random-looking bytes that are incompressible. Use enough to exceed
	// MinLength but small enough that compression expands.
	body := make([]byte, 300)
	for i := range body {
		body[i] = byte(i*37 + i*i)
	}
	mw := New(Config{Encodings: []string{"gzip"}})
	handler := func(c *celeris.Context) error {
		return c.Blob(200, "application/octet-stream", body)
	}

	ctx, rec := celeristest.NewContext("GET", "/data",
		celeristest.WithHeader("accept-encoding", "gzip"),
		celeristest.WithHandlers(mw, handler),
	)
	defer celeristest.ReleaseContext(ctx)

	if err := ctx.Next(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// If compressed >= original, original should be flushed.
	if rec.Header("content-encoding") != "" {
		t.Fatalf("expected no compression for incompressible body")
	}
	if !bytes.Equal(rec.Body, body) {
		t.Fatalf("expected original body")
	}
}

func TestSkipPaths(t *testing.T) {
	body := testBody()
	mw := New(Config{SkipPaths: []string{"/health"}})
	handler := jsonHandler(body)

	ctx, rec := celeristest.NewContext("GET", "/health",
		celeristest.WithHeader("accept-encoding", "gzip"),
		celeristest.WithHandlers(mw, handler),
	)
	defer celeristest.ReleaseContext(ctx)

	if err := ctx.Next(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Header("content-encoding") != "" {
		t.Fatalf("expected no compression for skip path")
	}
}

func TestSkipFunction(t *testing.T) {
	body := testBody()
	mw := New(Config{
		Skip: func(c *celeris.Context) bool {
			return c.Path() == "/skip"
		},
	})
	handler := jsonHandler(body)

	ctx, rec := celeristest.NewContext("GET", "/skip",
		celeristest.WithHeader("accept-encoding", "gzip"),
		celeristest.WithHandlers(mw, handler),
	)
	defer celeristest.ReleaseContext(ctx)

	if err := ctx.Next(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Header("content-encoding") != "" {
		t.Fatalf("expected no compression for skip function")
	}
}

func TestCustomMinLength(t *testing.T) {
	body := []byte(strings.Repeat("x", 100))
	mw := New(Config{MinLength: 50, Encodings: []string{"gzip"}})
	handler := func(c *celeris.Context) error {
		return c.Blob(200, "text/plain", body)
	}

	ctx, rec := celeristest.NewContext("GET", "/data",
		celeristest.WithHeader("accept-encoding", "gzip"),
		celeristest.WithHandlers(mw, handler),
	)
	defer celeristest.ReleaseContext(ctx)

	if err := ctx.Next(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Header("content-encoding") != "gzip" {
		t.Fatalf("expected gzip with custom MinLength 50")
	}
}

func TestCustomEncodingsOrder(t *testing.T) {
	body := testBody()
	// Server prefers gzip first.
	mw := New(Config{Encodings: []string{"gzip", "br", "zstd"}})
	handler := jsonHandler(body)

	// Client accepts all three.
	ctx, rec := celeristest.NewContext("GET", "/data",
		celeristest.WithHeader("accept-encoding", "gzip, br, zstd"),
		celeristest.WithHandlers(mw, handler),
	)
	defer celeristest.ReleaseContext(ctx)

	if err := ctx.Next(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Server preference should win when client accepts all.
	if rec.Header("content-encoding") != "gzip" {
		t.Fatalf("expected gzip (server priority), got %q", rec.Header("content-encoding"))
	}
}

func TestMultipleAcceptEncodingPreferences(t *testing.T) {
	body := testBody()
	mw := New()
	handler := jsonHandler(body)

	// Client explicitly prefers br over gzip.
	ctx, rec := celeristest.NewContext("GET", "/data",
		celeristest.WithHeader("accept-encoding", "gzip;q=0.5, br;q=1.0"),
		celeristest.WithHandlers(mw, handler),
	)
	defer celeristest.ReleaseContext(ctx)

	if err := ctx.Next(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Header("content-encoding") != "br" {
		t.Fatalf("expected br (client prefers q=1.0), got %q", rec.Header("content-encoding"))
	}
}

func TestHandlerErrorPropagated(t *testing.T) {
	errExpected := errors.New("handler failed")
	mw := New()
	handler := func(c *celeris.Context) error {
		return errExpected
	}

	ctx, _ := celeristest.NewContext("GET", "/data",
		celeristest.WithHeader("accept-encoding", "gzip"),
		celeristest.WithHandlers(mw, handler),
	)
	defer celeristest.ReleaseContext(ctx)

	err := ctx.Next()
	if !errors.Is(err, errExpected) {
		t.Fatalf("expected handler error to propagate, got %v", err)
	}
}

func TestLevelDefaultZeroValue(t *testing.T) {
	// Verify that a zero-value Config (all levels = 0 = LevelDefault)
	// compresses correctly using library defaults.
	body := testBody()
	mw := New(Config{Encodings: []string{"gzip"}})
	handler := jsonHandler(body)

	ctx, rec := celeristest.NewContext("GET", "/data",
		celeristest.WithHeader("accept-encoding", "gzip"),
		celeristest.WithHandlers(mw, handler),
	)
	defer celeristest.ReleaseContext(ctx)

	if err := ctx.Next(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Header("content-encoding") != "gzip" {
		t.Fatalf("expected gzip with LevelDefault, got %q", rec.Header("content-encoding"))
	}
	decompressed := decompressGzip(t, rec.Body)
	if !bytes.Equal(decompressed, body) {
		t.Fatalf("decompressed body mismatch with LevelDefault")
	}
}

func TestLevelNone(t *testing.T) {
	// LevelNone (-1) should produce valid gzip output (stored, no compression).
	body := testBody()
	mw := New(Config{
		Encodings: []string{"gzip"},
		GzipLevel: LevelNone,
	})
	handler := jsonHandler(body)

	ctx, rec := celeristest.NewContext("GET", "/data",
		celeristest.WithHeader("accept-encoding", "gzip"),
		celeristest.WithHandlers(mw, handler),
	)
	defer celeristest.ReleaseContext(ctx)

	if err := ctx.Next(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// LevelNone (gzip level 0 = NoCompression) produces valid but large output.
	// It may trigger the expansion guard for small bodies, so we just verify
	// that the output is decodable if compression was applied.
	if rec.Header("content-encoding") == "gzip" {
		decompressed := decompressGzip(t, rec.Body)
		if !bytes.Equal(decompressed, body) {
			t.Fatalf("decompressed body mismatch with LevelNone")
		}
	}
}

func TestLevelConstants(t *testing.T) {
	if LevelDefault != 0 {
		t.Fatalf("LevelDefault should be 0, got %d", LevelDefault)
	}
	if LevelNone != -1 {
		t.Fatalf("LevelNone should be -1, got %d", LevelNone)
	}
	if LevelBest != -2 {
		t.Fatalf("LevelBest should be -2 (sentinel), got %d", LevelBest)
	}
	if LevelFastest != 1 {
		t.Fatalf("LevelFastest should be 1, got %d", LevelFastest)
	}
}

// --- Fix 3: validate() tests ---

func TestValidatePanics(t *testing.T) {
	tests := []struct {
		name        string
		config      Config
		skipDefault bool // call validate() directly without applyDefaults
	}{
		{
			name:        "empty encodings",
			config:      Config{Encodings: []string{}},
			skipDefault: true, // applyDefaults would fill in defaults
		},
		{
			name:   "unsupported encoding",
			config: Config{Encodings: []string{"lz4"}},
		},
		{
			name:   "gzip level too low",
			config: Config{Encodings: []string{"gzip"}, GzipLevel: -3},
		},
		{
			name:   "gzip level too high",
			config: Config{Encodings: []string{"gzip"}, GzipLevel: 10},
		},
		{
			name:   "brotli level too low",
			config: Config{Encodings: []string{"br"}, BrotliLevel: -3},
		},
		{
			name:   "brotli level too high",
			config: Config{Encodings: []string{"br"}, BrotliLevel: 12},
		},
		{
			name:   "zstd level too low",
			config: Config{Encodings: []string{"zstd"}, ZstdLevel: -3},
		},
		{
			name:   "zstd level too high",
			config: Config{Encodings: []string{"zstd"}, ZstdLevel: 5},
		},
		{
			name:   "negative min length",
			config: Config{Encodings: []string{"gzip"}, MinLength: -1},
		},
		{
			name:   "empty string in excluded content types",
			config: Config{Encodings: []string{"gzip"}, ExcludedContentTypes: []string{"image/", ""}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Fatal("expected panic")
				}
			}()
			cfg := tt.config
			if !tt.skipDefault {
				cfg = applyDefaults(cfg)
			}
			cfg.validate()
		})
	}
}

func TestValidateAcceptsValidConfigs(t *testing.T) {
	tests := []struct {
		name   string
		config Config
	}{
		{
			"default config",
			defaultConfig,
		},
		{
			"gzip with LevelNone",
			Config{Encodings: []string{"gzip"}, GzipLevel: LevelNone, ExcludedContentTypes: []string{"image/"}},
		},
		{
			"brotli with LevelNone",
			Config{Encodings: []string{"br"}, BrotliLevel: LevelNone, ExcludedContentTypes: []string{"image/"}},
		},
		{
			"zstd with LevelNone",
			Config{Encodings: []string{"zstd"}, ZstdLevel: LevelNone, ExcludedContentTypes: []string{"image/"}},
		},
		{
			"gzip with LevelDefault (zero value)",
			Config{Encodings: []string{"gzip"}, GzipLevel: LevelDefault, ExcludedContentTypes: []string{"image/"}},
		},
		{
			"all encodings with LevelBest",
			Config{
				Encodings:            []string{"gzip", "br", "zstd"},
				GzipLevel:            LevelBest,
				BrotliLevel:          LevelBest,
				ZstdLevel:            LevelBest,
				ExcludedContentTypes: []string{"image/"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("unexpected panic: %v", r)
				}
			}()
			cfg := applyDefaults(tt.config)
			cfg.validate()
		})
	}
}

// --- Fix 7: concurrent request test ---

func TestConcurrentRequests(t *testing.T) {
	mw := New()
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			handler := func(c *celeris.Context) error {
				return c.Blob(200, "text/plain", []byte(strings.Repeat("x", 1000)))
			}
			ctx, rec := celeristest.NewContext("GET", "/",
				celeristest.WithHeader("accept-encoding", "gzip"),
				celeristest.WithHandlers(mw, handler),
			)
			err := ctx.Next()
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if rec.StatusCode != 200 {
				t.Errorf("want 200, got %d", rec.StatusCode)
			}
			celeristest.ReleaseContext(ctx)
		}()
	}
	wg.Wait()
}

// --- Zstd level mapping tests ---

func TestZstdLevelDefault(t *testing.T) {
	body := testBody()
	mw := New(Config{
		Encodings: []string{"zstd"},
		ZstdLevel: LevelDefault,
	})
	handler := jsonHandler(body)

	ctx, rec := celeristest.NewContext("GET", "/data",
		celeristest.WithHeader("accept-encoding", "zstd"),
		celeristest.WithHandlers(mw, handler),
	)
	defer celeristest.ReleaseContext(ctx)

	if err := ctx.Next(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Header("content-encoding") != "zstd" {
		t.Fatalf("expected zstd, got %q", rec.Header("content-encoding"))
	}
	decompressed := decompressZstd(t, rec.Body)
	if !bytes.Equal(decompressed, body) {
		t.Fatalf("decompressed body mismatch with LevelDefault")
	}
}

func TestZstdLevelBest(t *testing.T) {
	body := testBody()
	mw := New(Config{
		Encodings: []string{"zstd"},
		ZstdLevel: LevelBest,
	})
	handler := jsonHandler(body)

	ctx, rec := celeristest.NewContext("GET", "/data",
		celeristest.WithHeader("accept-encoding", "zstd"),
		celeristest.WithHandlers(mw, handler),
	)
	defer celeristest.ReleaseContext(ctx)

	if err := ctx.Next(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Header("content-encoding") != "zstd" {
		t.Fatalf("expected zstd, got %q", rec.Header("content-encoding"))
	}
	decompressed := decompressZstd(t, rec.Body)
	if !bytes.Equal(decompressed, body) {
		t.Fatalf("decompressed body mismatch with LevelBest")
	}
}

func TestZstdLevelNone(t *testing.T) {
	body := testBody()
	mw := New(Config{
		Encodings: []string{"zstd"},
		ZstdLevel: LevelNone,
	})
	handler := jsonHandler(body)

	ctx, rec := celeristest.NewContext("GET", "/data",
		celeristest.WithHeader("accept-encoding", "zstd"),
		celeristest.WithHandlers(mw, handler),
	)
	defer celeristest.ReleaseContext(ctx)

	if err := ctx.Next(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// LevelNone maps to SpeedFastest for zstd. Output should be valid.
	if rec.Header("content-encoding") == "zstd" {
		decompressed := decompressZstd(t, rec.Body)
		if !bytes.Equal(decompressed, body) {
			t.Fatalf("decompressed body mismatch with LevelNone")
		}
	}
}

func TestGzipLevelBest(t *testing.T) {
	body := testBody()
	mw := New(Config{
		Encodings: []string{"gzip"},
		GzipLevel: LevelBest,
	})
	handler := jsonHandler(body)

	ctx, rec := celeristest.NewContext("GET", "/data",
		celeristest.WithHeader("accept-encoding", "gzip"),
		celeristest.WithHandlers(mw, handler),
	)
	defer celeristest.ReleaseContext(ctx)

	if err := ctx.Next(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Header("content-encoding") != "gzip" {
		t.Fatalf("expected gzip, got %q", rec.Header("content-encoding"))
	}
	decompressed := decompressGzip(t, rec.Body)
	if !bytes.Equal(decompressed, body) {
		t.Fatalf("decompressed body mismatch with LevelBest")
	}
}

func TestBrotliLevelBest(t *testing.T) {
	body := testBody()
	mw := New(Config{
		Encodings:   []string{"br"},
		BrotliLevel: LevelBest,
	})
	handler := jsonHandler(body)

	ctx, rec := celeristest.NewContext("GET", "/data",
		celeristest.WithHeader("accept-encoding", "br"),
		celeristest.WithHandlers(mw, handler),
	)
	defer celeristest.ReleaseContext(ctx)

	if err := ctx.Next(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Header("content-encoding") != "br" {
		t.Fatalf("expected br, got %q", rec.Header("content-encoding"))
	}
	decompressed := decompressBrotli(t, rec.Body)
	if !bytes.Equal(decompressed, body) {
		t.Fatalf("decompressed body mismatch with LevelBest")
	}
}

func TestVaryOnHEAD(t *testing.T) {
	body := testBody()
	mw := New(Config{Encodings: []string{"gzip"}})
	handler := jsonHandler(body)

	ctx, rec := celeristest.NewContext("HEAD", "/data",
		celeristest.WithHeader("accept-encoding", "gzip"),
		celeristest.WithHandlers(mw, handler),
	)
	defer celeristest.ReleaseContext(ctx)

	if err := ctx.Next(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Header("content-encoding") != "" {
		t.Fatalf("expected no compression for HEAD")
	}
	if !hasVary(rec) {
		t.Fatalf("expected Vary: Accept-Encoding on HEAD response")
	}
}

func TestVaryOnOPTIONS(t *testing.T) {
	body := testBody()
	mw := New(Config{Encodings: []string{"gzip"}})
	handler := jsonHandler(body)

	ctx, rec := celeristest.NewContext("OPTIONS", "/data",
		celeristest.WithHeader("accept-encoding", "gzip"),
		celeristest.WithHandlers(mw, handler),
	)
	defer celeristest.ReleaseContext(ctx)

	if err := ctx.Next(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Header("content-encoding") != "" {
		t.Fatalf("expected no compression for OPTIONS")
	}
	if !hasVary(rec) {
		t.Fatalf("expected Vary: Accept-Encoding on OPTIONS response")
	}
}

func TestAllEncodingsLevelBest(t *testing.T) {
	// Verify that LevelBest does not panic for any encoding.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("unexpected panic with LevelBest: %v", r)
		}
	}()
	_ = New(Config{
		Encodings:   []string{"gzip", "br", "zstd"},
		GzipLevel:   LevelBest,
		BrotliLevel: LevelBest,
		ZstdLevel:   LevelBest,
	})
}

// --- Resolve-level raw integer tests ---

func TestResolveGzipLevelRawInt(t *testing.T) {
	body := testBody()
	mw := New(Config{Encodings: []string{"gzip"}, GzipLevel: 4})
	handler := jsonHandler(body)

	ctx, rec := celeristest.NewContext("GET", "/data",
		celeristest.WithHeader("accept-encoding", "gzip"),
		celeristest.WithHandlers(mw, handler),
	)
	defer celeristest.ReleaseContext(ctx)

	if err := ctx.Next(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Header("content-encoding") != "gzip" {
		t.Fatalf("expected gzip with raw level 4, got %q", rec.Header("content-encoding"))
	}
	decompressed := decompressGzip(t, rec.Body)
	if !bytes.Equal(decompressed, body) {
		t.Fatalf("decompressed body mismatch with gzip level 4")
	}
}

func TestResolveBrotliLevelRawInt(t *testing.T) {
	body := testBody()
	mw := New(Config{Encodings: []string{"br"}, BrotliLevel: 8})
	handler := jsonHandler(body)

	ctx, rec := celeristest.NewContext("GET", "/data",
		celeristest.WithHeader("accept-encoding", "br"),
		celeristest.WithHandlers(mw, handler),
	)
	defer celeristest.ReleaseContext(ctx)

	if err := ctx.Next(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Header("content-encoding") != "br" {
		t.Fatalf("expected br with raw level 8, got %q", rec.Header("content-encoding"))
	}
	decompressed := decompressBrotli(t, rec.Body)
	if !bytes.Equal(decompressed, body) {
		t.Fatalf("decompressed body mismatch with brotli level 8")
	}
}

func TestResolveZstdLevelRawInt(t *testing.T) {
	body := testBody()
	mw := New(Config{Encodings: []string{"zstd"}, ZstdLevel: 2})
	handler := jsonHandler(body)

	ctx, rec := celeristest.NewContext("GET", "/data",
		celeristest.WithHeader("accept-encoding", "zstd"),
		celeristest.WithHandlers(mw, handler),
	)
	defer celeristest.ReleaseContext(ctx)

	if err := ctx.Next(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Header("content-encoding") != "zstd" {
		t.Fatalf("expected zstd with raw level 2, got %q", rec.Header("content-encoding"))
	}
	decompressed := decompressZstd(t, rec.Body)
	if !bytes.Equal(decompressed, body) {
		t.Fatalf("decompressed body mismatch with zstd level 2")
	}
}

func TestResolveZstdLevelMax(t *testing.T) {
	// Level 4 is SpeedBestCompression, the maximum valid zstd level.
	body := testBody()
	mw := New(Config{Encodings: []string{"zstd"}, ZstdLevel: 4})
	handler := jsonHandler(body)

	ctx, rec := celeristest.NewContext("GET", "/data",
		celeristest.WithHeader("accept-encoding", "zstd"),
		celeristest.WithHandlers(mw, handler),
	)
	defer celeristest.ReleaseContext(ctx)

	if err := ctx.Next(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Header("content-encoding") != "zstd" {
		t.Fatalf("expected zstd with max level, got %q", rec.Header("content-encoding"))
	}
	decompressed := decompressZstd(t, rec.Body)
	if !bytes.Equal(decompressed, body) {
		t.Fatalf("decompressed body mismatch with zstd level 4")
	}
}

func TestValidateZstdLevel11Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for ZstdLevel 11")
		}
	}()
	New(Config{Encodings: []string{"zstd"}, ZstdLevel: 11})
}

// --- Deflate encoding tests ---

func TestDeflateCompression(t *testing.T) {
	body := testBody()
	mw := New(Config{Encodings: []string{"deflate"}})
	handler := jsonHandler(body)

	ctx, rec := celeristest.NewContext("GET", "/data",
		celeristest.WithHeader("accept-encoding", "deflate"),
		celeristest.WithHandlers(mw, handler),
	)
	defer celeristest.ReleaseContext(ctx)

	if err := ctx.Next(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if rec.Header("content-encoding") != "deflate" {
		t.Fatalf("expected content-encoding deflate, got %q", rec.Header("content-encoding"))
	}
	decompressed := decompressDeflate(t, rec.Body)
	if !bytes.Equal(decompressed, body) {
		t.Fatalf("decompressed body mismatch")
	}
}

func TestDeflateNegotiation(t *testing.T) {
	body := testBody()
	mw := New(Config{Encodings: []string{"gzip", "deflate"}})
	handler := jsonHandler(body)

	ctx, rec := celeristest.NewContext("GET", "/data",
		celeristest.WithHeader("accept-encoding", "deflate"),
		celeristest.WithHandlers(mw, handler),
	)
	defer celeristest.ReleaseContext(ctx)

	if err := ctx.Next(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Header("content-encoding") != "deflate" {
		t.Fatalf("expected deflate, got %q", rec.Header("content-encoding"))
	}
	decompressed := decompressDeflate(t, rec.Body)
	if !bytes.Equal(decompressed, body) {
		t.Fatalf("decompressed body mismatch")
	}
}

func TestDeflateNotInDefaultEncodings(t *testing.T) {
	body := testBody()
	// Default config does not include deflate.
	mw := New()
	handler := jsonHandler(body)

	ctx, rec := celeristest.NewContext("GET", "/data",
		celeristest.WithHeader("accept-encoding", "deflate"),
		celeristest.WithHandlers(mw, handler),
	)
	defer celeristest.ReleaseContext(ctx)

	if err := ctx.Next(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Deflate is not in the default encodings list, so should not compress.
	if rec.Header("content-encoding") != "" {
		t.Fatalf("expected no compression with default config for deflate-only client, got %q", rec.Header("content-encoding"))
	}
}

func TestDeflateLevelBest(t *testing.T) {
	body := testBody()
	mw := New(Config{
		Encodings:    []string{"deflate"},
		DeflateLevel: LevelBest,
	})
	handler := jsonHandler(body)

	ctx, rec := celeristest.NewContext("GET", "/data",
		celeristest.WithHeader("accept-encoding", "deflate"),
		celeristest.WithHandlers(mw, handler),
	)
	defer celeristest.ReleaseContext(ctx)

	if err := ctx.Next(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Header("content-encoding") != "deflate" {
		t.Fatalf("expected deflate, got %q", rec.Header("content-encoding"))
	}
	decompressed := decompressDeflate(t, rec.Body)
	if !bytes.Equal(decompressed, body) {
		t.Fatalf("decompressed body mismatch with LevelBest")
	}
}

func TestDeflateValidatePanics(t *testing.T) {
	tests := []struct {
		name   string
		config Config
	}{
		{
			"deflate level too low",
			Config{Encodings: []string{"deflate"}, DeflateLevel: -3},
		},
		{
			"deflate level too high",
			Config{Encodings: []string{"deflate"}, DeflateLevel: 10},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Fatal("expected panic")
				}
			}()
			cfg := applyDefaults(tt.config)
			cfg.validate()
		})
	}
}

func TestDeflateValidateAcceptsValid(t *testing.T) {
	tests := []struct {
		name   string
		config Config
	}{
		{
			"deflate with LevelDefault",
			Config{Encodings: []string{"deflate"}, DeflateLevel: LevelDefault, ExcludedContentTypes: []string{"image/"}},
		},
		{
			"deflate with LevelNone",
			Config{Encodings: []string{"deflate"}, DeflateLevel: LevelNone, ExcludedContentTypes: []string{"image/"}},
		},
		{
			"deflate with LevelBest",
			Config{Encodings: []string{"deflate"}, DeflateLevel: LevelBest, ExcludedContentTypes: []string{"image/"}},
		},
		{
			"deflate with raw level 5",
			Config{Encodings: []string{"deflate"}, DeflateLevel: 5, ExcludedContentTypes: []string{"image/"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("unexpected panic: %v", r)
				}
			}()
			cfg := applyDefaults(tt.config)
			cfg.validate()
		})
	}
}
