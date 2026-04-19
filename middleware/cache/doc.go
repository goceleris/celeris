// Package cache provides a pluggable HTTP response cache middleware.
//
// # Overview
//
// The cache buffers the handler response (status + filtered headers +
// body), encodes it via the versioned wire format in [middleware/store],
// and persists the result under a request-derived key. Subsequent
// requests that produce the same key skip the handler and replay the
// stored response.
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
// When [Config.Singleflight] is true (default), concurrent requests
// that miss on the same key coalesce: one handler runs, the rest wait
// for its result. Turn this off when handlers have side effects that
// must run per-request.
//
// # Cache-Control
//
// With [Config.RespectCacheControl] (default: true), the middleware
// honors directives on the handler's response:
//
//   - no-store, private → the response is not cached
//   - max-age=N         → TTL is capped at min(Config.TTL, N seconds)
//
// # Invalidation
//
//   - [Invalidate] removes a single computed key
//   - [InvalidatePrefix] removes every key with the given prefix
//     (requires store.PrefixDeleter)
package cache
