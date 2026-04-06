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
// inline, supporting single byte ranges (bytes=start-end).
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
