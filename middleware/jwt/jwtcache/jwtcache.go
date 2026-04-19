// Package jwtcache provides a convenience [store.KV] constructor
// for JWT middleware's JWKSCache option. It wraps a native celeris
// [driver/redis.Client] under a JWKS-specific key prefix so multiple
// celeris middleware can safely share one Redis deployment.
//
// Callers can alternatively pass any other [store.KV] (MemoryKV,
// Postgres-backed, or a custom implementation); jwtcache.New is purely
// a convenience wrapper with sensible defaults.
package jwtcache

import (
	sessionredis "github.com/goceleris/celeris/middleware/session/redisstore"

	"github.com/goceleris/celeris/driver/redis"
	"github.com/goceleris/celeris/middleware/store"
)

// Options configure the JWKS Redis cache.
type Options struct {
	// KeyPrefix is prepended to every JWKS cache key. Default: "jwks:".
	KeyPrefix string
}

// New returns a [store.KV] suitable for use as JWT Config.JWKSCache.
// Internally reuses [middleware/session/redisstore.Store] with a
// JWKS-specific prefix; both adapters share the same mapping from
// driver/redis semantics to the store.KV contract.
func New(client *redis.Client, opts ...Options) store.KV {
	prefix := "jwks:"
	if len(opts) > 0 && opts[0].KeyPrefix != "" {
		prefix = opts[0].KeyPrefix
	}
	return sessionredis.New(client, sessionredis.Options{KeyPrefix: prefix})
}
