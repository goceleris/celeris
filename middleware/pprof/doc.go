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
// # A profile ties up a worker
//
// The profile and trace endpoints record for "?seconds=N" (30 by default) and
// answer when they are done, and the handler runs on the I/O worker that
// reads the connection. On the native engines (epoll, io_uring) that worker
// serves no other connection for those N seconds: every request that lands on
// it waits. "seconds" has no upper limit, so one request can hold a worker for
// as long as it asks. (The Std engine runs a goroutine per connection and is
// not held.) Do not point a long profile at a server that is carrying traffic
// on every worker, and keep [Config].AuthFunc tight.
//
// To keep the workers free, mount the middleware as an async route, whose
// handler runs on a goroutine of its own (see [celeris.Route.Async]), instead
// of with Use:
//
//	s.GET(prefix+"/*p", pprof.New()).Async() // prefix is Config.Prefix, "/debug/pprof" by default
//
// With a two-worker server on Linux (epoll, io_uring and adaptive alike), a 2 s
// CPU profile started through Use made the slowest of eight plain requests
// wait about 1.5 s, and through the Async route 0.3 to 1 ms. A limit on "seconds" would be new API and is not
// part of this middleware yet (celeris#888 item 83).
//
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs/observability
package pprof
