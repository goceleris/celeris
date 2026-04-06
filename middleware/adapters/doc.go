// Package adapters bridges stdlib net/http middleware into celeris handler chains.
//
// Use [WrapMiddleware] to adapt any func(http.Handler) http.Handler into a
// [celeris.HandlerFunc]. This enables reuse of existing stdlib middleware
// such as rs/cors, gorilla/csrf, and similar libraries.
//
//	corsHandler := cors.Handler(cors.Options{AllowedOrigins: []string{"*"}})
//	server.Use(adapters.WrapMiddleware(corsHandler))
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
// # Limitations
//
// The responseCapture used by WrapMiddleware does not implement
// http.Hijacker or http.Flusher. Stdlib middleware that requires
// WebSocket upgrade (via Hijack) or streaming flush (via Flush)
// will not work through WrapMiddleware. For these use cases,
// implement the middleware natively in celeris.
package adapters
