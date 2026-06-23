// Package recovery provides panic recovery middleware for celeris.
//
// The middleware catches panics from downstream handlers, logs the stack trace
// via slog, and returns a configurable error response instead of crashing the
// server. Register it as early as possible in the middleware chain so it wraps
// all other handlers.
//
// Basic usage with defaults (4 KB stack trace, JSON 500 response):
//
//	server.Use(recovery.New())
//
// [New] accepts an optional [Config] to customise behaviour. Key fields:
//   - [Config].ErrorHandlerErr — preferred error handler (receives a typed error).
//   - [Config].ErrorHandler — legacy handler (receives any); kept for compatibility.
//   - [Config].BrokenPipeHandler — custom handler for broken pipe / ECONNRESET panics.
//   - [Config].StackSize — max bytes for stack trace capture (default 4096; 0 disables).
//   - [Config].DisableLogStack — suppress all panic log output when true.
//   - [Config].LogLevel — slog level for normal panic entries (default [slog.LevelError]).
//   - [Config].Logger — slog logger (default [slog.Default]).
//   - [Config].Skip / [Config].SkipPaths — skip recovery for selected requests.
//
// Panics with [http.ErrAbortHandler] are re-panicked to preserve standard
// library abort semantics. Broken pipe and ECONNRESET panics are logged at
// WARN level without a stack trace.
//
// The package exports sentinel errors for use with [errors.Is]:
//   - [ErrPanic]: generic panic recovery.
//   - [ErrBrokenPipe]: broken pipe or ECONNRESET.
//   - [ErrPanicContextCancelled]: panic after request context was cancelled.
//   - [ErrPanicResponseCommitted]: panic after response has been committed.
//
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs/error-handling
package recovery
