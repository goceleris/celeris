// Package middleware is the umbrella for celeris's production-ready middleware catalog.
//
// It declares no exported symbols of its own. Each piece of middleware lives in
// its own subpackage and exposes a New constructor returning a
// celeris.HandlerFunc (for example cors.New, jwt.New, ratelimit.New,
// compress.New). Install them at one of two points:
//
//   - Server.Use installs route middleware that runs after the router matches a
//     request (logging, recovery, auth, CORS, rate limiting, compression, ...).
//   - Server.Pre installs pre-routing middleware that runs before matching and
//     may mutate the request method, path, scheme, host, or client IP
//     (proxy, redirect, rewrite, methodoverride). Pre-routing middleware that
//     writes a response MUST return without calling c.Next(); routing is
//     then skipped.
//
// A handler that answers the request (writes the response, has it captured
// by a buffering middleware, or takes the connection over) and returns ends
// the chain: the handlers after it, a route included, do not run, and Next
// returns nil to the middleware above. A handler that returns without
// answering and without calling Next lets the chain continue.
//
// Ordering matters: each layer should see the context the layers before it
// established. See the documentation hub below for the recommended install
// order, per-middleware configuration, and cross-cutting conventions (auth
// stacking, the Vary header contract, and how the observe/metrics/otel
// measurement systems relate).
//
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs/middleware
package middleware
