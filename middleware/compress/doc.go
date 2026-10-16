// Package compress provides transparent response compression middleware
// for celeris.
//
// It negotiates an encoding from the Accept-Encoding request header and
// compresses 2xx responses on the fly, except a 206 Partial Content, whose
// Content-Range counts bytes of the unencoded representation and which is
// sent as it is. Supported encodings are zstd, brotli
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
// # ETag
//
// A strong ETag set below compress (by middleware/static over an fs.FS, or by the
// etag middleware) is a hash of the identity bytes. When compress encodes the
// response it makes the tag weak (W/"hash"): RFC 9110 8.8.3 says a strong
// validator changes with every octet of the representation, Content-Encoding
// included, and a weak tag never matches If-Range, so a client that resumes
// the encoded download gets the whole file again and not a 206 cut from the
// identity bytes. A 304 carries the tag a 200 to the same request would carry
// (RFC 9110 15.4.5), so compress weakens the strong tag of a 304 too, for a
// response whose handler marked the representation as one compress would
// encode. static (over an fs.FS) and etag mark; the mark is internal to this
// repository's middleware and no API. A 304 that another handler makes with its
// own strong tag is not marked and keeps the tag as it is, while the encoded 200
// of that handler carries the weak one: put etag below compress and let it
// reuse the handler's tag, and the 304 follows. A marked response that compress
// does not send encoded because the encoded form would be longer carries the
// weak tag as well, since the 304 cannot know that length.
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
