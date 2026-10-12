// Package singleflight provides request-coalescing middleware for celeris.
//
// When several identical requests arrive concurrently, only the first (the
// "leader") executes the handler chain; the rest (the "waiters") block until
// the leader finishes and then receive a copy of its response. This absorbs
// thundering-herd bursts on hot endpoints so they hit the backend once.
//
// Install it with [New], optionally passing a [Config]. The zero-value
// configuration deduplicates on method + path + sorted query string +
// Authorization + Cookie + Accept-Encoding + the four preconditions
// (If-None-Match, If-Modified-Since, If-Match and If-Unmodified-Since), so
// requests from different authenticated users are never coalesced, neither
// are requests that accept different encodings (values the negotiator reads
// alike, such as "gzip, br" and "gzip,br" or "GZIP" and "gzip", are the same;
// a value longer than 256 bytes is compared as sent), and neither are
// conditional requests with different validators: a 304 or a 412 answers its
// own validator. Identical revalidations (GET or HEAD), such as a CDN's, do
// coalesce. A conditional write (a PUT, POST, PATCH or DELETE with a
// precondition such as If-Match) never does: its handler is the update. A
// request with a Range header is never coalesced: its response answers that
// Range. With a custom [Config.KeyFunc], which may not include the
// validators, a conditional request is not coalesced either. Whatever the
// key, a waiter whose request differs from the leader's in a header that the
// leader's response names in Vary (or a response with Vary: *) runs its own
// handler instead of taking the leader's response. Use [Config.KeyFunc] to
// change the key, and [Config.Skip] or [Config.SkipPaths] to exclude requests
// (for example non-idempotent methods or large-response endpoints). Waiter
// responses carry an "x-singleflight: HIT" header.
//
// When the leader's handler returns an error or panics, every waiter returns
// that error or panics with that value, as a copy made before the leader
// returns: the leader's request strings are valid only until then. A string
// panic value is copied. An error becomes one whose message is a copy, which
// unwraps to the leader's error, so [errors.Is] and [errors.As] find what it
// holds; an [errors.As] to a *[celeris.HTTPError] finds a copy whose Message
// is copied too. Fields of other error types, and other panic values, are
// the leader's own: build them from copies ([strings.Clone]) if a waiter may
// read them, as celeris does for [celeris.BindError]'s Value. A waiter's
// error is not the leader's error value, so compare with [errors.Is], not
// ==. The exception is a leader whose own request context ended (its client
// went away, its deadline passed) and whose handler returned that context's
// error: that error is its client's, so each waiter runs its own handler
// instead of returning it.
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
