// Package recovery provides panic recovery middleware for celeris.
//
// The middleware catches panics from downstream handlers, logs the
// stack trace via slog, and returns a configurable error response
// instead of crashing the server.
//
// Basic usage with defaults (4 KB stack trace, JSON 500 response):
//
//	server.Use(recovery.New())
//
// Custom error handler and stack size:
//
//	server.Use(recovery.New(recovery.Config{
//	    StackSize: 8192,
//	    ErrorHandler: func(c *celeris.Context, err any) error {
//	        return c.String(500, "Something went wrong")
//	    },
//	}))
//
// Set [Config].StackSize to 0 to disable stack trace capture (the panic
// value, method, and path are still logged). Set [Config].DisableLogStack
// to true to suppress panic logging entirely.
//
// # Special Panic Types
//
// Panics with [http.ErrAbortHandler] are re-panicked rather than
// recovered, preserving the standard library's abort semantics.
//
// Broken pipe and ECONNRESET errors are detected automatically and
// logged at WARN level without a stack trace. Use
// [Config].BrokenPipeHandler to customize the response for these cases.
//
// # Nested Recovery
//
// If [Config].ErrorHandler itself panics, the middleware falls back to
// a default 500 Internal Server Error response.
//
// # Log Level
//
// Set [Config].LogLevel to control the slog level for normal panic log
// entries (default: [slog.LevelError]). Broken pipe panics are always
// logged at [slog.LevelWarn] regardless of this setting.
//
// # Middleware Order
//
// Register after logger/metrics so they see the 500 status from recovered panics.
package recovery
