// Package pprof provides a middleware that exposes Go runtime profiling data
// via the standard net/http/pprof handlers.
//
// The middleware intercepts requests under a configurable prefix
// (default "/debug/pprof") and dispatches to the matching pprof handler.
// Non-matching requests pass through to the next handler with zero overhead.
//
// Use [New] with an optional [Config] to mount the profiling endpoints:
//
//	server.Use(pprof.New())
//
// [Config].Prefix controls the URL prefix (default "/debug/pprof").
// [Config].AuthFunc gates access; the default restricts to loopback addresses
// (127.0.0.1 and ::1) using the raw TCP peer address — set a custom AuthFunc
// when running behind a reverse proxy. [Config].Skip and [Config].SkipPaths
// provide request-level bypass logic.
//
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs/observability
package pprof
