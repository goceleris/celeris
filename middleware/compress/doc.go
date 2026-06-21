// Package compress provides transparent response compression middleware
// for celeris.
//
// It negotiates an encoding from the Accept-Encoding request header and
// compresses 2xx responses on the fly. Supported encodings are zstd, brotli
// ("br"), gzip, and deflate; the default server-side priority is
// zstd > br > gzip. Deflate is supported but opt-in (add "deflate" to
// [Config].Encodings) because it is superseded by gzip.
//
// [New] returns the middleware; call it with no arguments for defaults
// (MinLength 256, the default encoding list and excluded content types) or
// pass a [Config] to tune behavior:
//
//	server.Use(compress.New())
//
// Per-encoding levels are set via [Config].GzipLevel, [Config].BrotliLevel,
// [Config].ZstdLevel, and [Config].DeflateLevel, each accepting a [Level]
// (LevelDefault, LevelFastest, LevelBest, LevelNone) or an integer in the
// encoding's valid range. Use [Config].Skip, [Config].SkipPaths,
// [Config].MinLength, and [Config].ExcludedContentTypes to control what gets
// compressed.
//
// The middleware buffers the response body before compressing, so it does not
// apply to handlers that use [celeris.Context.StreamWriter]. For streaming
// endpoints, wrap a StreamWriter with [NewCompressedStream] (gzip or brotli
// only), or bypass compression with [Config].Skip.
//
// This package is a separate Go module (middleware/compress/go.mod); import
// it as "github.com/goceleris/celeris/middleware/compress".
//
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs/middleware-content
package compress
