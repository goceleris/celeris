// Package idempotency implements the HTTP Idempotency-Key request pattern.
//
// # Semantics
//
// The middleware looks up the header named by [Config.KeyHeader]
// (default "Idempotency-Key") and:
//
//  1. Validates the key is 1..MaxKeyLength printable ASCII.
//  2. Attempts to atomically acquire a lock via [store.SetNXer].
//  3. On acquisition, runs the handler and stores the captured
//     response under the key with [Config.TTL] expiry. The lock
//     entry is replaced by the completed entry atomically.
//  4. On contention with an in-flight duplicate, returns 409 Conflict
//     via [Config.OnConflict] (no wait-and-retry).
//  5. On a subsequent request that finds a completed entry, replays
//     the stored response without running the handler.
//
// # Body hash
//
// [Config.BodyHash] enables SHA-256 hashing of the request body (up
// to [Config.MaxBodyBytes]) and compares it to the stored hash on
// replay. Mismatches return 422 Unprocessable Entity — catching
// clients that reuse a key with different payloads, which the IETF
// draft recommends rejecting. Disabled by default because hashing
// large bodies costs memory.
//
// # Store requirements
//
// The store must implement both [store.KV] and [store.SetNXer]. The
// default [NewMemoryStore] is in-memory and sharded, suitable for
// single-instance deployments. For multi-instance deployments, wire
// a [middleware/session/redisstore.Store] — it satisfies both
// interfaces. (Wrap with [store.Prefixed] to namespace alongside
// session data.)
//
// # Lock expiry
//
// A crashed handler leaves the lock entry behind. Once
// [Config.LockTimeout] passes, the key becomes acquirable again.
// Choose LockTimeout ≥ the worst-case handler latency plus network
// margin to avoid a second execution of a still-running request.
package idempotency
