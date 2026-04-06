// Package adapters bridges stdlib net/http middleware into celeris handler chains.
//
// Use [WrapMiddleware] to adapt any func(http.Handler) http.Handler into a
// [celeris.HandlerFunc]. This enables reuse of existing stdlib middleware
// such as rs/cors, gorilla/csrf, and similar libraries.
//
//	corsHandler := cors.Handler(cors.Options{AllowedOrigins: []string{"*"}})
//	server.Use(adapters.WrapMiddleware(corsHandler))
//
// Warning: Do not use both adapters.WrapMiddleware with a stdlib CORS
// library (e.g., rs/cors) AND the native celeris/middleware/cors in the
// same chain. This produces duplicate Access-Control-* headers and
// conflicting preflight handling. Use one or the other.
//
// For converting celeris handlers to stdlib, use [celeris.ToHandler] directly.
//
// # How It Works
//
// WrapMiddleware creates a temporary http.ResponseWriter (responseCapture)
// and reconstructs an *http.Request from the celeris Context. The stdlib
// middleware runs against these. If the middleware calls the inner handler,
// control returns to the celeris chain via c.Next(). Any response headers
// the stdlib middleware set before calling next are copied to the celeris
// response.
//
// If the stdlib middleware short-circuits (does not call the inner handler),
// the captured status code, headers, and body are written through celeris.
//
// # Performance
//
// WrapMiddleware reconstructs an *http.Request per call, which costs
// 8-15 heap allocations depending on header count and body presence.
// For hot-path middleware (e.g., CORS on every request), prefer the native
// celeris/middleware/cors over adapters.WrapMiddleware(rs/cors) for
// zero-alloc performance. Use WrapMiddleware for middleware that runs
// infrequently or where the stdlib library has no native celeris equivalent.
//
// # Limitations
//
// The responseCapture implements http.Flusher (as a no-op for compatibility)
// but does not implement http.Hijacker. Stdlib middleware that requires
// WebSocket upgrade (via Hijack) will not work through WrapMiddleware.
//
// # Reverse Proxy
//
// [ReverseProxy] creates a handler that forwards requests to a target URL
// using [net/http/httputil.ReverseProxy] under the hood:
//
//	target, _ := url.Parse("http://backend:8080")
//	server.Any("/api/*path", adapters.ReverseProxy(target))
//
// Options:
//
//   - [WithTransport]: set a custom [http.RoundTripper]
//   - [WithModifyRequest]: mutate outbound requests (e.g. add headers)
//   - [WithModifyResponse]: inspect or modify backend responses before forwarding
//   - [WithErrorHandler]: custom error handling for proxy failures
//
// The proxy automatically sets X-Forwarded-For, X-Forwarded-Host, and
// X-Forwarded-Proto via [net/http/httputil.ProxyRequest.SetXForwarded].
// Panics if target is nil.
//
// ReverseProxy delegates to [celeris.Adapt], which buffers the response.
// Streaming responses (SSE, WebSocket upgrade) are not supported through
// the proxy.
package adapters
