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

	// Root is the OS filesystem directory from which to serve files.
	// Either Root or Filesystem must be set.
	Root string

	// Filesystem is an alternative file source (e.g. embed.FS or
	// fstest.MapFS). Either Root or Filesystem must be set.
	Filesystem fs.FS

	// Index is the file to serve for directory requests.
	// Default: "index.html".
	Index string

	// SPA enables single-page application mode. When a requested file
	// does not exist, the Index file is served instead of passing to the
	// next handler.
	SPA bool

	// Browse enables directory listing. When true, requests to directories
	// that lack an index file receive a simple HTML listing of entries.
	Browse bool

	// MaxAge sets the Cache-Control max-age directive in seconds.
	// Zero (default) means no Cache-Control header is added.
	MaxAge int

	// Prefix is the URL prefix to strip before file lookup.
	// Must start with "/". Default: "/".
	Prefix string
}

func applyDefaults(cfg Config) Config {
	if cfg.Index == "" {
		cfg.Index = "index.html"
	}
	if cfg.Prefix == "" {
		cfg.Prefix = "/"
	}
	return cfg
}

func (cfg Config) validate() {
	if cfg.Root == "" && cfg.Filesystem == nil {
		panic("static: either Root or Filesystem must be set")
	}
	if cfg.Prefix != "" && cfg.Prefix[0] != '/' {
		panic("static: Prefix must start with /")
	}
}
