// Package async is internal infrastructure shared by the celeris
// PostgreSQL and Redis drivers. It is not a supported public API: the
// types exported here exist to let the two drivers share plumbing without
// forcing the user-facing packages to leak implementation details. No
// backwards-compatibility guarantees are made for imports from outside
// the celeris module.
//
// # What it provides
//
//   - [Bridge]:  FIFO queue of in-flight [PendingRequest]s per connection.
//     Drivers Enqueue before writing bytes to the wire and Pop as responses
//     arrive. Responses-in-order is a property of the underlying protocols
//     (Postgres v3, RESP).
//
//   - [Pool][C]: a generic worker-affinity connection pool. C is the
//     driver's connection type (must satisfy [Conn]). Each pool keeps a
//     per-worker idle list so handlers on worker N preferentially reuse
//     conns whose event-loop callbacks run on worker N. Lifetime / idle /
//     max-open are enforced by the pool.
//
//   - [Backoff]: exponential-with-jitter delay computation for retry
//     loops. Not safe for concurrent use.
//
//   - Internal [health] checker scheduled by the pool.
//
// Drivers build their user-facing API (postgres.Pool, redis.Client) by
// composing these primitives with a protocol-specific dispatch loop.
package async
