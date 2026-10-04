// Package cache provides a pluggable HTTP response cache middleware.
//
// # Overview
//
// The cache buffers the handler response (status + filtered headers +
// body), encodes it via the versioned wire format in [middleware/store],
// and persists the result under a request-derived key. Subsequent
// requests that produce the same key skip the handler and replay the
// stored response. A 206 Partial Content or a 416 is never stored: both
// answer one request's Range, which the default key does not include, and
// even under a key that includes it a replay would skip the handler's
// If-Range check.
//
// # Backends
//
// Any [store.KV] implementation works. The default [NewMemoryStore]
// provides a sharded LRU with per-shard mutexes; [store.MemoryKV] is
// also compatible but does not implement LRU eviction. For multi-
// instance deployments, use [middleware/session/redisstore.New] with
// a cache-specific prefix.
//
// # Singleflight
//
// By default concurrent requests that miss on the same key coalesce: one
// handler runs, the rest wait for its result. They wait for the handler
// only: not for its client, since every request, the one that ran the
// handler included, writes its own response after the wait, so a client
// that reads slowly delays only its own response; and not for the store's
// Set, which the leader makes once the others have their result. A
// request that arrives during that Set takes the result too. A waiter
// waits only as long as its request context lives. When the handler, or
// the store's Set, panics, the panic is the leader's, and each waiter
// runs its own handler. Set [Config.DisableSingleflight] when handlers
// have side effects that must run per-request.
//
// # Cache-Control
//
// Unless [Config.IgnoreCacheControl] is set, the middleware honors
// directives on the handler's response:
//
//   - no-store, private → the response is not cached
//   - max-age=N         → TTL is capped at min(Config.TTL, N seconds)
//
// # Invalidation
//
//   - [Invalidate] removes a single computed key
//   - [InvalidatePrefix] removes every key with the given prefix
//     (requires store.PrefixDeleter)
//
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs/middleware-content
package cache
