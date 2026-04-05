package bodylimit

import "github.com/goceleris/celeris"

// ErrBodyTooLarge is returned when the request body exceeds the configured limit.
var ErrBodyTooLarge = celeris.NewHTTPError(413, "Request Entity Too Large")

// ErrLengthRequired is returned when ContentLengthRequired is enabled and the
// request omits a Content-Length header.
var ErrLengthRequired = celeris.NewHTTPError(411, "Length Required")

// New creates a body limit middleware with the given config.
//
// Architectural note: celeris buffers the full request body before middleware
// executes, so this middleware cannot prevent oversized payloads from entering
// memory when Content-Length is absent or dishonest. The framework's own
// maxRequestBodySize (100 MB) provides the hard ceiling. This middleware adds
// an application-level limit that rejects already-buffered bodies that exceed
// the configured threshold.
//
// For strict enforcement against clients that omit Content-Length, enable
// [Config].ContentLengthRequired to reject such requests with 411 Length
// Required before the body check runs.
func New(config ...Config) celeris.HandlerFunc {
	cfg := defaultConfig
	if len(config) > 0 {
		cfg = config[0]
	}
	cfg = applyDefaults(cfg)
	validate(&cfg)

	maxBytes := cfg.MaxBytes
	errHandler := cfg.ErrorHandler
	requireCL := cfg.ContentLengthRequired

	skipMap := make(map[string]struct{}, len(cfg.SkipPaths))
	for _, p := range cfg.SkipPaths {
		skipMap[p] = struct{}{}
	}

	return func(c *celeris.Context) error {
		if cfg.Skip != nil && cfg.Skip(c) {
			return c.Next()
		}

		if _, ok := skipMap[c.Path()]; ok {
			return c.Next()
		}

		// Auto-skip bodyless methods (RFC 7231 §4.3, RFC 7231 §4.3.6,
		// RFC 7231 §4.3.8). TRACE must not have a body; CONNECT
		// transitions to tunnel mode.
		switch c.Method() {
		case "GET", "HEAD", "DELETE", "OPTIONS", "TRACE", "CONNECT":
			return c.Next()
		}

		cl := c.ContentLength()

		// Reject requests without Content-Length when required.
		if requireCL && cl < 0 {
			if errHandler != nil {
				return errHandler(c, ErrLengthRequired)
			}
			return ErrLengthRequired
		}

		// Phase 1: Content-Length pre-check (fast path).
		if cl > maxBytes {
			if errHandler != nil {
				return errHandler(c, ErrBodyTooLarge)
			}
			return ErrBodyTooLarge
		}

		// Phase 2: Actual body byte count (catches lying/absent Content-Length).
		if int64(len(c.Body())) > maxBytes {
			if errHandler != nil {
				return errHandler(c, ErrBodyTooLarge)
			}
			return ErrBodyTooLarge
		}

		return c.Next()
	}
}
