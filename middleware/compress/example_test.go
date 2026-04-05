package compress_test

import (
	"github.com/goceleris/celeris/middleware/compress"
)

func ExampleNew() {
	// Default: zstd > brotli > gzip, MinLength 256.
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
