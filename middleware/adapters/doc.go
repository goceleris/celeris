// Package adapters provides conversion functions between celeris handlers
// and net/http handlers, plus a reverse proxy helper.
//
// Unlike standard celeris middleware, this package does not follow the
// New(config ...Config) pattern. It exposes standalone functions for bridging
// the two ecosystems.
//
// # WrapMiddleware: stdlib to celeris
//
// [WrapMiddleware] converts a standard func(http.Handler) http.Handler
// middleware (such as rs/cors, gorilla/csrf, or chi/middleware) into a
// [celeris.HandlerFunc]:
//
//	server.Use(adapters.WrapMiddleware(stdlibCORS))
//
// The wrapped middleware can either call through to the next handler in the
// celeris chain (by invoking its inner http.Handler argument) or
// short-circuit the request (e.g., returning 403 Forbidden for CSRF
// failures). When the middleware short-circuits, its captured status code,
// headers, and body are written to the celeris context automatically.
//
// # ToStdlib: celeris to http.Handler
//
// [ToStdlib] converts a [celeris.HandlerFunc] to an [http.Handler] for use
// with net/http routers, test infrastructure, or stdlib middleware chains.
// It delegates to [celeris.ToHandler].
//
//	http.Handle("/legacy", adapters.ToStdlib(myHandler))
//
// # ReverseProxy
//
// [ReverseProxy] creates a celeris handler that proxies requests to a
// target URL using [net/http/httputil.ReverseProxy]. It supports
// configurable transport, request modification, and error handling via
// functional options:
//
//	target, _ := url.Parse("http://backend:8080")
//	server.Any("/api/*path", adapters.ReverseProxy(target,
//	    adapters.WithTransport(customTransport),
//	    adapters.WithModifyRequest(func(r *http.Request) {
//	        r.Header.Set("X-Custom", "value")
//	    }),
//	))
//
// # Header Propagation
//
// When WrapMiddleware calls through to the celeris chain, response headers
// are written by the downstream celeris handlers directly. Headers set by
// the stdlib middleware before calling its inner handler are not visible to
// the celeris chain (they are written to the capture buffer, not the
// celeris context). This is a fundamental limitation of bridging two
// response-writing models. If the stdlib middleware short-circuits, all of
// its headers are propagated to the celeris response.
package adapters
