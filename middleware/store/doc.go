// Package store defines the unified byte-level key-value interface shared
// by celeris middleware (session, csrf, ratelimit, jwt, cache, idempotency).
//
// # Interfaces
//
// [KV] is the minimal required surface: Get, Set with TTL, Delete. All
// implementations must be safe for concurrent use and honor the contract
// that Get returns (nil, [ErrNotFound]) on a missing key.
//
// Optional extension interfaces ([GetAndDeleter], [Scanner], [PrefixDeleter],
// [SetNXer], [Scripter]) surface backend capabilities. Middleware feature-
// detect these via type assertion and fall back to emulation or no-op
// semantics when a backend does not implement them.
//
// # Reference backends
//
//   - [MemoryKV] — in-memory, sharded implementation. Implements every
//     optional extension. Used as the default store and in tests.
//   - middleware/session/redisstore — Redis-backed via driver/redis.
//     Implements KV + Scanner; GetAndDeleter when Redis >= 6.2.
//   - middleware/session/postgresstore — PostgreSQL-backed via driver/postgres.
//     Implements KV only; Reset via TRUNCATE (exposed as io.Closer).
//   - middleware/csrf/redisstore — Redis-backed CSRF storage.
//     Implements KV + GetAndDeleter (GETDEL).
//   - middleware/ratelimit/redisstore — Redis-backed ratelimit store.
//     Implements ratelimit.Store via EVALSHA; not a KV adapter.
//
// # Response wire format
//
// [EncodedResponse] + [ResponseWireVersion] define a byte-efficient
// snapshot format used by cache and idempotency middleware to persist
// captured HTTP responses. The format is versioned to allow forward
// compatibility; decoders reject unknown versions.
//
// # Contract summary
//
//   - Get returns (nil, [ErrNotFound]) on miss. (nil, nil) is forbidden.
//   - Set with ttl == 0 stores with no expiry.
//   - Delete is idempotent: deleting a missing key returns nil.
//   - Values returned by Get are caller-owned; backends must copy.
//   - All methods are safe for concurrent use.
package store
