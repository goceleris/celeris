package requestid

import (
	"context"

	"github.com/goceleris/celeris"
)

// stdContextKey is an unexported type used as the key for storing the
// request ID in a stdlib context.Context value.
type stdContextKey struct{}

// FromStdContext returns the request ID from a stdlib [context.Context].
// Returns an empty string if no request ID was stored.
func FromStdContext(ctx context.Context) string {
	v, ok := ctx.Value(stdContextKey{}).(string)
	if !ok {
		return ""
	}
	return v
}

// ContextKey is the context store key for the request ID.
const ContextKey = "request_id"

const maxIDLen = 128

// FromContext returns the request ID from the context store.
// Returns an empty string if no request ID was stored.
func FromContext(c *celeris.Context) string {
	v, ok := c.Get(ContextKey)
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

func validID(id string) bool {
	if len(id) == 0 || len(id) > maxIDLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] < 0x20 || id[i] > 0x7E {
			return false
		}
	}
	return true
}

// maxGeneratorRetries is the number of times a custom generator is retried
// if it returns an empty string, before falling back to the built-in UUID.
const maxGeneratorRetries = 3

// New creates a request ID middleware with the given config.
func New(config ...Config) celeris.HandlerFunc {
	cfg := defaultConfig
	if len(config) > 0 {
		cfg = config[0]
	}
	isCustomGen := cfg.Generator != nil
	cfg = applyDefaults(cfg)

	header := cfg.Header
	gen := cfg.Generator
	trustProxy := !cfg.DisableTrustProxy
	skip := cfg.Skip

	fallbackGen := defaultGenerator.UUID

	skipMap := make(map[string]struct{}, len(cfg.SkipPaths))
	for _, p := range cfg.SkipPaths {
		skipMap[p] = struct{}{}
	}

	return func(c *celeris.Context) error {
		if skip != nil && skip(c) {
			return c.Next()
		}

		if _, ok := skipMap[c.Path()]; ok {
			return c.Next()
		}

		var id string
		if trustProxy {
			id = c.Header(header)
			if !validID(id) {
				id = ""
			}
		}
		if id == "" {
			for range maxGeneratorRetries {
				id = gen()
				if validID(id) {
					break
				}
				id = ""
				if !isCustomGen {
					break
				}
			}
			if id == "" {
				id = fallbackGen()
			}
		}

		c.SetHeader(header, id)
		c.Set(ContextKey, id)
		c.SetContext(context.WithValue(c.Context(), stdContextKey{}, id))

		return c.Next()
	}
}
