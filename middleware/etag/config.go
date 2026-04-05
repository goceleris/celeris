package etag

import "github.com/goceleris/celeris"

// Config defines the ETag middleware configuration.
type Config struct {
	// Skip defines a function to skip this middleware for certain requests.
	Skip func(c *celeris.Context) bool

	// SkipPaths lists paths to skip (exact match).
	SkipPaths []string

	// Strong controls whether ETags use the strong format "xxxxxxxx".
	// When false (default), weak ETags are used: W/"xxxxxxxx".
	// Weak ETags are recommended for responses that may be content-negotiated
	// or transfer-encoded.
	Strong bool

	// HashFunc computes a custom ETag from the response body.
	// When set, the returned string is used as the opaque-tag (without quotes
	// or W/ prefix -- those are added automatically based on the Strong setting).
	// Default: CRC-32 IEEE hex.
	HashFunc func(body []byte) string
}

// defaultConfig is the default ETag configuration.
// It is unexported to prevent mutation; use New() with no arguments
// for the default behavior. Zero value: Strong=false produces weak ETags.
var defaultConfig = Config{}

// applyDefaults is intentionally empty -- Strong defaults to false (weak ETags)
// via zero value, and there are no zero-value fields to backfill.
// Kept for consistency with the middleware pattern.
func applyDefaults(cfg Config) Config {
	return cfg
}
