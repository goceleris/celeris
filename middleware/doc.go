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
//     writes a response MUST return without calling c.Next().
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
