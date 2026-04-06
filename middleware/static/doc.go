// Package static provides file-serving middleware for celeris.
//
// The middleware serves static files from an OS directory or an [fs.FS]
// (such as [embed.FS] or [testing/fstest.MapFS]). It handles GET and HEAD
// requests only; all other methods pass through to the next handler.
//
// Basic usage (OS directory):
//
//	server.Use(static.New(static.Config{
//	    Root: "./public",
//	}))
//
// Embedded filesystem:
//
//	//go:embed assets
//	var assets embed.FS
//
//	server.Use(static.New(static.Config{
//	    Filesystem: assets,
//	    Prefix:     "/assets",
//	}))
//
// # OS vs fs.FS
//
// When [Config].Root is set, files are served from the OS filesystem using
// [celeris.Context.FileFromDir], which provides path traversal and symlink
// escape protection. When [Config].Filesystem is set, files are served via
// [celeris.Context.FileFromFS] with path sanitization (Clean + ".." rejection).
// Exactly one of Root or Filesystem must be set.
//
// # SPA Mode
//
// When [Config].SPA is true, requests for non-existent files serve the
// [Config].Index file (default "index.html") instead of falling through to
// the next handler. This supports single-page applications where client-side
// routing handles unknown paths. Existing files are always served directly,
// even in SPA mode.
//
// # Cache-Control
//
// Set [Config].MaxAge to a positive number of seconds to add a
// Cache-Control: public, max-age=N header to served files. Zero (default)
// omits the header. The header is only set on successful file responses;
// it is cleared when falling through to the next handler.
//
// # Path Traversal Protection
//
// For OS filesystems, [celeris.Context.FileFromDir] resolves symlinks and
// verifies the resolved path stays within the root directory. For fs.FS,
// the middleware cleans the path with [path.Clean] and rejects any path
// containing "..". Combined, these measures prevent directory traversal
// attacks (CWE-22).
//
// # Prefix Stripping
//
// [Config].Prefix strips a URL prefix before file lookup. For example,
// Prefix "/static" maps GET /static/app.js to the file app.js in the root.
// The prefix must start with "/". Requests that do not match the prefix
// pass through to the next handler.
//
// # Browse Mode
//
// When [Config].Browse is true, directory requests that lack an index file
// receive a simple HTML listing of directory entries. This is useful for
// development but should be disabled in production to avoid information
// disclosure.
//
// # Middleware Ordering
//
// Static file middleware should run BEFORE application routes and AFTER
// security middleware (cors, csrf, secure). For cache-friendly setups,
// place it before etag and compress so those can post-process the response:
//
//	server.Use(compress.New())
//	server.Use(etag.New())
//	server.Use(static.New(static.Config{Root: "./public"}))
//
// # GET and HEAD Only
//
// Only GET and HEAD requests are handled. POST, PUT, DELETE, PATCH, and
// other methods bypass the middleware entirely (no file lookup, no disk I/O)
// and proceed to the next handler.
//
// # Index File
//
// When a request path resolves to a directory, the middleware attempts to
// serve [Config].Index (default "index.html") from that directory. If the
// index file does not exist and Browse is false, the request falls through
// to the next handler.
//
// # Skipping
//
// Use [Config].Skip for dynamic skip logic or [Config].SkipPaths for
// path exclusions. SkipPaths uses exact path matching:
//
//	server.Use(static.New(static.Config{
//	    Root:      "./public",
//	    SkipPaths: []string{"/api", "/health"},
//	}))
package static
