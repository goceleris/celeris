package cache_test

import (
	"time"

	"github.com/goceleris/celeris/middleware/cache"
	"github.com/goceleris/celeris/middleware/store"
)

// ExampleConfig — the minimum useful configuration: an in-memory LRU
// store with a 60-second TTL. Drop the resulting middleware on a
// router with `app.Use(cache.New(cfg))`. Subsequent matching requests
// within the TTL skip the handler chain and replay the cached response.
func ExampleConfig() {
	_ = cache.Config{
		Store: store.NewMemoryKV(),
		TTL:   60 * time.Second,
	}
}

// ExampleConfig_redisStore demonstrates wiring a Redis-backed store via
// any [store.KV] implementation (the project ships
// [store.NewMemoryKV]; the same shape applies to redis / postgres /
// memcached adapters under [middleware/store]).
func ExampleConfig_redisStore() {
	// Stand-in: any store.KV may be assigned here.
	kv := store.NewMemoryKV()
	_ = cache.Config{
		Store:               kv,
		TTL:                 30 * time.Second,
		RespectCacheControl: true,
		Singleflight:        true,
	}
}
