// Package static serves static files from the OS filesystem or an [fs.FS].
//
// Use [New] with a [Config] to mount the middleware. Set [Config].Root for an
// OS directory or [Config].FS for an embedded/virtual filesystem (mutually
// exclusive; FS takes precedence when both are set). Key options:
//
//   - [Config].Prefix — URL path prefix, matched at segment boundaries.
//   - [Config].Index — directory index filename (default "index.html").
//   - [Config].Browse — enable HTML directory listings.
//   - [Config].SPA — single-page app mode: unknown paths serve the index file.
//   - [Config].MaxAge — Cache-Control max-age duration (zero = no header).
//   - [Config].Compress — serve pre-compressed .br/.gz variants when accepted.
//   - [Config].Skip / [Config].SkipPaths — skip the middleware dynamically or
//     by exact path match.
//
// Only GET and HEAD requests are processed; all others pass through. The
// middleware sets Last-Modified, ETag (weak, mtime+size), and optional
// Cache-Control headers and handles conditional requests (304 Not Modified).
//
// A GET with a single byte range gets 206 Partial Content; a range no byte of
// the file satisfies gets 416 Range Not Satisfiable with
// "Content-Range: bytes */<size>". If-Range is honoured: the range is served
// only while the client's validator still matches, otherwise the whole file
// is sent as a 200. Because the ETag is weak and If-Range uses the strong
// comparison, only a Last-Modified date can match here (RFC 9110 §13.1.5).
// HEAD ignores Range, and a request for several ranges gets the whole file.
//
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs/static-files
package static
