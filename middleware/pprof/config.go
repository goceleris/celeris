package pprof

import (
	"net"

	"github.com/goceleris/celeris"
)

// Config defines the pprof middleware configuration.
type Config struct {
	// Skip defines a function to skip this middleware for certain requests.
	Skip func(c *celeris.Context) bool

	// SkipPaths lists full paths to bypass entirely (exact match).
	// Unlike Skip (which is only consulted for requests matching the prefix),
	// SkipPaths is checked before any other logic.
	SkipPaths []string

	// Prefix is the URL path prefix for pprof endpoints.
	// Default: "/debug/pprof".
	Prefix string

	// AuthFunc is an authentication check executed before any pprof endpoint.
	// If it returns false, the middleware responds with 403 Forbidden.
	// Default: allows only loopback IPs (127.0.0.1 and ::1).
	// Set to func(*celeris.Context) bool { return true } to allow public access.
	AuthFunc func(c *celeris.Context) bool
}

var defaultConfig = Config{
	Prefix: "/debug/pprof",
	AuthFunc: func(c *celeris.Context) bool {
		rawAddr := c.RemoteAddr()
		host, _, err := net.SplitHostPort(rawAddr)
		if err != nil {
			// RemoteAddr may lack a port (e.g. Unix socket or test stub).
			host = rawAddr
		}
		if ip := net.ParseIP(host); ip != nil {
			return ip.IsLoopback()
		}
		return false
	},
}

func applyDefaults(cfg Config) Config {
	if cfg.Prefix == "" {
		cfg.Prefix = "/debug/pprof"
	}
	if cfg.AuthFunc == nil {
		cfg.AuthFunc = defaultConfig.AuthFunc
	}
	return cfg
}

func (cfg Config) validate() {
	if cfg.Prefix != "" && cfg.Prefix[0] != '/' {
		panic("pprof: Prefix must start with /")
	}
}
