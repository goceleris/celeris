// Package singleflight provides request-coalescing middleware for celeris.
//
// When several identical requests arrive concurrently, only the first (the
// "leader") executes the handler chain; the rest (the "waiters") block until
// the leader finishes and then receive a copy of its response. This absorbs
// thundering-herd bursts on hot endpoints so they hit the backend once.
//
// Install it with [New], optionally passing a [Config]. The zero-value
// configuration deduplicates on method + path + sorted query string +
// Authorization + Cookie, so requests from different authenticated users are
// never coalesced. Use [Config.KeyFunc] to change the key, and [Config.Skip]
// or [Config.SkipPaths] to exclude requests (for example non-idempotent
// methods or large-response endpoints). Waiter responses carry an
// "x-singleflight: HIT" header.
//
//	server.Use(singleflight.New())
//
// Singleflight buffers the leader's response, so install it after timeout
// middleware and before response transforms such as compress or etag. It is
// intended for idempotent reads; a custom KeyFunc that returns user-specific
// data must incorporate user identity to avoid cross-user leakage.
//
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs/middleware-traffic
package singleflight
