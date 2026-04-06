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
package static
