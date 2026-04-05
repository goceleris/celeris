// Package singleflight provides request deduplication middleware for celeris.
//
// When multiple identical requests arrive concurrently, only the first
// (the "leader") executes the handler chain. Subsequent requests (the
// "waiters") block until the leader completes, then receive a copy of
// the leader's response. This prevents the thundering herd problem where
// a popular endpoint receives a burst of identical requests that all hit
// the backend simultaneously.
//
// Basic usage:
//
//	server.Use(singleflight.New())
//
// Custom key function:
//
//	server.Use(singleflight.New(singleflight.Config{
//	    KeyFunc: func(c *celeris.Context) string {
//	        return c.Path() // ignore query parameters
//	    },
//	}))
//
// # Algorithm
//
// The middleware maintains an in-memory map of in-flight keys. For each
// incoming request:
//
//  1. Compute the deduplication key via [Config].KeyFunc.
//  2. Lock the group and check the map.
//  3. If the key exists, the request is a waiter: unlock, wait for the
//     leader to finish, then replay the captured response.
//  4. If the key is absent, the request is the leader: register the key,
//     unlock, buffer the response, execute c.Next(), capture the result,
//     remove the key from the map, and wake all waiters.
//
// The embedded singleflight group uses no external dependencies.
//
// # Default Key
//
// The default key function produces: method + "\x00" + path + "\x00" +
// sorted query string. Query parameters are sorted via [url.Values.Encode]
// so that ?a=1&b=2 and ?b=2&a=1 produce the same key. When the request
// has no query string, the query component is omitted entirely (no
// parsing overhead).
//
// # x-singleflight Response Header
//
// Waiter responses include the header "x-singleflight: HIT" so that
// callers (and observability tools) can distinguish coalesced responses
// from leader responses. The leader response does not carry this header.
//
// # Middleware Ordering
//
// Singleflight should run AFTER timeout middleware (so each coalesced
// request respects its own timeout) and BEFORE transform middleware
// like compress or etag (so the response is captured before transformation):
//
//	server.Use(timeout.New(...))      // outermost
//	server.Use(singleflight.New())    // dedup on uncompressed response
//	server.Use(compress.New())        // innermost
//	server.Use(etag.New())
//
// # Idempotency
//
// Singleflight is designed for idempotent read endpoints (GET, HEAD).
// Using it on non-idempotent methods (POST, PUT, DELETE) may cause
// unintended behavior: only one request executes and all waiters receive
// the same response. If your endpoint modifies state, either skip it
// with [Config].SkipPaths or use a [Config].KeyFunc that differentiates
// by request body or session.
//
// # Error Propagation
//
// If the leader's handler returns an error, all waiters receive the same
// error. The error is propagated as-is (including [celeris.HTTPError]
// with its status code).
//
// # Panic Propagation
//
// If the leader's handler panics, the panic value is captured and
// re-panicked in every waiter goroutine (and in the leader after
// cleanup). This ensures recovery middleware further up the chain
// catches the panic in every request context.
//
// # Non-2xx Responses
//
// Non-2xx responses (404, 500, etc.) are coalesced just like 2xx
// responses. The middleware does not distinguish between success and
// failure status codes — it deduplicates all in-flight requests for
// the same key.
//
// # Skipping
//
// Use [Config].Skip for dynamic skip logic or [Config].SkipPaths for
// path exclusions. SkipPaths uses exact path matching:
//
//	server.Use(singleflight.New(singleflight.Config{
//	    SkipPaths: []string{"/admin", "/webhook"},
//	}))
package singleflight
