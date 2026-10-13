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
// request that arrives during that Set takes the result too, as long as
// the result is within its TTL: a Set that never returns does not keep it
// alive. A waiter waits only as long as its request context lives; on
// epoll and io_uring an HTTP/1 request context does not end unless a
// middleware such as timeout gives it a deadline. When the handler, or
// the store's Set, panics, the panic is the leader's, and each waiter
// runs its own handler. Set [Config.DisableSingleflight] when handlers
// have side effects that must run per-request.
//
// # Cache-Control
//
// Unless [Config.IgnoreCacheControl] is set, the middleware honors
// directives on the handler's response:
//
//   - no-store, private, no-cache → the response is not cached
//   - max-age=0                   → the response is not cached
//   - max-age=N (N > 0)           → TTL is capped at min(Config.TTL, N seconds)
//   - max-age that is not N       → the response is not cached
//
// A no-cache or max-age=0 response is not stored because a stored one
// would be served without the revalidation the origin asked for.
//
// The word "private" or "no-store" anywhere in the value, in any case, keeps
// the response out of the cache, as it always has, whatever else the value says
// (so x-private-ext, or a "private" inside a quoted string, is refused too).
// That is checked before the value is read, and what is read afterwards can
// only add refusals. The argument of max-age is digits only (RFC 9111 4.2.1:
// an invalid max-age is stale): max-age=abc, max-age=-1, max-age=60abc,
// max-age="60" and max-age= are not stored.
//
// A Cache-Control value that is not a comma-separated list of directives
// (public; max-age=3600, max-age = 60, a quote that never closes) is not
// stored either: a value the cache cannot read with certainty is not guessed
// at, because a wrong guess would serve one user's response to another.
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
