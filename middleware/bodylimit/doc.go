// Package bodylimit provides request body size limiting middleware for
// celeris.
//
// [New] returns a [celeris.HandlerFunc] that enforces a maximum request body
// size using a two-phase check: Content-Length header (fast path) then actual
// body bytes (catches lying or absent Content-Length). Requests exceeding the
// limit are rejected with 413 Request Entity Too Large.
//
// Configure via [Config]: set [Config].Limit to a human-readable size string
// (e.g. "10MB", "1.5GiB"; SI and IEC units accepted; takes precedence over
// [Config].MaxBytes), or set [Config].MaxBytes directly (default 4 MiB).
// Enable [Config].ContentLengthRequired to reject requests that omit
// Content-Length with 411 Length Required. Bodyless methods (GET, HEAD,
// DELETE, OPTIONS, TRACE, CONNECT) are auto-skipped; use [Config].Skip or
// [Config].SkipPaths for additional exclusions. Customize error responses with
// [Config].ErrorHandler. Sentinel errors [ErrBodyTooLarge] and
// [ErrLengthRequired] are usable with errors.Is.
//
// Note: this middleware runs after the engine has buffered the body. It adds
// an application-level per-route cap below the server-wide engine limit
// (celeris.Config.MaxRequestBodySize, default 100 MiB); it is not a
// substitute for setting that limit.
//
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs/middleware-traffic
package bodylimit
