// Package static serves static files from the OS filesystem or an [fs.FS].
//
// Basic usage with an OS directory:
//
//	server.Use(static.New(static.Config{Root: "./public"}))
//
// With an embedded filesystem:
//
//	//go:embed assets/*
//	var assets embed.FS
//	server.Use(static.New(static.Config{FS: assets}))
//
// With a URL prefix:
//
//	server.Use(static.New(static.Config{
//	    Root:   "./public",
//	    Prefix: "/static",
//	}))
//
// # Prefix Matching
//
// The Prefix is matched at segment boundaries. A prefix of "/api" will match
// "/api/file.txt" but NOT "/api-docs/file.txt". This prevents unintended
// path collisions.
//
// # Directory Browsing
//
// When Browse is enabled and the request path resolves to a directory without
// an index file, an HTML directory listing is rendered. Filenames in the
// listing are URL-encoded in href attributes and HTML-escaped in display
// text to prevent XSS via crafted filenames.
//
// # SPA Mode
//
// When [Config].SPA is true the middleware operates in single-page
// application mode: requests for non-existent files serve the index file
// (default index.html) instead of falling through to the next handler.
// This allows client-side routers to handle arbitrary URL paths. Existing
// files and directories are still served normally.
//
// # Caching
//
// The middleware sets Last-Modified and ETag headers based on file metadata.
// Conditional requests (If-Modified-Since, If-None-Match) return 304 Not
// Modified when the client already has a fresh copy.
//
// The ETag is a weak validator computed from the file's modification time
// and size: W/"<mtime_hex>-<size_hex>". For embed.FS files where ModTime
// is zero, caching headers are omitted.
//
// # Cache-Control
//
// [Config].MaxAge sets the Cache-Control max-age directive. When MaxAge is
// greater than zero the response includes a "public, max-age=N" header
// (where N is seconds). When MaxAge is zero (the default) no Cache-Control
// header is added. MaxAge works alongside ETag and Last-Modified: browsers
// that have a cached copy within the max-age window skip the network
// entirely, and once the window expires they can still use conditional
// requests to validate freshness.
//
// # Range Requests
//
// OS files support range requests via the core [celeris.Context.FileFromDir]
// method. For fs.FS files, the middleware implements range request handling
// inline, supporting single byte ranges (bytes=start-end). When the underlying
// fs.File implements [io.ReadSeeker], range requests seek to the requested
// offset and read only the required bytes instead of loading the entire file
// into memory.
//
// # Content-Type Detection
//
// Content types are determined by file extension via [mime.TypeByExtension].
// When the extension is unknown or absent, the middleware sniffs the first 512
// bytes using [http.DetectContentType] (for fs.FS files) or delegates to the
// core file-serving method (for OS files).
//
// # Pre-Compressed Files
//
// When [Config].Compress is true, the middleware checks for pre-compressed
// variants of the requested file before serving the original. It inspects
// the Accept-Encoding request header and looks for a .br (Brotli) or .gz
// (gzip) file alongside the original. Brotli is preferred when both are
// accepted. The response includes Content-Encoding and Vary headers.
//
// Pre-compressed file serving only applies to OS filesystem (Root), not
// fs.FS. This pairs well with build tools that generate .br and .gz files
// at deploy time (e.g. Vite, esbuild, or a post-build compression step).
//
// # Security
//
// OS files are served through [celeris.Context.FileFromDir], which prevents
// directory traversal and symlink escape. For fs.FS files, paths are cleaned
// before opening.
//
// # Method Filtering
//
// Only GET and HEAD requests are processed. All other methods pass through
// to the next handler.
//
// # Ordering
//
// Place the static middleware after security and authentication middleware
// if you need to protect static files. Place it before compress and etag
// if you want framework-level caching/compression (though the static
// middleware sets its own ETag from file metadata, which the etag middleware
// will preserve).
//
// # Skipping
//
// Use [Config].Skip for dynamic skip logic or [Config].SkipPaths for
// exact-match path exclusions. Skipped requests call c.Next() without
// serving any files.
package static
