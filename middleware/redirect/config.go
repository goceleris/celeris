package redirect

import (
	"fmt"

	"github.com/goceleris/celeris"
)

// Config defines the redirect middleware configuration.
type Config struct {
	// Skip defines a function to skip this middleware for certain requests.
	Skip func(c *celeris.Context) bool

	// SkipPaths lists paths to skip (exact match).
	SkipPaths []string

	// Code is the HTTP redirect status code. Must be 301, 302, 303, 307, or 308.
	// Default: 301 (Moved Permanently).
	Code int
}

var defaultConfig = Config{
	Code: 301,
}

func applyDefaults(cfg Config) Config {
	if cfg.Code == 0 {
		cfg.Code = 301
	}
	return cfg
}

func (cfg Config) validate() {
	switch cfg.Code {
	case 301, 302, 303, 307, 308:
		// valid redirect codes
	default:
		panic(fmt.Sprintf("redirect: Code must be a redirect status (301, 302, 303, 307, 308), got %d", cfg.Code))
	}
}
