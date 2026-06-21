// Package adapters bridges stdlib net/http middleware and handlers into celeris handler chains.
//
// Use [WrapMiddleware] to adapt any func(http.Handler) http.Handler into a
// [celeris.HandlerFunc], enabling reuse of existing stdlib middleware such as
// rs/cors, gorilla/csrf, and similar libraries.
//
// Use [ReverseProxy] to forward requests to a backend URL, with optional hooks
// via [WithTransport], [WithModifyRequest], [WithModifyResponse], and
// [WithErrorHandler].
//
// Note: do not combine [WrapMiddleware] with a stdlib CORS library and the
// native celeris/middleware/cors in the same chain — this produces duplicate
// Access-Control-* headers and conflicting preflight handling.
//
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs/net-http-interop
package adapters
