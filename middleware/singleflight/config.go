package singleflight

import "github.com/goceleris/celeris"

// Config defines the singleflight middleware configuration.
type Config struct {
	// Skip defines a function to skip this middleware for certain requests.
	Skip func(c *celeris.Context) bool

	// SkipPaths lists paths to skip (exact match).
	SkipPaths []string

	// KeyFunc extracts the deduplication key from the request. Requests
	// with the same key that arrive while a leader request is in-flight
	// are coalesced — waiters receive a copy of the leader's response.
	//
	// Default: method + "\x00" + path + "\x00" + sorted-query-string.
	KeyFunc func(c *celeris.Context) string
}

var defaultConfig = Config{}

func applyDefaults(cfg Config) Config {
	if cfg.KeyFunc == nil {
		cfg.KeyFunc = defaultKeyFunc
	}
	return cfg
}

func validate(Config) {}

func defaultKeyFunc(c *celeris.Context) string {
	m := c.Method()
	p := c.Path()
	rq := c.RawQuery()
	if rq == "" {
		return m + "\x00" + p
	}
	sorted := c.QueryParams().Encode()
	return m + "\x00" + p + "\x00" + sorted
}
