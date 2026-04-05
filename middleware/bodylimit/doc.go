// Package bodylimit provides request body size limiting middleware for
// celeris.
//
// The middleware enforces a maximum request body size using a two-phase
// approach: first checking Content-Length (fast path), then verifying
// actual body bytes (catches lying or absent Content-Length). Requests
// exceeding the limit are rejected with 413 Request Entity Too Large.
//
// Basic usage with the default 4 MB limit:
//
//	server.Use(bodylimit.New())
//
// Human-readable size string:
//
//	server.Use(bodylimit.New(bodylimit.Config{
//	    Limit: "10MB",
//	}))
//
// [Config].Limit accepts SI and IEC units: B, KB, MB, GB, TB, PB, EB,
// KiB, MiB, GiB, TiB, PiB, EiB. Fractional values are supported.
// When set, Limit takes precedence over MaxBytes.
//
// IMPORTANT: Celeris buffers the full request body before middleware
// runs. The framework's maxRequestBodySize (100 MB) is the hard ceiling.
// This middleware adds an application-level check on already-buffered
// data. Enable [Config].ContentLengthRequired to reject requests
// without Content-Length (411 Length Required).
//
// Bodyless methods (GET, HEAD, DELETE, OPTIONS, TRACE, CONNECT) are
// auto-skipped. Use [Config].Skip or [Config].SkipPaths for additional
// exclusions. Set [Config].ErrorHandler to customize error responses.
//
// [ErrBodyTooLarge] and [ErrLengthRequired] are exported sentinel errors
// usable with errors.Is for upstream error handling.
package bodylimit
