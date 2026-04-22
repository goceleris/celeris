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
- **`Context.RequestID()` / `Context.SetRequestID(string)`** — zero-alloc
  dedicated accessor for the request ID (skips the `any`-interface
  boxing that `c.Set(RequestIDKey, id)` used to pay per request).
  `middleware/requestid` now stores via `SetRequestID`; value still
  surfaces through `c.Get(RequestIDKey)` for back-compat.
- **`Context.SetString(key, val)` / `Context.GetString(key)`** — typed
  string storage that avoids the `any` box on both write and read.
  `middleware/{csrf,basicauth,keyauth,requestid}` migrated; chains that
  carry a handful of auth-scoped strings drop one alloc per middleware
  layer. `GetString` falls back to the generic `c.keys` map and to the
  dedicated `RequestID` field, so existing `c.Get(key)` callers still
  observe the same values.

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

- **iouring engine: HTTP/1 Connection:close churn collapse (260× fix).**
  Under sustained HTTP/1 Connection:close traffic against a client that
  pools connections — the canonical production pattern: load balancers,
  service mesh sidecars, HTTP/2 upstream pools fronting an H1 backend —
  the iouring engine throttled to ~90 rps on aarch64 kernel 6.6.10, a
  100–260× collapse vs the epoll engine's ~25 k rps on the identical
  workload. CPU profile showed workers spending 97 % of their time
  asleep in `io_uring_enter`, drowning in spurious recv CQEs (25 k/s
  per worker against ~50 useful completions — a 500 : 1 waste ratio)
  from the ring-mapped provided-buffer multishot recv path. Fix:
  switch the default recv model to single-shot per-connection recv
  (the same model `engine/epoll` uses); opt back in to multishot via
  `CELERIS_IOURING_MULTISHOT_RECV=1` for workloads genuinely dominated
  by long-lived keep-alive connections. Measured delta on
  `mini@msr1`: `celeris-iouring-*-h1 churn` = 0.1 k → 24 k rps
  (+21 000 %); median across all 180 other benchmark × engine ×
  protocol combinations is within ±5 % of the pre-fix matrix.
- **iouring engine: multishot accept now re-arms on CQE_F_MORE=0.**
  Previously the re-arm was gated on `!SupportsMultishotAccept`, so a
  kernel-terminated multishot (backpressure or error path) would
  silently stop the worker from accepting new connections. Now fires
  in both paths.
- **iouring engine: synchronous close replaces the async CLOSE SQE.**
  The async path left the fd open across `io_uring_enter` boundaries,
  compounding with the server-initiated FIN_WAIT_2 pileup to wedge the
  ephemeral port pool under churn. `unix.Close` on a non-blocking
  socket is a pure descriptor-table op and does not stall the event
  loop. Detached (WS/SSE) conns keep the graceful half-close + drain
  + close path because middleware may have queued close-frame bytes
  that the RST path would cut off.
- **iouring engine: ACCEPT_DIRECT runtime probe.** The previous probe
  only verified `IORING_REGISTER_FILES` succeeded, but kernel
  6.6.10-cix accepts the file-table registration and then fails the
  actual `ACCEPT_DIRECT` SQE with EINVAL at runtime. Every worker
  paid the cold-fallback cost on its first accept, and the ring
  stayed in a mixed "files-registered-but-unused" state. The probe
  now submits a real multishot-accept-direct against a scratch
  listen socket and checks the CQE res; an EINVAL response marks
  fixed-files unsupported at start-up so the engine takes the plain
  fd path cleanly.
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
- **H2 parser enforced stale 16 KiB `MaxReadFrameSize` while advertising
  up to 1 MiB.** `NewH2State` never raised the parser ceiling after
  `InitReader`, so any DATA frame > 16 384 bytes hit
  `FRAME_SIZE_ERROR` and the connection was silently dropped. Visible
  as 100 % error rate on any H2 POST body > 16 KiB from compliant
  clients (loadgen, curl, x/net/http2 when the peer chose large frames).
  Fix: `p.SetMaxReadFrameSize(cfg.MaxFrameSize)` right after InitReader
  with a 16 KiB-minimum clamp for direct `H2Config` callers.
- **H2 default `SETTINGS_MAX_FRAME_SIZE` raised 16 KiB → 1 MiB** to match
  `golang.org/x/net/http2` and fasthttp2. The prior default rejected
  well-behaved clients that pre-negotiate their send chunk size.
- **H2 default `SETTINGS_INITIAL_WINDOW_SIZE` raised 65 535 → 1 MiB.**
  A 64 KiB + 1-byte body POST over H2 deadlocked on the old window: the
  client blocked waiting for WINDOW_UPDATE while the server waited to
  read the full body before sending it. 1 MiB matches std http2.
- **H1 `SO_RCVBUF`/`SO_SNDBUF` default → 0 (kernel auto-tune).** Forcing
  256 KiB disabled Linux TCP auto-tuning — on hosts where
  `net.core.rmem_max < 256 KiB` (msr1 default: 208 KiB) the socket was
  capped at the lower of the two, throttling 1 MiB POST throughput by
  ~10 % vs fiber/fasthttp. Users who need a hard cap can still set
  `Resources.SocketRecv/SocketSend` explicitly.
- **H1 zero-copy request body path.** Previously every POST copied the
  raw body slice from the engine's read buffer into a `*bytes.Buffer`
  (`s.Data`) before the handler saw it. New `Stream.SetRawBody` +
  `Stream.rawBody` install a zero-copy view that `GetData` / `Body` /
  `BodyCopy` return directly. `BodyCopy` remains safe for retention.
- **H1 dedicated `bodyBuf` for partial-body multi-read POSTs.** A 1 MiB
  body accumulated in 128 × 8 KiB Writes used to pay ~2 MiB of
  userspace memcpy to `state.buffer` (`*bytes.Buffer` doubling grow).
  New `H1State.bodyBuf` + `bodyNeeded` + the engine-side
  `NextRecvBuf` / `ConsumeBodyRecv` / `DispatchBufferedBody` hooks land
  body bytes straight into a correctly-sized slice, skipping the
  `cs.buf → state.buffer` memcpy. Active only in sync-handler mode
  (async handler path still goes through `asyncInBuf`).
- **Combined impact on msr1:**
  - H1 POST 4 K: **+9.6 %** vs fiber (loss → win).
  - H1 POST 64 K: gap −23 % → −3 % vs fiber.
  - H1 POST 1 M: gap −43 % → −4 % vs fiber.
  - H2 POST 64 K: **+63 %** vs chi (previously "0 rps due to H2 bug").
  - H2 GET: **+83 % / +91 %** over the next-best competitor.

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

#### Context / hot-path micro-optimizations (103-round perf loop)

Byte-identical output, zero behavioural change, hot paths only. All
gains measured on `mini@msr1`, ≥3 % deltas kept, null rounds excluded.

- **`c.JSON()` reflection-free fast path** — primitive scalars, strings,
  `[]string`, `[]int/int64/uint64/bool`, `map[string]string`, and
  `map[string]any` payloads up to 16 keys bypass `encoding/json`
  entirely. Keys are pre-sorted (stack-allocated `[16]string` +
  insertion sort) so output is byte-identical to `json.Marshal` with
  `SetEscapeHTML(false)`. ASCII strings take a pure-copy path; strings
  with escapes are handled inline; floats use stdlib's exact branching
  rule (`'f'` in `[1e-6, 1e21)`, `'e'` otherwise with exponent
  cleanup). Complex or nested shapes fall through to `encoding/json`.
  Per-request alloc drop on JSON responses ranges from 2 allocs on
  `{"ok":true}` to 5+ allocs on mid-size structured maps.
- **`c.Query(key)` zero-alloc scan** — scans `rawQuery` directly with
  byte arithmetic, skipping the `url.ParseQuery` map allocation for
  the common case of one-shot lookups. `QueryParams()` still parses
  into a `url.Values` the first time it's called.
- **Context retention wins** — `capturedBody` backing array kept
  across pool reuse (R82); `requestID` / `stringKeys` dedicated fields
  skip `any`-boxing (R85–R87); `parseForm` short-circuits on non-form
  requests (R88); stack `[]byte` builder for `SetCookie` instead of
  `fmt.Fprintf` (R46).
- **Driver tightening** — `driver/postgres` pools `Rows`,
  `[]driver.NamedValue` slabs, encoded-args scratch, and direct-mode
  payload buffers (R47–R50); `driver/memcached` routes `asBytes` ints
  through `strconv.Append*`, elides cluster wrapper allocs, and fixes a
  `Pipeline` escape (R16, R19, R39); `driver/redis` prebuilds argument
  arrays for the EVALSHA path and adopts `GetDelBytes` (R37, R40).
- **Middleware tightening** — `middleware/jwt` `classifiedError`
  replaces `fmt.Errorf("%w: %w", ...)` (4 allocs → 1) and releases
  `MapClaims` on every error path (R71, R72, R76); `middleware/cache`
  gets a single-param `sortedQuery` fast path, a default key generator
  fast path, and reuses existing backing arrays in `MemoryStore.Set`
  (R61, R65, R79); `middleware/session` pools `sessionDataPool` and
  returns fresh maps to it (R44, R53, R63); `middleware/healthcheck`
  detects the default always-true checker config and skips the
  per-request fan-out goroutine allocation (R89); `middleware/static`
  pre-formats headers and caches per-file ETag/Last-Modified (R41,
  R67, R70); `middleware/singleflight` pools `*call` entries for the
  no-waiter path and skips capture when no one is waiting (R73, R74,
  R78); `middleware/{cache,idempotency,requestid,methodoverride}`
  pre-lowercase configured header names so `c.Header`'s fast path
  fires without allocating per request (R42, R52, R59, R60); many
  store adapters pool the `prefix+key` buffer used on every call
  (R55–R57).
- **`HTTPError.Error()`** — stack-buffer concat replaces
  `fmt.Sprintf` in the hot error-formatting path (R69).

Running total across the 103-round loop:
- 60+ committed wins (40+ null/investigation rounds excluded).
- No behavioural changes; existing tests, fuzz suites, and
  `h1spec`/`h2spec` continue to pass on both x86_64 and arm64.
- Back-compat preserved: `c.Get(RequestIDKey)` still returns the
  request ID; `c.Get(key)` still returns values set via
  `c.SetString(key, ...)`.

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
