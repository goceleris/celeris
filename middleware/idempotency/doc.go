// Package idempotency implements the HTTP Idempotency-Key request pattern for Celeris.
//
// [New] returns a middleware that deduplicates retried writes. When a request
// carries the header named by [Config.KeyHeader] (default "Idempotency-Key"),
// the middleware acquires an atomic lock via [KVStore], runs the handler once,
// persists the response, and replays it on subsequent requests with the same
// key. Concurrent duplicates return 409 Conflict (overridable via
// [Config.OnConflict]). Requests without the key header pass through unchanged.
//
// Key types: [Config] controls all behaviour; [KVStore] is the store interface
// ([store.KV] + [store.SetNXer]); [NewMemoryStore] provides a sharded
// in-memory default suitable for single-instance deployments. For
// multi-instance deployments, supply a Redis-backed store and namespace it
// with [store.Prefixed]. Set [Config.BodyHash] to detect key reuse with
// different payloads (returns 422 on mismatch). [ErrStoreMissingSetNX] is
// returned by [New] when a provided store lacks atomic SetNX support.
//
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs/middleware-traffic
package idempotency
