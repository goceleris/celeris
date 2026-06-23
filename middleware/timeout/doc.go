// Package timeout provides request timeout middleware for celeris.
//
// [New] returns a [celeris.HandlerFunc] that wraps each request's context
// with a deadline. If the downstream handler does not complete within the
// configured duration, the context is cancelled and a configurable error
// response is returned.
//
// Key exported symbols:
//
//   - [New] — constructs the middleware from a [Config].
//   - [Config] — options: [Config.Timeout] (static duration, default 5 s),
//     [Config.TimeoutFunc] (per-request duration; overrides Timeout when > 0),
//     [Config.ErrorHandler] (called on timeout; default returns 503),
//     [Config.TimeoutErrors] (treat matching upstream errors as timeouts),
//     [Config.Preemptive] (run handler in a goroutine; see below),
//     [Config.Skip] / [Config.SkipPaths] (bypass the middleware).
//   - [ErrServiceUnavailable] — sentinel 503 error; aliases
//     celeris.ErrServiceUnavailable for cross-middleware errors.Is matching.
//
// Cooperative mode (default, Preemptive: false): the handler runs in the
// request goroutine with the context deadline set. Handlers must observe
// c.Context().Done() to respect the cancellation. No goroutine or buffer
// overhead.
//
// Preemptive mode (Preemptive: true): the handler runs in a spawned goroutine
// with the response buffered. When the deadline expires the middleware waits
// for the goroutine to exit, discards the buffered response, and invokes the
// error handler. Handlers MUST exit promptly on context cancellation to avoid
// blocking the connection. Incompatible with streaming (StreamWriter).
//
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs/middleware-traffic
package timeout
