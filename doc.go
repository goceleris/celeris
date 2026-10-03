// Package celeris is a high-performance HTTP server with dual-architecture
// I/O (io_uring + epoll) and a high-level API for routing and request handling.
//
// Create a server with [New], register routes with the verb methods
// ([Server.GET], [Server.POST], …), and serve with [Server.Start] (or
// [Server.StartWithContext] for graceful shutdown). Routes support static
// paths, named parameters (:id) and catch-all wildcards (*path).
//
//	s := celeris.New(celeris.Config{Addr: ":8080"})
//	s.GET("/users/:id", func(c *celeris.Context) error {
//	    return c.JSON(200, map[string]string{"id": c.Param("id")})
//	})
//	log.Fatal(s.Start())
//
// Handlers have the signature [HandlerFunc] (func(*[Context]) error) and
// receive a pooled *[Context] that carries the request and response: params
// ([Context.Param]), query/form/body parsing ([Context.Query],
// [Context.Bind]), and typed responses ([Context.JSON], [Context.String],
// [Context.File], [Context.Stream]). Do not retain a *[Context] after the
// handler returns; use [Context.BodyCopy] to keep body bytes alive.
//
// The request strings a Context returns ([Context.Method], [Context.Path],
// [Context.Header], [Context.Param], [Context.Query], [Context.Cookie],
// [Context.Host], [Context.ClientIP], [Context.RawQuery] and the strings
// derived from them) are valid until the handler returns. On [Epoll],
// [IOUring] and [Adaptive] HTTP/1.1 they are views of the connection's
// receive buffer, which the engine reuses for the connection's next request
// and, once the connection closes, for another connection. Compare and read
// them freely during the request; copy one with [strings.Clone] before
// anything keeps it past the handler: a map key or cache entry, a goroutine
// or channel, a closure, a log record a handler may keep, a metric label or
// a span attribute. A kept view later reads other request bytes, possibly
// another client's. [Context.Detach] and [Context.Hijack] copy the values
// the Context holds; see their docs for what they cannot copy.
//
// Group routes with [Server.Group], attach middleware with [Server.Use],
// [RouteGroup.Use] or [Route.Use], and return [HTTPError] (via [NewHTTPError])
// for HTTP-status errors. The in-tree middleware/* packages (logger, recovery,
// cors, compress, …) supply ready-made [HandlerFunc] middleware.
//
// By default handlers run inline on the I/O worker; mark I/O-bound routes with
// [Route.Async] (or flip the default via [Config.AsyncHandlers]) to dispatch
// them on a per-connection goroutine.
//
// On Linux, [Config.Engine] selects between [IOUring], [Epoll], [Adaptive] and
// [Std]; other platforms run [Std] only. [Config.Protocol] selects [HTTP1],
// [H2C] or [Auto] (HTTP/1.1 + h2c). [Server.Collector] exposes per-request
// metrics. Test handlers with the github.com/goceleris/celeris/celeristest
// helpers.
//
// # Documentation
//
// Full guides, tutorials and examples: https://goceleris.dev/docs
// Start here: https://goceleris.dev/docs/getting-started
package celeris
