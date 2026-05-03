package secure

import (
	"fmt"

	"github.com/goceleris/celeris"
)

// validateHeaderValue panics if v contains a CR, LF, or NUL byte.
// Header values reach the wire via [Context.AppendRespHeader] which
// skips per-request sanitization for performance — invariant must
// hold at construction time. Failing fast at New() catches a
// misconfiguration that would otherwise cause silent header injection
// or a malformed response under high load.
func validateHeaderValue(name, v string) {
	for i := 0; i < len(v); i++ {
		if b := v[i]; b == '\r' || b == '\n' || b == 0 {
			panic(fmt.Sprintf("secure: header %q value contains CR/LF/NUL at byte %d (header injection); fix the config", name, i))
		}
	}
}

// New creates a security headers middleware with the given config.
// All non-HSTS header values are pre-computed at initialization for
// zero-allocation responses on the hot path. HSTS is only sent over
// HTTPS connections (determined by Context.Scheme).
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

	var skip celeris.SkipHelper
	skip.Init(cfg.SkipPaths, cfg.Skip)

	headers := buildHeaders(cfg)
	hstsValue := buildHSTSValue(cfg)

	// Validate values once at construction time so the per-request hot
	// path can use [Context.AppendRespHeader] (no sanitization scan, no
	// dedup walk). Keys are hardcoded lowercase ASCII in buildHeaders.
	for _, h := range headers {
		validateHeaderValue(h[0], h[1])
	}
	if hstsValue != "" {
		validateHeaderValue("strict-transport-security", hstsValue)
	}

	return func(c *celeris.Context) error {
		if skip.ShouldSkip(c) {
			return c.Next()
		}

		for _, h := range headers {
			c.AppendRespHeader(h[0], h[1])
		}

		if hstsValue != "" && c.Scheme() == "https" {
			c.AppendRespHeader("strict-transport-security", hstsValue)
		}

		return c.Next()
	}
}
