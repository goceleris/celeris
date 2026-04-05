package debug

import (
	"net"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/observe"
)

// Config defines the debug middleware configuration.
type Config struct {
	// Skip defines a function to skip this middleware for certain requests.
	Skip func(c *celeris.Context) bool

	// Prefix is the URL path prefix for debug endpoints.
	// Default: "/debug/celeris".
	Prefix string

	// AuthFunc is an authentication check executed before any debug endpoint.
	// If it returns false, the middleware responds with 403 Forbidden.
	// Default: allows only loopback IPs (127.0.0.1 and ::1).
	// Set to func(*celeris.Context) bool { return true } to allow public access.
	AuthFunc func(c *celeris.Context) bool

	// Server provides access to registered routes via Server.Routes().
	// Nil is safe; the /routes endpoint returns an empty list.
	Server *celeris.Server

	// Collector provides metrics via Collector.Snapshot().
	// Nil is safe; the /metrics endpoint returns 501 Not Implemented.
	Collector *observe.Collector

	// SkipPaths is a list of URL paths for which the debug middleware is
	// bypassed entirely. Unlike Skip (which is only consulted for debug
	// prefixed requests), SkipPaths is checked before any other logic.
	SkipPaths []string

	// Endpoints selectively enables or disables individual debug endpoints.
	// Keys are endpoint names: "status", "metrics", "config", "routes",
	// "memory", "build", "runtime". A true value enables the endpoint;
	// false disables it. When nil (default), all endpoints are enabled.
	Endpoints map[string]bool

	// MemStatsTTL controls how long the /memory endpoint caches
	// runtime.ReadMemStats results. ReadMemStats is expensive (it STWs the
	// runtime), so caching avoids hammering under load.
	//
	// Default: 1s. Set to 0 to disable caching (not recommended).
	// Negative values are treated as "not set" and fall back to the default.
	MemStatsTTL time.Duration
}

// defaultConfigCopy returns the default debug middleware configuration.
// Each call returns a fresh copy.
func defaultConfigCopy() Config {
	return defaultConfig
}

var defaultConfig = Config{
	Prefix: "/debug/celeris",
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
		cfg.Prefix = "/debug/celeris"
	}
	if cfg.AuthFunc == nil {
		cfg.AuthFunc = defaultConfig.AuthFunc
	}
	if cfg.MemStatsTTL <= 0 {
		cfg.MemStatsTTL = time.Second
	}
	// Floor: ReadMemStats triggers a stop-the-world pause; allowing sub-100ms
	// TTLs under load would cause back-to-back STW storms.
	if cfg.MemStatsTTL > 0 && cfg.MemStatsTTL < 100*time.Millisecond {
		cfg.MemStatsTTL = 100 * time.Millisecond
	}
	return cfg
}

func (cfg Config) validate() {
	if cfg.Prefix != "" && cfg.Prefix[0] != '/' {
		panic("debug: Prefix must start with /")
	}
}
