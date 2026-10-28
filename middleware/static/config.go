package static

import (
	"io/fs"
	"time"

	"github.com/goceleris/celeris"
)

// Config defines the static file middleware configuration.
type Config struct {
	// Skip defines a function to skip this middleware for certain requests.
	Skip func(c *celeris.Context) bool

	// SkipPaths lists paths to skip (exact match).
	SkipPaths []string

	// Root is the directory path on the OS filesystem from which to serve
	// files. Mutually exclusive with FS. If neither Root nor FS is set,
	// the middleware panics.
	//
	// Everything below Root is served, dotfiles and dot directories included
	// (.env, .git/config): the package has no rule against them today (a
	// default deny for them is being decided). A path that leaves Root, by
	// "..", an absolute path, or a symlink that resolves outside it, is
	// refused. A relative Root is read against the working directory at each
	// request; a Root made of ".." segments ("..", "../..") means the directory
	// it resolves to, and nothing outside that directory is served.
	//
	// Root may be ".", "./" or "/", and serves the tree below it as any other
	// directory does. Mind what that is: "." is the process's working
	// directory, with its sources and configuration (.env, .git), and "/" is
	// the whole filesystem (/etc/passwd, the home directories,
	// /proc/self/environ, the process's environment: whatever the process may
	// read), where nothing is outside Root any more, so the containment check
	// that protects the others protects nothing. When the working directory
	// is "/" (the default of a systemd service and of a container without a
	// WORKDIR), "." is the whole filesystem too. Before v1.6.0 these three
	// served nothing below the root, so a configuration that was inert since
	// v1.3.3 starts to serve files after the upgrade: name a directory that
	// holds only what is meant to be public.
	Root string

	// FS is an [fs.FS] from which to serve files (e.g. embed.FS).
	// Mutually exclusive with Root. If both Root and FS are set, FS takes
	// precedence. The middleware does not check where an FS leads: an
	// [os.DirFS] follows a symlink out of its directory, which Root does not
	// (see its containment check); use Root for a directory that may hold
	// untrusted symlinks.
	FS fs.FS

	// Prefix is the URL path prefix to strip before looking up the file.
	// For example, Prefix: "/static" serves /static/style.css from style.css
	// in Root/FS. The prefix is matched at segment boundaries: /static does
	// not match /static-docs.
	// Default: "/" (serve from the URL root).
	Prefix string

	// Index is the default file to serve when the request path resolves to
	// a directory.
	// Default: "index.html".
	Index string

	// Browse enables directory listing when the request path resolves to
	// a directory without an index file.
	// Default: false.
	Browse bool

	// SPA enables single-page application mode. When true, requests for
	// non-existent files serve the index file instead of falling through
	// to the next handler. Useful for single-page applications.
	// Default: false.
	SPA bool

	// MaxAge sets the Cache-Control max-age directive. Zero means no
	// Cache-Control header.
	MaxAge time.Duration

	// Compress enables serving pre-compressed files. When true, the middleware
	// checks for .br (Brotli) and .gz (gzip) variants of the requested file
	// and serves them if the client accepts the encoding via Accept-Encoding,
	// read with its q-values: an encoding the client refuses with q=0, by name
	// or through a "*;q=0" that does not list it ("*" matches only the
	// encodings the field does not list), is not served. Brotli is preferred over
	// gzip when both are accepted (x-gzip counts as gzip). A file served in
	// answer to a request that carries an Accept-Encoding header names
	// Accept-Encoding in Vary (once), whether or not the file has a variant.
	// Works with both OS filesystem (Root) and fs.FS.
	Compress bool
}

var defaultConfig = Config{
	Prefix: "/",
	Index:  "index.html",
}

func applyDefaults(cfg Config) Config {
	if cfg.Prefix == "" {
		cfg.Prefix = defaultConfig.Prefix
	}
	if cfg.Index == "" {
		cfg.Index = defaultConfig.Index
	}
	return cfg
}

func (cfg Config) validate() {
	if cfg.Root == "" && cfg.FS == nil {
		panic("static: Root or FS must be set")
	}
	if cfg.Prefix != "" && cfg.Prefix[0] != '/' {
		panic("static: Prefix must start with /")
	}
}
