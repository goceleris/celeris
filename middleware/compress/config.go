package compress

import (
	"slices"
	"strings"

	"github.com/goceleris/celeris"
)

// Level controls the compression level for a given encoding.
// The zero value (LevelDefault) selects the library default, which is
// idiomatic Go: a zero-value Config uses sensible defaults for all levels.
type Level int

const (
	// LevelDefault uses the library default compression level.
	// It is intentionally the zero value so that an unset field in Config
	// falls through to the library default.
	LevelDefault Level = 0
	// LevelNone stores data without compression (gzip level 0, brotli level 0).
	LevelNone Level = -1
	// LevelBest is a sentinel that resolves to the encoding-specific maximum:
	// gzip -> 9, brotli -> 11, zstd -> SpeedBestCompression (4).
	LevelBest Level = -2
	// LevelFastest uses the fastest (lowest) compression level.
	LevelFastest Level = 1
)

// Config defines the compress middleware configuration.
type Config struct {
	// Skip defines a function to skip this middleware for certain requests.
	Skip func(c *celeris.Context) bool

	// SkipPaths lists paths to skip from compression (exact match).
	SkipPaths []string

	// MinLength is the minimum response body size (in bytes) required for
	// compression. Responses smaller than this are sent uncompressed.
	// Default: 256.
	//
	// Note: MinLength cannot be set to 0 to mean "compress all" — a value
	// of 0 is treated as "use default" (256). Set MinLength to 1 to compress
	// all non-empty responses regardless of size.
	MinLength int

	// Encodings lists the supported encodings in server-side priority order.
	// Supported values: "zstd", "br", "gzip".
	// Default: ["zstd", "br", "gzip"].
	Encodings []string

	// GzipLevel sets the gzip compression level.
	// Default: LevelDefault (0 = library default, gzip level 6).
	// Use LevelNone (-1) for store-only (no compression).
	GzipLevel Level

	// BrotliLevel sets the brotli compression level.
	// Default: LevelDefault (0 = library default, brotli level 6).
	// Use LevelNone (-1) for no compression.
	BrotliLevel Level

	// ZstdLevel sets the zstd compression level.
	// Default: LevelDefault (0 = library default).
	// Use LevelNone (-1) for no compression.
	ZstdLevel Level

	// ExcludedContentTypes lists content-type prefixes that should not be
	// compressed. The check is case-insensitive and uses prefix matching.
	// Default: ["image/", "video/", "audio/"].
	ExcludedContentTypes []string
}

// defaultBrotliLevel is the library-default brotli compression level (6).
const defaultBrotliLevel Level = 6

var defaultConfig = Config{
	MinLength:            256,
	Encodings:            []string{"zstd", "br", "gzip"},
	GzipLevel:            LevelDefault,
	BrotliLevel:          LevelDefault,
	ZstdLevel:            LevelDefault,
	ExcludedContentTypes: []string{"image/", "video/", "audio/"},
}

var supportedEncodings = map[string]struct{}{
	"gzip": {},
	"br":   {},
	"zstd": {},
}

func applyDefaults(cfg Config) Config {
	// MinLength: 0 means "use default" (256). To compress all sizes, set
	// MinLength to 1. This is documented in the Config struct.
	if cfg.MinLength == 0 {
		cfg.MinLength = defaultConfig.MinLength
	}
	if len(cfg.Encodings) == 0 {
		cfg.Encodings = defaultConfig.Encodings
	}
	// Levels: LevelDefault (0, the zero value) maps to library defaults in
	// pool initialization. No override needed — the zero value IS the default.
	if len(cfg.ExcludedContentTypes) == 0 {
		cfg.ExcludedContentTypes = defaultConfig.ExcludedContentTypes
	}
	return cfg
}

func (cfg Config) validate() {
	if len(cfg.Encodings) == 0 {
		panic("compress: Encodings must not be empty")
	}
	for _, enc := range cfg.Encodings {
		if _, ok := supportedEncodings[enc]; !ok {
			panic("compress: unsupported encoding: " + enc)
		}
	}
	if slices.Contains(cfg.Encodings, "gzip") {
		if cfg.GzipLevel != LevelBest && (cfg.GzipLevel < -1 || cfg.GzipLevel > 9) {
			panic("compress: GzipLevel must be between -1 (LevelNone) and 9")
		}
	}
	if slices.Contains(cfg.Encodings, "br") {
		if cfg.BrotliLevel != LevelBest && (cfg.BrotliLevel < -1 || cfg.BrotliLevel > 11) {
			panic("compress: BrotliLevel must be between -1 (LevelNone) and 11")
		}
	}
	if slices.Contains(cfg.Encodings, "zstd") {
		if cfg.ZstdLevel != LevelBest && (cfg.ZstdLevel < -1 || cfg.ZstdLevel > 11) {
			panic("compress: ZstdLevel must be between -1 (LevelNone) and 11")
		}
	}
	if cfg.MinLength < 0 {
		panic("compress: MinLength must be non-negative")
	}
	for _, ct := range cfg.ExcludedContentTypes {
		if ct == "" {
			panic("compress: ExcludedContentTypes must not contain empty strings")
		}
	}
}

// isExcluded returns true if the content type matches any excluded prefix.
func isExcluded(contentType string, excluded []string) bool {
	ct := strings.ToLower(contentType)
	for _, prefix := range excluded {
		if strings.HasPrefix(ct, prefix) {
			return true
		}
	}
	return false
}
