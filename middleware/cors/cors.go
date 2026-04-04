package cors

import "github.com/goceleris/celeris"

// New creates a CORS middleware with the given config.
func New(config ...Config) celeris.HandlerFunc {
	cfg := defaultConfig
	if len(config) > 0 {
		cfg = config[0]
	}
	cfg = applyDefaults(cfg)
	cfg.validate()
	p := precompute(cfg)

	skipMap := make(map[string]struct{}, len(cfg.SkipPaths))
	for _, path := range cfg.SkipPaths {
		skipMap[path] = struct{}{}
	}

	return func(c *celeris.Context) error {
		if cfg.Skip != nil && cfg.Skip(c) {
			return c.Next()
		}

		if _, ok := skipMap[c.Path()]; ok {
			return c.Next()
		}

		origin := c.Header("origin")

		// No Origin header — not a CORS request. Add Vary: Origin when
		// specific origins are configured so intermediate caches do not
		// serve a cached response (without CORS headers) to a later
		// cross-origin request, or vice versa.
		if origin == "" {
			if !p.allowAllOrigins {
				c.AddHeader("vary", "Origin")
			}
			return c.Next()
		}

		// Reject origins exceeding the maximum length to prevent CPU waste.
		if len(origin) > maxOriginLength {
			return c.Next()
		}

		// Handle the special "null" origin (sandboxed iframes, file:// URLs).
		if origin == "null" {
			if !p.nullAllowed && !p.allowAllOrigins {
				return c.Next()
			}
			// Fall through — null is in the allowed set or allowAll is true.
		}

		if !p.isOriginAllowed(c, origin) {
			if !p.allowAllOrigins {
				c.AddHeader("vary", "Origin")
			}
			return c.Next()
		}

		isPreflight := c.Method() == "OPTIONS" && c.Header("access-control-request-method") != ""

		// Set origin header.
		if p.allowAllOrigins {
			c.SetHeader("access-control-allow-origin", "*")
		} else {
			c.SetHeader("access-control-allow-origin", origin)
			// For preflight, the preflightVary string already includes Origin.
			// Only add standalone Vary: Origin on non-preflight responses.
			if !isPreflight {
				c.AddHeader("vary", "Origin")
			}
		}

		if cfg.AllowCredentials {
			c.SetHeader("access-control-allow-credentials", "true")
		}

		if p.exposeHeaders != "" {
			c.SetHeader("access-control-expose-headers", p.exposeHeaders)
		}

		// Preflight: OPTIONS with Access-Control-Request-Method header.
		if isPreflight {
			c.SetHeader("access-control-allow-methods", p.allowMethods)
			if p.mirrorRequestHeaders {
				if reqHeaders := c.Header("access-control-request-headers"); reqHeaders != "" {
					c.SetHeader("access-control-allow-headers", reqHeaders)
				}
			} else {
				c.SetHeader("access-control-allow-headers", p.allowHeaders)
			}
			if p.maxAge != "" {
				c.SetHeader("access-control-max-age", p.maxAge)
			}
			c.AddHeader("vary", p.preflightVary)
			if p.allowPrivateNetwork && c.Header("access-control-request-private-network") == "true" {
				c.SetHeader("access-control-allow-private-network", "true")
			}
			return c.NoContent(204)
		}

		return c.Next()
	}
}
