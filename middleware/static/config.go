package static

import (
	"io/fs"

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
	Root string

	// FS is an [fs.FS] from which to serve files (e.g. embed.FS).
	// Mutually exclusive with Root. If both Root and FS are set, FS takes
	// precedence.
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
}
