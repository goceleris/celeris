// Package etag provides ETag generation and conditional-response middleware
// for celeris.
//
// [New] returns middleware that buffers the response body of GET and HEAD
// requests, computes an ETag validator, and sets the ETag header. When a
// request carries a matching If-None-Match header, it returns 304 Not
// Modified with no body. Other methods and non-2xx or empty responses pass
// through untouched. If the downstream handler already set an ETag header,
// that tag is reused rather than recomputed.
//
// By default the validator is a CRC-32 (IEEE) checksum of the body, emitted
// as a weak tag (W/"xxxxxxxx"). Configure behavior through [Config]:
//   - Strong: emit strong tags ("xxxxxxxx") instead of weak.
//   - HashFunc: supply a custom hash; it returns the opaque-tag, and the
//     quotes and optional W/ prefix are added automatically.
//   - Skip / SkipPaths: bypass buffering for dynamic conditions or for
//     exact paths (e.g. large downloads, streaming endpoints).
//
// Because the body is buffered to compute the tag, etag should run INSIDE
// compression middleware so the validator is computed on the uncompressed
// body.
//
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs/middleware-content
package etag
