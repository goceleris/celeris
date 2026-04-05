package compress_test

import (
	"strings"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/compress"
)

func ExampleNew() {
	// Default: zstd > brotli > gzip, MinLength 256.
	//   server.Use(compress.New())
	_ = compress.New()
}

func ExampleNew_gzipOnly() {
	_ = compress.New(compress.Config{
		Encodings: []string{"gzip"},
		GzipLevel: compress.LevelFastest,
	})
}

func ExampleNew_customMinLength() {
	_ = compress.New(compress.Config{
		MinLength: 1024,
	})
}

func ExampleNew_excludedContentTypes() {
	_ = compress.New(compress.Config{
		ExcludedContentTypes: []string{
			"image/",
			"video/",
			"audio/",
			"application/octet-stream",
		},
	})
}

func ExampleNew_skipPaths() {
	_ = compress.New(compress.Config{
		SkipPaths: []string{"/health", "/metrics", "/debug/pprof"},
	})
}

func ExampleNew_noCompression() {
	// LevelNone stores data without compression (useful for testing).
	_ = compress.New(compress.Config{
		Encodings: []string{"gzip"},
		GzipLevel: compress.LevelNone,
	})
}

func ExampleNew_skip() {
	// Dynamic skip: bypass compression for WebSocket upgrades and
	// streaming download endpoints.
	_ = compress.New(compress.Config{
		Skip: func(c *celeris.Context) bool {
			if c.Header("upgrade") == "websocket" {
				return true
			}
			return strings.HasPrefix(c.Path(), "/download/")
		},
	})
}

func ExampleNew_zstdLevel() {
	// Use SpeedBestCompression (level 4) for zstd.
	_ = compress.New(compress.Config{
		Encodings: []string{"zstd"},
		ZstdLevel: compress.LevelBest,
	})
}

func ExampleNew_deflate() {
	// Opt-in to deflate for legacy client support. Deflate is not
	// included in the default Encodings list.
	_ = compress.New(compress.Config{
		Encodings:    []string{"zstd", "br", "gzip", "deflate"},
		DeflateLevel: compress.LevelDefault,
	})
}
