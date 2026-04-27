package requestid

import (
	"context"
	"fmt"
	"strings"

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
//
// Uses [celeris.Context.GetString] so the lookup covers all three
// sources: the dedicated requestID field set by
// [celeris.Context.SetRequestID] (preferred, zero-alloc), the stringKeys
// map from [celeris.Context.SetString], and the any-typed c.keys map
// from user code that called c.Set(ContextKey, id) directly.
func FromContext(c *celeris.Context) string {
	s, _ := c.GetString(ContextKey)
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

	// Pre-lowercase the header name so c.Header's fast-path fires
	// without allocating per request, even if a caller overrides the
	// default with a mixed-case value like "X-Request-Id".
	header := strings.ToLower(cfg.Header)

	// Validate the header name once at construction time so the per-
	// request hot path can call [celeris.Context.SetHeaderTrust] (no
	// sanitization scan). Lowercase ASCII without CR/LF/NUL is the
	// SetHeaderTrust contract.
	for i := 0; i < len(header); i++ {
		if b := header[i]; b == '\r' || b == '\n' || b == 0 {
			panic(fmt.Sprintf("requestid: Header config %q contains CR/LF/NUL at byte %d (header injection); fix the config", cfg.Header, i))
		}
	}
	gen := cfg.Generator
	trustProxy := !cfg.DisableTrustProxy
	enableStdCtx := cfg.EnableStdContext

	fallbackGen := defaultGenerator.UUID

	var skip celeris.SkipHelper
	skip.Init(cfg.SkipPaths, cfg.Skip)

	return func(c *celeris.Context) error {
		if skip.ShouldSkip(c) {
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
			if isCustomGen {
				// Custom generators may misbehave; validate output and
				// retry. After maxGeneratorRetries the fallback UUID
				// guarantees the request still gets an ID.
				for range maxGeneratorRetries {
					id = gen()
					if validID(id) {
						break
					}
					id = ""
				}
				if id == "" {
					id = fallbackGen()
				}
			} else {
				// Built-in UUID generator: output is always a 36-byte
				// hex+hyphen string in [0x20, 0x7E]. validID is a no-op
				// here and retries can never succeed differently — skip
				// both on the common path.
				id = gen()
			}
		}

		// SetHeaderTrust is safe here: header was validated lowercase /
		// no CR-LF-NUL at construction time, and id is either a
		// validID-checked incoming header value or a built-in UUID
		// (both already restricted to printable ASCII).
		c.SetHeaderTrust(header, id)
		// SetRequestID stores into a dedicated string field, skipping the
		// any-interface boxing that c.Set(ContextKey, id) would pay on
		// every request. Value still surfaces via Get(celeris.RequestIDKey)
		// for backward compatibility (re-boxes per call).
		c.SetRequestID(id)
		if enableStdCtx {
			c.SetContext(context.WithValue(c.Context(), stdContextKey{}, id))
		}

		return c.Next()
	}
}
