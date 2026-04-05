// Package compress provides transparent response compression middleware
// for celeris.
//
// Supported encodings are zstd, brotli, and gzip, negotiated via the
// Accept-Encoding request header. The server-side priority order is
// configurable; the default prefers zstd > brotli > gzip.
//
// Basic usage with defaults (zstd > br > gzip, MinLength 256):
//
//	server.Use(compress.New())
//
// Gzip-only with fastest compression:
//
//	server.Use(compress.New(compress.Config{
//	    Encodings: []string{"gzip"},
//	    GzipLevel: compress.LevelFastest,
//	}))
//
// Exclude specific content types and paths:
//
//	server.Use(compress.New(compress.Config{
//	    ExcludedContentTypes: []string{"image/", "video/", "audio/", "application/octet-stream"},
//	    SkipPaths:            []string{"/health", "/metrics"},
//	}))
//
// # Content-Negotiation
//
// The middleware calls [celeris.Context.AcceptsEncodings] with the
// configured encoding list. If the client does not accept any supported
// encoding, the response passes through uncompressed.
//
// # Status Code Range
//
// Only 2xx responses are compressed. Error responses (4xx, 5xx), redirects
// (3xx), and informational responses (1xx) pass through uncompressed.
//
// # Pool Strategy
//
// Writer pools eliminate per-request allocations on the hot path:
//
//   - gzip: [sync.Pool] of *gzip.Writer — Get, Reset, Write, Close, Put.
//   - brotli: [sync.Pool] of *brotli.Writer — same pattern.
//   - zstd: single thread-safe [zstd.Encoder] — EncodeAll, no pool needed.
//   - buffers: [sync.Pool] of *bytes.Buffer for gzip/brotli output.
//
// # Response Buffering
//
// The middleware fully buffers the downstream response body before deciding
// whether to compress. This means the entire response is held in memory.
// For large responses (e.g., file downloads, streaming SSE), use
// [Config].Skip or [Config].SkipPaths to bypass compression and avoid
// excessive memory usage.
//
// For file-serving or export endpoints that produce large responses,
// use Skip or SkipPaths to bypass compression and avoid excessive memory use:
//
//	compress.New(compress.Config{
//	    SkipPaths: []string{"/api/export", "/files"},
//	    Skip: func(c *celeris.Context) bool {
//	        return strings.HasPrefix(c.Path(), "/download/")
//	    },
//	})
//
// # MinLength Threshold
//
// Responses smaller than [Config].MinLength bytes (default 256) are not
// compressed. This avoids wasting CPU on responses too small to benefit
// from compression.
//
// # Excluded Content Types
//
// Content types matching [Config].ExcludedContentTypes prefixes are
// skipped. The default excludes "image/", "video/", and "audio/" since
// these are typically already compressed.
//
// # Vary Header
//
// The middleware appends "Accept-Encoding" to the Vary header using
// [celeris.Context.AddHeader] so that existing Vary values (e.g., Origin
// from CORS) are preserved. The Vary header is added on all non-skipped
// responses, including those that pass through uncompressed (below
// MinLength, excluded content type, etc.), so that caches correctly
// distinguish responses that vary by encoding.
//
// # Compression Expansion Guard
//
// If the compressed output is equal to or larger than the original body,
// the original is sent uncompressed. This prevents pathological expansion
// on already-compressed or incompressible data.
//
// # Ordering with ETag
//
// Compress runs OUTSIDE etag middleware. The recommended chain is:
//
//	server.Use(compress.New())  // outermost
//	server.Use(etag.New())      // inner — ETag computed on uncompressed body
//
// # Separate Sub-Module
//
// This package is a separate Go module (middleware/compress/go.mod) with
// its own dependency set. Import it as:
//
//	import "github.com/goceleris/celeris/middleware/compress"
package compress
