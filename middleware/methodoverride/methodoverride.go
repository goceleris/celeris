package methodoverride

import (
	"strings"

	"github.com/goceleris/celeris"
)

// New creates a method override middleware with the given config.
// The middleware reads an overridden HTTP method from a configurable source
// (header, form field, or custom getter) and rewrites the request method
// before routing. Only POST requests are rewritten by default.
func New(config ...Config) celeris.HandlerFunc {
	cfg := defaultConfig
	if len(config) > 0 {
		cfg = config[0]
	}
	cfg = applyDefaults(cfg)
	validate(cfg)

	allowed := make(map[string]struct{}, len(cfg.AllowedMethods))
	for _, m := range cfg.AllowedMethods {
		allowed[strings.ToUpper(m)] = struct{}{}
	}
	getter := cfg.Getter

	var skip celeris.SkipHelper
	skip.Init(cfg.SkipPaths, cfg.Skip)

	return func(c *celeris.Context) error {
		if skip.ShouldSkip(c) {
			return c.Next()
		}

		original := c.Method()
		if _, ok := allowed[original]; !ok {
			return c.Next()
		}

		override := getter(c)
		if override == "" {
			return c.Next()
		}

		upper := strings.ToUpper(override)
		if upper != original {
			c.SetMethod(upper)
		}

		return c.Next()
	}
}
