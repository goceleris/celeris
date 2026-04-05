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

	// Code is the HTTP redirect status code. Must be 300-308.
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
	if cfg.Code < 300 || cfg.Code > 308 {
		panic(fmt.Sprintf("redirect: Code must be 300-308, got %d", cfg.Code))
	}
}
