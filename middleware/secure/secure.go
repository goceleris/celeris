package secure

import "github.com/goceleris/celeris"

// New creates a security headers middleware with the given config.
// All non-HSTS header values are pre-computed at initialization for
// zero-allocation responses on the hot path. HSTS is only sent over
// HTTPS connections (Scheme() checks X-Forwarded-Proto internally).
//
// validate() panics at initialization for invalid configurations
// (e.g., HSTSPreload with insufficient MaxAge). This is intentional:
// misconfigurations are caught at startup, not at request time.
func New(config ...Config) celeris.HandlerFunc {
	cfg := defaultConfig
	if len(config) > 0 {
		cfg = config[0]
	}
	cfg = applyDefaults(cfg)
	cfg.validate()

	skipMap := make(map[string]struct{}, len(cfg.SkipPaths))
	for _, p := range cfg.SkipPaths {
		skipMap[p] = struct{}{}
	}

	headers := buildHeaders(cfg)
	hstsValue := buildHSTSValue(cfg)
	skip := cfg.Skip

	return func(c *celeris.Context) error {
		if skip != nil && skip(c) {
			return c.Next()
		}

		if _, ok := skipMap[c.Path()]; ok {
			return c.Next()
		}

		for _, h := range headers {
			c.SetHeader(h[0], h[1])
		}

		if hstsValue != "" && c.Scheme() == "https" {
			c.SetHeader("strict-transport-security", hstsValue)
		}

		return c.Next()
	}
}
