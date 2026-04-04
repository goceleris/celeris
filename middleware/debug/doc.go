// Package debug provides a debug/introspection middleware for celeris.
//
// The middleware intercepts requests under a configurable prefix
// (default "/debug/celeris") and serves JSON endpoints for server
// introspection. Non-matching requests pass through with zero overhead.
//
// Basic usage:
//
//	server.Use(debug.New(debug.Config{
//	    Server:    server,
//	    Collector: collector,
//	}))
//
// Endpoints: {prefix}/ (index), /status, /metrics, /config, /routes,
// /memory, /build, /runtime.
//
// Use [Config].Endpoints to selectively enable/disable endpoints (nil =
// all enabled). Disabled endpoints return 404; the index reflects only
// enabled ones.
//
// By default, access is restricted to loopback addresses (127.0.0.1,
// ::1) via [Config].AuthFunc. This uses the raw TCP peer address, NOT
// X-Forwarded-For. Behind a reverse proxy, set AuthFunc to a scheme
// that does not rely on RemoteAddr (e.g., shared secret header).
//
// [Config].MemStatsTTL controls /memory caching (default 1s, floor
// 100ms) to avoid repeated stop-the-world pauses from ReadMemStats.
//
// Server and Collector are optional; when nil, /routes returns [] and
// /metrics returns 501.
package debug
