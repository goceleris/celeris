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
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs/static-files
package static
