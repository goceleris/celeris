package singleflight

import (
	"slices"

	"github.com/goceleris/celeris"
)

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
	// Default: method + "\x00" + path + "\x00" + sorted-query-string
	// + "\x00" + Authorization header + "\x00" + Cookie header. The
	// Authorization and Cookie components ensure that requests from
	// different authenticated users produce different keys, preventing
	// cross-user data leakage. Unauthenticated requests (no auth/cookie
	// headers) still coalesce normally.
	//
	// If you provide a custom KeyFunc, ensure it incorporates user
	// identity for any endpoint that returns user-specific data.
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
	auth := c.Header("authorization")
	cookie := c.Header("cookie")

	var key string
	if rq == "" {
		key = m + "\x00" + p
	} else {
		sorted := c.QueryParams()
		for _, vals := range sorted {
			slices.Sort(vals)
		}
		key = m + "\x00" + p + "\x00" + sorted.Encode()
	}
	if auth != "" {
		key += "\x00" + auth
	}
	if cookie != "" {
		key += "\x00" + cookie
	}
	return key
}
