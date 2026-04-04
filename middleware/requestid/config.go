package requestid

import "github.com/goceleris/celeris"

// Config defines the request ID middleware configuration.
type Config struct {
	// Skip defines a function to skip this middleware for certain requests.
	Skip func(c *celeris.Context) bool

	// SkipPaths lists paths to skip (exact match).
	SkipPaths []string

	// Generator returns a new request ID. Default: buffered UUID v4.
	Generator func() string

	// Header is the header name to read/write. Default: "x-request-id".
	Header string

	// DisableTrustProxy, when true, ignores inbound request ID headers
	// and always generates a fresh ID. This prevents request ID spoofing
	// from untrusted clients. Default: false (inbound headers are trusted).
	DisableTrustProxy bool
}

// defaultConfig is the default request ID configuration.
// It is unexported to prevent mutation; use New() with no arguments
// for the default behavior.
var defaultConfig = Config{
	Header: "x-request-id",
}

func applyDefaults(cfg Config) Config {
	if cfg.Generator == nil {
		cfg.Generator = defaultGenerator.UUID
	}
	if cfg.Header == "" {
		cfg.Header = "x-request-id"
	}
	return cfg
}

var defaultGenerator = newBufferedGenerator()
