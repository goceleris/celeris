// Package ratelimit provides token-bucket rate limiting middleware for
// celeris.
//
// The limiter uses a sharded token-bucket algorithm keyed by client IP
// (by default). Each key gets an independent bucket that refills at the
// configured rate up to the burst capacity. A background goroutine
// periodically evicts expired buckets.
//
// Basic usage with defaults (10 RPS, burst 20):
//
//	server.Use(ratelimit.New())
//
// Human-readable rate string (100 per minute, burst 200):
//
//	server.Use(ratelimit.New(ratelimit.Config{
//	    Rate:  "100-M",
//	    Burst: 200,
//	}))
//
// Custom limits keyed by API key:
//
//	server.Use(ratelimit.New(ratelimit.Config{
//	    RPS:   100,
//	    Burst: 200,
//	    KeyFunc: func(c *celeris.Context) string {
//	        return c.Header("x-api-key")
//	    },
//	}))
//
// The middleware sets X-RateLimit-Limit, X-RateLimit-Remaining, and
// X-RateLimit-Reset headers on every allowed response, and Retry-After
// on denied responses (429 Too Many Requests). Set DisableHeaders to
// suppress all rate limit headers.
//
// [Config].SlidingWindow enables a sliding window counter algorithm for
// smoother rate limiting near window boundaries.
//
// [Config].RateFunc allows per-request rate selection, with distinct
// limiters cached up to [Config].MaxDynamicLimiters (default 1024).
//
// [ParseRate] parses rate strings in "<count>-<unit>" format (S, M, H, D)
// into RPS and burst values, returning an error for malformed input.
//
// [ErrTooManyRequests] is the exported sentinel error (429) returned on
// deny, usable with errors.Is for error handling in upstream middleware.
//
// Set [Config].Skip to bypass the middleware dynamically, or
// [Config].SkipPaths for exact-match path exclusions.
//
// [Config].Store allows plugging in an external storage backend (e.g., Redis)
// that implements the [Store] interface. The store handles its own rate logic.
//
// [Config].SkipFailedRequests and [Config].SkipSuccessfulRequests refund
// tokens for requests that fail (status >= 400) or succeed (status < 400),
// respectively. Store backends must implement [StoreUndo] for refunds.
//
// [Config].CleanupContext controls the lifetime of background goroutines.
// When the context is cancelled, cleanup goroutines stop. If nil, they run
// until the process exits.
//
// Note: [New] spawns one or more background goroutines for bucket eviction.
// Set CleanupContext to a cancellable context to ensure they are stopped
// when the middleware is no longer needed.
package ratelimit
