// Package ratelimit provides token-bucket and sliding-window rate limiting
// middleware for Celeris.
//
// [New] returns a [celeris.HandlerFunc] that enforces per-key request limits
// using a sharded token-bucket algorithm by default. Keys default to the client
// IP via [celeris.Context.ClientIP]; supply [Config.KeyFunc] to key on anything
// else (API key, user ID, etc.).
//
// Core configuration fields: [Config.RPS] and [Config.Burst] set the static
// rate; [Config.Rate] accepts human-readable strings ("100-M", "1000-H") parsed
// by [ParseRate]. [Config.RateFunc] selects a per-request rate string, with
// distinct limiters cached up to [Config.MaxDynamicLimiters] (default 1024).
// [Config.SlidingWindow] switches to a sliding-window counter for smoother
// limiting near window boundaries. [Config.Store] plugs in an external backend
// (e.g. Redis) that implements [Store]; [StoreUndo] is the optional extension
// for token refunds used by [Config.SkipFailedRequests] and
// [Config.SkipSuccessfulRequests].
//
// On every allowed response the middleware sets X-RateLimit-Limit,
// X-RateLimit-Remaining, and X-RateLimit-Reset headers; denied responses
// (429) also receive Retry-After. Set [Config.DisableHeaders] to suppress all
// rate-limit headers. [Config.ErrorHandler] customises the 429 response (it
// supersedes the deprecated [Config.LimitReached]). The sentinel error
// [ErrTooManyRequests] is passed to ErrorHandler and is usable with errors.Is.
// [ErrDynamicLimitersExhausted] is returned when MaxDynamicLimiters is full.
//
// [ValidateConfig] checks a [Config] for errors without panicking, useful when
// loading configuration from files or untrusted sources before calling [New].
// [New] spawns background goroutines for bucket eviction; set
// [Config.CleanupContext] to a cancellable context to stop them when the
// middleware is no longer needed.
//
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs/middleware-traffic
package ratelimit
