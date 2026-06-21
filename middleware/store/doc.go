// Package store defines the unified byte-level key-value interface shared
// by celeris middleware (session, csrf, cache, idempotency, SSE replay).
//
// # Core interface
//
// [KV] is the minimal required surface: Get, Set with TTL, Delete. All
// implementations must be safe for concurrent use and honor the contract
// that Get returns (nil, [ErrNotFound]) on a missing key; returning
// (nil, nil) is forbidden.
//
// Optional extension interfaces surface backend capabilities; middleware
// feature-detects them via type assertion and falls back to emulation
// or no-op semantics when a backend does not implement them:
//
//   - [GetAndDeleter] — atomic GET+DEL (e.g. Redis GETDEL); used by csrf.
//   - [Scanner] — key enumeration by prefix; used by session Reset.
//   - [PrefixDeleter] — bulk delete by prefix; used by cache InvalidatePrefix.
//   - [SetNXer] — atomic set-if-not-exists; used by idempotency lock acquisition.
//   - [Counter] — atomic monotonic counter (INCR); used by SSE replay for cross-process IDs.
//   - [Scripter] — server-side atomic scripts (EVALSHA); used by ratelimit Redis adapter.
//
// # Reference implementations
//
//   - [MemoryKV] / [NewMemoryKV] — in-memory sharded store. Implements KV,
//     GetAndDeleter, Scanner, PrefixDeleter, SetNXer, and Counter.
//     Default store for single-process deployments and tests.
//   - middleware/session/redisstore — Redis-backed; KV + Scanner + GetAndDeleter.
//   - middleware/session/postgresstore — PostgreSQL-backed; KV only.
//   - middleware/csrf/redisstore — Redis-backed CSRF; KV + GetAndDeleter.
//   - middleware/ratelimit/redisstore — Redis-backed ratelimit via EVALSHA; implements Scripter.
//
// # Utilities
//
// [Prefixed] wraps any [KV] so all keys are transparently namespaced with a
// prefix — useful when sharing one backend among multiple middleware.
// [EncodeJSON] / [DecodeJSON] are convenience helpers for adapters that
// persist structured payloads through the byte-level [KV] surface.
// [EncodedResponse] + [ResponseWireVersion] define the versioned wire format
// used by cache and idempotency middleware to persist captured HTTP responses.
//
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs/data-stores
package store
