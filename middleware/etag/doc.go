// Package etag provides automatic ETag generation and conditional response
// middleware for celeris.
//
// The middleware computes a CRC-32 checksum of the response body and sets
// an ETag header. On subsequent requests with a matching If-None-Match
// header, it returns 304 Not Modified with no body, saving bandwidth.
//
// Basic usage:
//
//	server.Use(etag.New())
//
// Strong ETags (byte-for-byte identical guarantee):
//
//	server.Use(etag.New(etag.Config{Strong: true}))
//
// # Algorithm
//
// The ETag value is computed using CRC-32 (IEEE polynomial) of the full
// response body. By default, weak ETags are generated in the format
// W/"xxxxxxxx" where xxxxxxxx is the hex-encoded checksum. Strong ETags
// omit the W/ prefix: "xxxxxxxx".
//
// CRC-32 is chosen for speed (single pass, no allocations beyond the
// fixed-size stack buffer). It is not cryptographically secure, but
// ETag is a cache validation mechanism, not a security primitive.
//
// # Custom Hash Function
//
// Use [Config].HashFunc to supply a custom hash. The function receives the
// full response body and returns the opaque-tag string (without quotes or
// W/ prefix -- those are added automatically based on the Strong setting):
//
//	server.Use(etag.New(etag.Config{
//	    HashFunc: func(body []byte) string {
//	        h := sha256.Sum256(body)
//	        return hex.EncodeToString(h[:16])
//	    },
//	}))
//
// # Handler-Set ETags
//
// If the downstream handler already sets an ETag header, the middleware
// respects it and does not recompute. The existing ETag is still checked
// against If-None-Match for conditional 304 responses.
//
// # Weak Comparison (RFC 7232 Section 2.3.2)
//
// If-None-Match uses weak comparison: the W/ prefix is stripped before
// comparing opaque-tags. This means W/"abc" matches both W/"abc" and
// "abc", per the RFC.
//
// # If-None-Match: *
//
// The wildcard value "*" matches any ETag, unconditionally returning 304.
//
// # If-Match (Not Supported)
//
// This middleware does not handle If-Match (RFC 7232 Section 3.1). If-Match
// is used for conditional writes (PUT/DELETE) and should be validated at
// the application layer. The middleware only processes GET/HEAD requests.
//
// # If-None-Match Lenient Parsing
//
// The If-None-Match parser accepts unquoted tokens in addition to properly
// quoted ETag values. While RFC 7232 specifies that entity-tags must be
// quoted strings, real-world clients and proxies occasionally emit bare
// tokens. The parser handles these gracefully to avoid spurious cache misses.
//
// # Method Filtering
//
// Only GET and HEAD requests are processed. POST, PUT, DELETE, PATCH,
// and other methods bypass the middleware entirely (no buffering, no
// ETag computation) with zero allocations.
//
// # Content-Length on 304
//
// When returning 304 Not Modified, any Content-Length header set by the
// downstream handler is preserved. Per RFC 9110 Section 15.4.5, a 304
// response MUST generate headers that would have been sent in a 200
// response, including Content-Length when it matches the selected
// representation.
//
// # Full-Body Buffering
//
// The middleware buffers the entire response body in memory to compute
// the ETag checksum. For large responses, this increases memory usage.
// Use [Config].Skip or [Config].SkipPaths to bypass ETag processing
// for endpoints that return large payloads (file downloads, streaming):
//
//	server.Use(etag.New(etag.Config{
//	    SkipPaths: []string{"/download", "/export"},
//	}))
//
// # Middleware Ordering
//
// ETag should run INSIDE compression middleware (closer to the handler).
// This ensures the ETag is computed on the uncompressed body, so clients
// that re-request without Accept-Encoding still get a cache hit:
//
//	server.Use(compress.New())  // outer: compresses the response
//	server.Use(etag.New())      // inner: ETag on uncompressed body
//
// # Skipping
//
// Use [Config].Skip for dynamic skip logic or [Config].SkipPaths for
// path exclusions. SkipPaths uses exact path matching:
//
//	server.Use(etag.New(etag.Config{
//	    SkipPaths: []string{"/health", "/metrics"},
//	}))
//
// # Non-2xx and Empty Responses
//
// Responses with non-2xx status codes or empty bodies are flushed
// without ETag computation. Only successful responses with content
// participate in conditional caching.
//
// # Session Middleware Interaction
//
// When session middleware runs alongside ETag, 304 Not Modified responses
// still carry the Set-Cookie header from session refresh. This is correct
// behavior (sessions must be refreshed for security), but it prevents
// shared caches (CDN, reverse proxy) from caching 304 responses per
// RFC 7234 Section 3.1. Browser-level caching is unaffected.
//
// For static assets that benefit from shared-cache ETag optimization,
// consider using SkipPaths on the session middleware to exclude those paths.
package etag
