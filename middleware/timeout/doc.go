// Package timeout provides request timeout middleware for celeris.
//
// The middleware wraps each request's context with a deadline. If the
// downstream handler does not complete before the deadline, the context
// is cancelled and a configurable error response is returned.
//
// Basic usage with the default 5-second timeout:
//
//	server.Use(timeout.New())
//
// Custom timeout and error handler:
//
//	server.Use(timeout.New(timeout.Config{
//	    Timeout: 10 * time.Second,
//	    ErrorHandler: func(c *celeris.Context, err error) error {
//	        return c.JSON(503, map[string]string{"error": "timed out"})
//	    },
//	}))
//
// Set [Config].Preemptive to true to run the handler in a separate
// goroutine with response buffering. In preemptive mode, the middleware
// returns the timeout error immediately when the deadline expires, even
// if the handler is blocked on a non-context-aware operation.
//
// Set [Config].TimeoutFunc to compute a timeout dynamically per request.
// The static [Config].Timeout is used as a fallback when TimeoutFunc is
// nil or returns a non-positive duration.
//
// In cooperative mode (default), the handler runs in the request
// goroutine. The context deadline is set, but the handler must check
// c.Context().Done() to respect the timeout. Cooperative mode has no
// extra goroutine overhead and no response buffering cost.
//
// [Config].ErrorHandler receives the triggering error
// (context.DeadlineExceeded, a matched TimeoutErrors entry, or a
// panic-wrapped error). If it panics, [ErrServiceUnavailable] is
// returned as a last-resort fallback.
//
// [Config].TimeoutErrors lists errors that should be treated as
// timeouts even if the deadline has not been reached (e.g., database
// query timeouts).
//
// [ErrServiceUnavailable] is the exported sentinel error (503) returned
// when no custom error handler is configured, usable with errors.Is.
//
// Set [Config].Skip to bypass the middleware dynamically, or
// [Config].SkipPaths for exact-match path exclusions.
//
// Warning: In preemptive mode, if the handler does not exit promptly
// after context cancellation, the middleware goroutine blocks waiting
// for the handler to complete. This ties up the connection and prevents
// the Context from being returned to the pool. Handlers MUST check
// c.Context().Done() and return quickly on cancellation.
package timeout
