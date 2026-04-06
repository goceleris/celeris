// Package pprof provides a middleware that exposes Go runtime profiling data
// via the standard net/http/pprof handlers.
//
// The middleware intercepts requests under a configurable prefix
// (default "/debug/pprof") and dispatches to the matching pprof handler.
// Non-matching requests pass through to the next handler with zero overhead.
//
// # Security
//
// By default, access is restricted to loopback addresses (127.0.0.1, ::1)
// via [Config].AuthFunc. This uses the raw TCP peer address, NOT
// X-Forwarded-For. Behind a reverse proxy, set AuthFunc to a scheme that
// does not rely on RemoteAddr (e.g., shared secret header).
//
// Profiling endpoints expose sensitive runtime internals. Never expose them
// publicly in production without proper authentication.
//
// # Endpoints
//
// All endpoints are relative to the configured prefix:
//
//	{prefix}/           — index page listing available profiles
//	{prefix}/cmdline    — command-line arguments
//	{prefix}/profile    — CPU profile (accepts ?seconds=N)
//	{prefix}/symbol     — symbol lookup
//	{prefix}/trace      — execution trace (accepts ?seconds=N)
//	{prefix}/allocs     — allocation profile
//	{prefix}/block      — block profile
//	{prefix}/goroutine  — goroutine stacks
//	{prefix}/heap       — heap profile
//	{prefix}/mutex      — mutex contention profile
//	{prefix}/threadcreate — thread creation profile
//
// # Ordering
//
// Place pprof after the debug middleware in the middleware chain. Since pprof
// intercepts by path prefix, it can be installed at any position, but placing
// it after debug avoids shadowing debug endpoints when both share the
// /debug/ prefix namespace.
//
// # Basic usage
//
//	server.Use(pprof.New())
//
// # Custom AuthFunc
//
//	server.Use(pprof.New(pprof.Config{
//	    AuthFunc: func(c *celeris.Context) bool {
//	        return c.Header("x-pprof-token") == os.Getenv("PPROF_TOKEN")
//	    },
//	}))
package pprof
