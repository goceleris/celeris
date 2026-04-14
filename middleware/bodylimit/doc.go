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
// LAYER OF DEFENSE — read carefully:
//
// The DoS-grade cap lives at the engine read layer:
// [celeris.Config.MaxRequestBodySize] (default 100 MB). Bodies larger
// than that are rejected by the engine BEFORE any buffering happens, so
// the framework never holds them in memory. Set
// [celeris.Config.MaxRequestBodySize] to your real maximum at server
// construction time.
//
// This middleware runs AFTER the engine has already buffered the body.
// Use it for per-route caps that are smaller than the server-wide
// MaxRequestBodySize (e.g. a /comments endpoint that should accept at
// most 64 KB while /uploads accepts up to MaxRequestBodySize). It is
// NOT a substitute for setting MaxRequestBodySize itself; a malicious
// client can still send up to MaxRequestBodySize before this middleware
// rejects them.
//
// Enable [Config].ContentLengthRequired to reject requests without
// Content-Length (411 Length Required), forcing clients to declare the
// body size up-front.
//
// Bodyless methods (GET, HEAD, DELETE, OPTIONS, TRACE, CONNECT) are
// auto-skipped. Use [Config].Skip or [Config].SkipPaths for additional
// exclusions. Set [Config].ErrorHandler to customize error responses.
//
// [ErrBodyTooLarge] and [ErrLengthRequired] are exported sentinel errors
// usable with errors.Is for upstream error handling.
package bodylimit
