package swagger

import (
	"io/fs"

	"github.com/goceleris/celeris"
)

// Config defines the swagger middleware configuration.
type Config struct {
	// Skip defines a function to skip this middleware for certain requests.
	Skip func(c *celeris.Context) bool

	// SkipPaths lists paths to skip (exact match).
	SkipPaths []string

	// SpecURL is the URL path that serves the raw OpenAPI spec.
	// Default: "/swagger/doc.json".
	SpecURL string

	// UIPath is the URL path prefix for the Swagger UI page.
	// Default: "/swagger".
	UIPath string

	// SpecContent is the raw OpenAPI specification bytes (JSON or YAML).
	// Either SpecContent or Filesystem must be set.
	SpecContent []byte

	// Filesystem provides an alternative source for the OpenAPI spec.
	// When set, the spec is read from Filesystem at SpecFile path.
	Filesystem fs.FS

	// SpecFile is the path within Filesystem to read the spec from.
	// Default: "openapi.json".
	SpecFile string

	// UIEngine selects the documentation UI.
	// Supported values: "swagger-ui" (default) or "scalar".
	UIEngine string

	// AuthFunc is an optional authentication check executed before
	// serving the UI or spec. If it returns false, the middleware
	// responds with 403 Forbidden.
	AuthFunc func(c *celeris.Context) bool
}

var defaultConfig = Config{
	SpecURL:  "/swagger/doc.json",
	UIPath:   "/swagger",
	SpecFile: "openapi.json",
	UIEngine: "swagger-ui",
}

func applyDefaults(cfg Config) Config {
	if cfg.SpecURL == "" {
		cfg.SpecURL = defaultConfig.SpecURL
	}
	if cfg.UIPath == "" {
		cfg.UIPath = defaultConfig.UIPath
	}
	if cfg.SpecFile == "" {
		cfg.SpecFile = defaultConfig.SpecFile
	}
	if cfg.UIEngine == "" {
		cfg.UIEngine = defaultConfig.UIEngine
	}
	return cfg
}

func (cfg Config) validate() {
	if cfg.SpecContent == nil && cfg.Filesystem == nil {
		panic("swagger: either SpecContent or Filesystem must be set")
	}
	if cfg.UIPath != "" && cfg.UIPath[0] != '/' {
		panic("swagger: UIPath must start with /")
	}
	if cfg.SpecURL != "" && cfg.SpecURL[0] != '/' {
		panic("swagger: SpecURL must start with /")
	}
}
