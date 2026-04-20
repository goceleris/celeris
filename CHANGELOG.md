# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.5.0] — 2026-04-21

Middleware-level integration of the v1.4.0 native drivers, three new
middlewares, and memcached cluster failover. Ships **breaking changes**
to `middleware/session` and `middleware/csrf` — both now sit on a
unified `middleware/store.KV` interface; deprecation shims are provided.

### Added

- **`middleware/store`** — unified `KV` interface plus optional
  extensions (`GetAndDeleter`, `Scanner`, `PrefixDeleter`, `SetNXer`,
  `Scripter`). `NewMemoryKV` (sharded LRU, 4-shards-per-tick cleanup),
  `Prefixed`, `EncodeJSON`/`DecodeJSON`. All middleware/driver adapters
  plug in here. (#217)
- **Session / CSRF / JWT / ratelimit adapters**
  - `middleware/session/redisstore` (#212)
  - `middleware/session/postgresstore` (#213) — auto-created
    `celeris_sessions` table, `ON CONFLICT` upsert, background expiry
    sweep
  - `middleware/session/memcachedstore` — full parity minus `Scanner`
    (memcached has no SCAN) ✨
  - `middleware/csrf/redisstore` (#215) — atomic single-use via
    `GETDEL` (Redis ≥6.2, falls back to `GET+DEL` via `OldRedisCompat`)
  - `middleware/csrf/memcachedstore` — atomic single-use via
    `Gets + CAS(sentinel, 1s TTL) + Delete` ✨
  - `middleware/ratelimit/redisstore` (#214) — Lua token bucket via
    `EVALSHA`, reloads on `NOSCRIPT`
  - `middleware/ratelimit/memcachedstore` — token bucket via CAS loop
    (24 retries, 16-byte bucket state) ✨
  - `middleware/jwt/jwtcache` (#216) — Redis-backed JWKS cache, keyed
    per URL, write-through on HTTP refresh
  - `middleware/jwt/jwtmccache` — memcached-backed JWKS cache ✨
- **New middlewares**
  - `middleware/cache` (#184) — HTTP response cache with sharded LRU
    store, optional singleflight, Cache-Control respect, Vary support,
    configurable key generation. HIT ≤ 2 allocs/op, MISS ≤ 5 allocs/op.
  - `middleware/idempotency` (#185) — Idempotency-Key per RFC draft,
    `SetNX`-based lock + state-machine, body-hash validation, 409 on
    concurrent duplicate. Replay ≤ 3 allocs/op.
  - `middleware/overload` (#195) — 5-stage CPU ladder
    (Normal/Expand/Reap/Reorder/Backpressure/Reject) with hysteresis,
    opt-in reap, per-priority reorder/reject. Zero allocs on Normal.
- **Memcached cluster failover (#243)** — passive + background-probe
  healing. `clusterNode.failing/lastFailAt/consecutiveFails` atomics;
  `pickNode` walks `c.nodes` (not the ring) on failing, O(NumNodes) not
  O(ringWeight); `probeLoop` goroutine clears flags on recovery; new
  `NodeHealth()` / `NodeStats()` observability.
- **CI** — `memcached-cluster-failover` matrix (1.6.29 / 1.6.36 /
  1.6.41) in `drivers.yml`; memcached conformance in `ci.yml`.
- **Benchmark harness** — `test/benchcmp_db/` module, compares celeris
  vs fiber v3 / echo v4 / chi v5 / net/http stdlib on end-to-end HTTP +
  real DB with same middleware stack + same keep-alive pool.

### Changed

- **`session.Config.Store` now takes `middleware/store.KV`** (breaking,
  see *Migration*).
- **`csrf.Config.Storage` now takes `middleware/store.KV`** (breaking,
  see *Migration*).
- Session values are JSON-encoded on the wire (was gob) so multi-
  language / multi-service sharing works. Numeric values decode as
  `float64`; built-in accessors handle both `int` and `float64` forms
  transparently.
- `memcachedstore` for ratelimit exposes `Store.RetriesTotal()` for
  contention observability under the CAS loop.

### Fixed

- **`WithEngine(srv)` deadlock under concurrent warmup on inline
  handlers.** When `AsyncHandlers=false` (celeris default) and the
  engine's `WorkerLoop` didn't implement the driver's
  `syncRoundTripper` interface (io_uring, epoll today), `pool.Open`
  called `doStartup` which blocked on a channel while the same locked-M
  worker goroutine was the only one that could drain the reply CQE.
  Repro: 16-30% hang rate on 200-goroutine warmup. Fix: drivers now
  auto-fall-back to direct mode (Go netpoll, M-safe) when sync support
  is unavailable. Symmetric across `driver/{postgres,redis,memcached}`.
- 25 golangci-lint errors across `errcheck`, `gofmt`, `revive`,
  `staticcheck`, `unused` (internal fakes + session/csrf/cache tests).

### Performance

Measured on `mini@msr1` (ARM Cortex-A720, 12 cores, kernel 6.6.10,
Go 1.26.1), `-benchtime=3s -count=3`, median reported.

- **Session via Redis (end-to-end HTTP + DB):** 457 µs / 78 allocs
  (celeris leads Fiber by -11 %, Echo/Chi/Stdlib by -21 %).
- **Cache GET via Redis:** celeris wins allocs (-23 % vs Echo/Chi),
  within 2 % of Fiber on ns/op.
- **Memcached GET component:** 48 µs / 16 B / 2 allocs (celeris beats
  gomemcache -19 % latency, 7× less memory).
- **Pure middleware chains:** celeris wins ChainAPI (4.1 µs) and
  ChainFullStack (10.7 µs), is 5–10× faster than Echo/Chi/Stdlib on
  every chain.
- **PGQuery e2e:** prior matrix reported celeris lagging — root cause
  was the benchmark running with default `AsyncHandlers=false`; with
  the correct config (see `celeris.Config.AsyncHandlers` doc) it drops
  from 5.9 ms to 855 µs, near parity with `pgx` + `net/http` stdlib.

### Deprecated

- `session.Store` interface (alias retained; use `store.KV` directly).
- `session.NewMemoryStore` (alias for `store.NewMemoryKV`).
- `csrf.Storage` interface (alias retained; use `store.KV` directly).
- `csrf.NewMemoryStorage` (alias for `store.NewMemoryKV`).

All deprecated symbols remain wired through `StoreFromKV` /
`StorageFromKV` shims so existing user code compiles without changes.
Removal scheduled for v1.6.0.

### Breaking changes

See *Migration* below.

---

## Migration: v1.4.0 → v1.5.0

### 1. Session store

**Before** (v1.4.0):

```go
import "github.com/goceleris/celeris/middleware/session"

s := session.New(session.Config{
    Store: session.NewMemoryStore(), // returned session.Store
})
```

**After** (v1.5.0):

```go
import (
    "github.com/goceleris/celeris/middleware/session"
    "github.com/goceleris/celeris/middleware/store"
)

s := session.New(session.Config{
    Store: store.NewMemoryKV(), // returns *store.MemoryKV (implements store.KV)
})
```

To keep using the old `session.Store` interface while you migrate, wrap:

```go
s := session.New(session.Config{
    Store: session.StoreFromKV(yourOldStore),
})
```

To move to a distributed backend:

```go
import (
    "github.com/goceleris/celeris/driver/redis"
    "github.com/goceleris/celeris/middleware/session/redisstore"
)

cli, _ := redis.NewClient("127.0.0.1:6379")
s := session.New(session.Config{
    Store: redisstore.New(cli),
})
```

For Postgres-backed sessions (schema auto-created):

```go
import (
    "github.com/goceleris/celeris/driver/postgres"
    "github.com/goceleris/celeris/middleware/session/postgresstore"
)

pool, _ := postgres.Open(dsn)
kv, _ := postgresstore.New(ctx, pool)
s := session.New(session.Config{Store: kv})
```

For memcached-backed sessions:

```go
import (
    celmc "github.com/goceleris/celeris/driver/memcached"
    "github.com/goceleris/celeris/middleware/session/memcachedstore"
)

cli, _ := celmc.NewClient("127.0.0.1:11211")
s := session.New(session.Config{Store: memcachedstore.New(cli)})
```

Note: session data is now JSON-encoded. Numerics arrive as `float64`;
the built-in `GetInt` / `GetInt64` / `GetFloat64` accessors cope with
both forms so most user code needs no change.

### 2. CSRF storage

**Before:**

```go
c := csrf.New(csrf.Config{
    Storage: csrf.NewMemoryStorage(),
})
```

**After:**

```go
c := csrf.New(csrf.Config{
    Storage: store.NewMemoryKV(),
})
```

Or use the Redis / memcached adapters for multi-instance single-use
token validation:

```go
c := csrf.New(csrf.Config{
    Storage: csrfredisstore.New(redisClient), // atomic GETDEL
})
```

Shim available: `csrf.StorageFromKV(oldStorage)`.

### 3. `AsyncHandlers` and driver I/O

If your handlers call DB / cache / upstream services, set
`AsyncHandlers: true` on `celeris.Config`. Doing so unlocks goroutine-
per-request dispatch, which is what `net.Conn.Read` (used by the
direct-mode driver fallback) needs to park cleanly on Go netpoll. The
default (`false`) is optimised for pure-CPU handlers.

```go
srv := celeris.New(celeris.Config{
    Addr: ":8080",
    AsyncHandlers: true,  // <- for DB / cache / upstream-HTTP handlers
})
```

Cost on CPU-only workloads: ~3–5 % (one goroutine spawn per request).
Savings on blocking I/O: up to 6× (5.9 ms → 855 µs measured).

### 4. Ratelimit Redis adapter

No API change, but the adapter now uses `EVALSHA` instead of the in-
memory token bucket. Script load happens in `New()`; `NOSCRIPT` errors
are auto-recovered. If you had a custom `ratelimit.Store`, it continues
to work unchanged.

### 5. JWT JWKS caching

The optional `jwt.Config.JWKSCache` field is new. Pass a
`middleware/store.KV` (via `jwtcache.New` for Redis or
`jwtmccache.New` for memcached) to cache JWKS fetches across
instances. Without it, each process fetches independently (v1.4.0
behaviour).

---

## [1.4.0] — 2026-04-19

Native PostgreSQL, Redis, and Memcached drivers; H2C upgrade;
`EventLoopProvider` API for driver-engine sharing.

(See release notes — historical changelog not backfilled.)
