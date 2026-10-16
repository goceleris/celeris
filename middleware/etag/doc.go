// Package etag provides ETag generation and conditional-response middleware
// for celeris.
//
// [New] returns middleware that buffers the response body of GET and HEAD
// requests, computes an ETag validator, and sets the ETag header. When a
// request carries a matching If-None-Match header, it returns 304 Not
// Modified with no body. Other methods and non-2xx or empty responses pass
// through untouched. A 206 Partial Content is never hashed: its body is one
// part, whose hash is not the representation's tag. If the downstream
// handler already set an ETag header, that tag is reused rather than
// recomputed, on a 206 too, where a matching If-None-Match answers 304 as it
// does for a full response.
//
// By default the validator is a CRC-32 (IEEE) checksum of the body, emitted
// as a weak tag (W/"xxxxxxxx"). Configure behavior through [Config]:
//   - Strong: emit strong tags ("xxxxxxxx") instead of weak.
//   - HashFunc: supply a custom hash; it returns the opaque-tag, and the
//     quotes and optional W/ prefix are added automatically.
//   - Skip / SkipPaths: bypass buffering for dynamic conditions or for
//     exact paths (e.g. large downloads, streaming endpoints).
//
// With the compress middleware in front, which has negotiated an encoding for
// the request, a strong tag (Strong: true, or one the handler set) is marked for
// it, so that the 304 carries the weak form of the tag that compress gives the
// encoded 200 (RFC 9110 15.4.5). A weak tag needs no mark, and without compress
// in front, or when it negotiated no encoding for the request, no mark is made.
//
// Because the body is buffered to compute the tag, etag should run INSIDE
// compression middleware so the validator is computed on the uncompressed
// body.
//
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs/middleware-content
package etag
