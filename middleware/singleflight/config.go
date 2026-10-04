package singleflight

import (
	"net/url"
	"slices"
	"strings"

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
	// Default: method + "\x00" + path + "\x00" + sorted-query-string,
	// then the Authorization, Cookie and Accept-Encoding headers, each
	// labelled so that one can never read as another. The Authorization
	// and Cookie components ensure that requests from different
	// authenticated users produce different keys, preventing cross-user
	// data leakage. Unauthenticated requests (no auth/cookie headers)
	// still coalesce normally. Accept-Encoding keeps requests that accept
	// different encodings apart when compress runs inside singleflight.
	//
	// If you provide a custom KeyFunc, ensure it incorporates user
	// identity for any endpoint that returns user-specific data. Whatever
	// the key, a waiter whose request differs from the leader's in a
	// header the leader's response names in Vary (or a response with
	// Vary: *) runs its own handler instead of taking the leader's
	// response.
	KeyFunc func(c *celeris.Context) string
}

var defaultConfig = Config{}

func applyDefaults(cfg Config) Config {
	if cfg.KeyFunc == nil {
		cfg.KeyFunc = defaultKeyFunc
	}
	return cfg
}

// No validation needed: all Config fields have safe zero values.
func (cfg Config) validate() {}

func defaultKeyFunc(c *celeris.Context) string {
	m := c.Method()
	p := c.Path()
	rq := c.RawQuery()
	auth := c.Header("authorization")
	cookie := c.Header("cookie")
	// The encoding compress negotiates comes from Accept-Encoding, so
	// requests that accept different encodings get different responses
	// (celeris#912).
	ae := c.Header("accept-encoding")

	var q string
	if rq != "" {
		// Clone query params before sorting to avoid mutating the
		// context's cached queryCache.
		params := c.QueryParams()
		sorted := make(url.Values, len(params))
		for k, v := range params {
			cp := make([]string, len(v))
			copy(cp, v)
			slices.Sort(cp)
			sorted[k] = cp
		}
		q = sorted.Encode()
	}
	if q == "" && auth == "" && cookie == "" && ae == "" {
		return m + "\x00" + p
	}
	// Each header component is labelled, so one header's value can never
	// read as another's (a header value cannot hold a NUL). The key is
	// built in one allocation.
	n := len(m) + 1 + len(p)
	if q != "" {
		n += 1 + len(q)
	}
	for _, v := range [...]string{auth, cookie, ae} {
		if v != "" {
			n += 3 + len(v)
		}
	}
	var b strings.Builder
	b.Grow(n)
	b.WriteString(m)
	b.WriteString("\x00")
	b.WriteString(p)
	if q != "" {
		b.WriteString("\x00")
		b.WriteString(q)
	}
	if auth != "" {
		b.WriteString("\x00a=")
		b.WriteString(auth)
	}
	if cookie != "" {
		b.WriteString("\x00c=")
		b.WriteString(cookie)
	}
	if ae != "" {
		b.WriteString("\x00e=")
		b.WriteString(ae)
	}
	return b.String()
}
