// Package compress provides transparent response compression middleware
// for celeris.
//
// Supported encodings are zstd, brotli, gzip, and deflate, negotiated via
// the Accept-Encoding request header. The server-side priority order is
// configurable; the default prefers zstd > brotli > gzip. Deflate is
// supported but opt-in only (not in the default Encodings list) because
// it is a legacy encoding superseded by gzip.
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
// Per-encoding compression levels are configurable via [Config].GzipLevel,
// [Config].BrotliLevel, and [Config].ZstdLevel. Each accepts [Level]
// constants (LevelDefault, LevelFastest, LevelBest, LevelNone) or an
// integer within the encoding's valid range.
//
// Exclude specific content types and paths:
//
//	server.Use(compress.New(compress.Config{
//	    ExcludedContentTypes: []string{"image/", "video/", "audio/", "application/octet-stream"},
//	    SkipPaths:            []string{"/health", "/metrics"},
//	}))
//
// # Method Filtering
//
// HEAD and OPTIONS requests bypass compression entirely (only Vary is added).
// This avoids unnecessary buffering for requests that do not produce
// response bodies.
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
// # Deflate (opt-in)
//
// The "deflate" encoding (raw DEFLATE, RFC 1951) is supported but not
// included in the default Encodings list. Add it explicitly:
//
//	server.Use(compress.New(compress.Config{
//	    Encodings: []string{"zstd", "br", "gzip", "deflate"},
//	}))
//
// Deflate is a legacy encoding. Prefer gzip, brotli, or zstd for new
// deployments.
//
// # Streaming Compression
//
// For endpoints that produce large or streaming responses, use
// [NewCompressedStream] to wrap a [celeris.StreamWriter] with on-the-fly
// gzip or brotli compression. This avoids buffering the entire response
// body but does not support the expansion guard or MinLength threshold.
//
// # Pool Strategy
//
// Writer pools eliminate per-request allocations on the hot path:
//
//   - gzip: [sync.Pool] of *gzip.Writer — Get, Reset, Write, Close, Put.
//   - brotli: [sync.Pool] of *brotli.Writer — same pattern.
//   - deflate: [sync.Pool] of *flate.Writer — same pattern.
//   - zstd: single thread-safe [zstd.Encoder] — EncodeAll, no pool needed.
//   - buffers: [sync.Pool] of *bytes.Buffer for gzip/brotli/deflate output.
//
// # StreamWriter Incompatibility
//
// This middleware uses [celeris.Context.BufferResponse] which is incompatible
// with [celeris.Context.StreamWriter]. If the downstream handler uses
// StreamWriter, BufferResponse returns nil and the streamed response
// bypasses this middleware entirely. Use [Config].Skip to explicitly
// exclude streaming endpoints.
//
// # Response Buffering (No Streaming Compression)
//
// This middleware buffers the entire response body in memory before
// deciding whether and how to compress. This design enables the expansion
// guard (flush original if compressed >= original) and correct
// Content-Length headers, but means the full uncompressed body must fit
// in memory.
//
// For endpoints that produce large responses (file downloads, CSV exports,
// streaming JSON), use [Config].Skip or [Config].SkipPaths to bypass
// compression entirely. Consider using the framework's StreamWriter for
// such endpoints instead.
//
//	compress.New(compress.Config{
//	    SkipPaths: []string{"/api/export", "/files"},
//	    Skip: func(c *celeris.Context) bool {
//	        return strings.HasPrefix(c.Path(), "/download/")
//	    },
//	})
//
// Competing frameworks (Echo, Fiber) offer streaming compression by
// wrapping the response writer, but this prevents expansion detection
// and accurate Content-Length.
//
// # MinLength Threshold
//
// Responses smaller than [Config].MinLength bytes (default 256) are not
// compressed. Set MinLength to 0 to compress all non-empty responses.
// This avoids wasting CPU on responses too small to benefit from compression.
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
// # BREACH Attack Warning
//
// Compressing HTTPS responses that reflect user input (search queries,
// form values, URL parameters) alongside secrets (CSRF tokens, session
// IDs) can leak those secrets via the BREACH attack. Mitigation options:
//   - Exclude sensitive endpoints with [Config].SkipPaths or [Config].Skip
//   - Randomize padding on pages that mix user input and secrets
//   - Separate secret-bearing responses from user-controlled content
//
// See http://breachattack.com for details. This applies to any HTTP
// compression layer, not just this middleware.
//
// # Response Size Measurement
//
// When metrics or OTel middleware runs before compress (the recommended
// ordering), response size metrics reflect uncompressed application-level
// sizes. See middleware/metrics and middleware/otel documentation.
//
// # Separate Sub-Module
//
// This package is a separate Go module (middleware/compress/go.mod) with
// its own dependency set. Import it as:
//
//	import "github.com/goceleris/celeris/middleware/compress"
package compress
