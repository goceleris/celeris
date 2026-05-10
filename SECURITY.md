# Security Policy

## Supported Versions

| Version       | Supported |
|---------------|-----------|
| >= 1.4.0      | Yes       |
| < 1.4.0       | No        |

Security updates are issued only for the 1.4.x line. Earlier versions
(1.3.x and below) no longer receive fixes, including critical ones —
upgrade to the latest 1.4.x to remain covered.

### v1.4.2 Security Improvements

v1.4.2 ships SSE fan-out (`Broker` + `PreparedEvent` + `ReplayStore`),
WebSocket fan-out (`Hub` + `PreparedMessage`), per-client SSE
`MaxQueueDepth` + `OnSlowClient` policy, an engine-side bind retry
on transient `EADDRINUSE`, and a Go 1.26.3 toolchain bump.
Security-relevant changes:

- **Slow-subscriber DoS posture (SSE)**: `sse.Broker.SubscriberBuffer`
  bounds each subscriber's outbound queue; `sse.BrokerConfig.OnSlowSubscriber`
  decides what happens when a queue fills (`Drop`, `Remove`, or `Close`).
  Default is `Drop` — leaves the stream coherent (the next event's id
  carries enough state for `Last-Event-ID` replay to resync). Per-client
  `sse.Config.MaxQueueDepth` + `OnSlowClient` (`Drop`/`Close`/`Block`)
  sit one layer below for direct `Client.Send` callers. The slow-path
  goroutine fan-out is bounded by `SlowSubscriberConcurrency` so a
  storm of slow callbacks cannot exhaust scheduler resources.

- **Slow-conn DoS posture (WebSocket)**: `websocket.Hub` pairs the
  same model — `OnSlowConn` defaults to `Close` (a dropped WS frame
  would corrupt the message-boundary contract, so eviction is the
  safer default). `MaxConcurrency` caps goroutine pressure on
  fan-out; `Hub.Close` joins every in-flight Broadcast before tearing
  down conns, so shutdown cannot race a still-fanning-out message.
  **Authorization MUST happen before `Hub.Register`**: Hub broadcasts
  go to every registered connection unfiltered. Use
  `Hub.BroadcastFilter` with a pure predicate for per-conn ACL.

- **`PreparedMessage` rejects control opcodes**: control frames are
  bound by RFC 6455 §5.5 to ≤125 bytes and non-fragmented. The
  cache-and-broadcast model can't honor those constraints safely, so
  `NewPreparedMessage` returns `ErrInvalidPreparedOpcode` for
  `OpClose` / `OpPing` / `OpPong`. Use `Conn.WriteControl` per-conn
  for control frames.

- **Replay-store memory bounds (SSE)**: `sse.NewRingBuffer(maxN)` is
  bounded by N. `sse.NewKVReplayStore(KVReplayStoreConfig{TTL: …})`
  is bounded by the supplied TTL — **always set TTL** to prevent
  unbounded growth on a long-running broker. The in-memory ID index
  is also soft-capped via `KVReplayStoreConfig.MaxIndex`. Multi-
  instance ID monotonicity uses `store.Counter` (Redis `INCR`,
  Postgres `RETURNING id`); when the KV doesn't implement Counter,
  the store falls back to a per-process counter and multi-instance
  setups will see ID collisions across instances on reconnect.

- **Engine bind retry on `EADDRINUSE`**: `engine/{epoll,iouring}`
  retry `unix.Bind` up to 9 times with exponential-jittered backoff
  when EADDRINUSE fires on a SO_REUSEPORT-group join. The retry is
  bounded (~½ second worst case) and does not mask a true conflict —
  a persistent EADDRINUSE escapes the budget unchanged with the
  full bind diagnostic attached. Non-EADDRINUSE errors short-circuit
  immediately. The retry only smooths a documented kernel-side bind-
  table race observed when 12+ sockets in a single process race into
  the same SO_REUSEPORT group at startup.

- **Go toolchain bump (1.26.2 → 1.26.3)**: absorbs two stdlib CVEs
  surfaced by govulncheck on the 1.26.2 toolchain:
  - `GO-2026-4971`: panic in `net.Dial` and `net.LookupPort` when
    handling NUL-byte input on Windows.
  - `GO-2026-4918`: infinite loop in `net/http/internal/http2`
    transport on a malformed `SETTINGS_MAX_FRAME_SIZE` from a peer.
  Both are fixed in `net@go1.26.3` / `net/http@go1.26.3`. Every
  go.mod in the repo + the loadgen sub-module pin `go 1.26.3`; CI
  workflows pin the same explicit patch version so a stale runner
  cache cannot regress to 1.26.2.

- **CVE-2023-44487 "HTTP/2 Rapid Reset" mitigation**: unchanged from
  prior versions — `protocol/h2/stream/processor.go::handleRSTStream`
  enforces a sliding 1-second window with a 100 reset/sec rate limit
  and a 200 burst cap; exceeding it triggers GOAWAY with
  `ENHANCE_YOUR_CALM`. The PR does not regress this path; matrix
  spec compliance (`mage spec` / h2spec) re-validates it.

### v1.4.1 Security Improvements

v1.4.1 ships the middleware × driver integration milestone, the dynamic
worker scaler, memcached cluster failover, and a sweep of driver/middleware
hot-path optimizations. Security-relevant changes:

- **`middleware/overload` shedding ladder**: 5-stage controller with CPU
  + queue-depth + tail-latency-EMA signals returns 503 + Retry-After at
  saturation. Designed to keep an overloaded process from cascading into
  total unavailability under DoS-shaped traffic. Defaults are
  conservative; the alloc-budget guard test pins the Normal-stage path
  at zero allocations so the middleware itself cannot become a bottleneck.
- **`middleware/idempotency` lock state machine**: client-supplied
  `Idempotency-Key` header is matched against a `store.SetNXer`-backed
  lock entry; concurrent duplicates while the original is in-flight
  return 409 (no double-execute), and replays serve the cached response
  for the configured TTL. Lock entries are released on handler
  completion or expire after `LockTimeout` so a crashed handler does not
  permanently block the key. Constant-time key comparison via
  `subtle.ConstantTimeCompare`.
- **`middleware/cache` Cache-Control honoring**: by default, response
  `Cache-Control` directives (`no-store`, `private`, `max-age`) cap the
  effective TTL. `Set-Cookie` is excluded from stored headers by default
  to prevent session-cookie leakage between users. `Vary` header
  components are folded into the cache key to prevent cross-user replay
  on shared keys.
- **`middleware/store` unified `KV` interface**: session, csrf, ratelimit,
  cache, idempotency, and JWKS cache all share the same byte-level
  contract. `Prefixed` namespace helper prevents key collisions when a
  single Redis / Postgres instance backs multiple middlewares; without
  it a CSRF token store and a session store could overwrite each other.
  Backed adapters: in-memory LRU (sharded), Redis (`GETDEL` for atomic
  single-use, falls back to `GET+DEL` on Redis < 6.2 via `OldRedisCompat`),
  Postgres (`ON CONFLICT` upsert + background expiry sweep), memcached.
- **`Context.SetHeaderTrust` / `Context.AppendRespHeader`**: new
  fast-path response-header verbs that skip the CRLF / NUL sanitize
  scan. Documented as caller-asserted invariants — used internally by
  `requestid`, `secure`, and `ratelimit` after one-time validation at
  middleware construction. Not for user-supplied input. The full
  `SetHeader` / `AddHeader` verbs continue to sanitize.
- **`auto_cache_statements` default flip**: `Pool.Open` and
  `NewConnector` now default `auto_cache_statements=true` (matching pgx).
  Cached prepared statements are scoped per connection and discarded on
  conn close; no cross-conn leakage. Opt out per DSN with
  `auto_cache_statements=false` for environments that need the
  simple-query semantics (e.g. server-side trigger-driven caches).
- **memcached cluster failover**: `pickNode` successor walk is
  deterministic so a node-down event does not silently route writes to
  a different shard's read-replica. `recordResult` uses a hysteresis
  threshold to prevent thrashing. Stale reads during failover are
  surfaced as transient errors rather than silently served from a
  potentially-stale node.

### v1.4.0 Security Improvements

v1.4.0 introduces the native PostgreSQL, Redis, and memcached drivers
plus H2C upgrade support and the EventLoopProvider plumbing. Security
posture is conservative:

- **TLS not implemented**: `driver/postgres` rejects `sslmode=require /
  verify-ca / verify-full` with `ErrSSLNotSupported` rather than
  silently downgrading. `sslmode=prefer / allow` emit a stderr warning
  before downgrading to plaintext. `driver/redis` rejects `rediss://`
  URLs at `NewClient` time. Until first-class TLS lands, deploy over
  VPC, loopback, or terminate at a sidecar TLS proxy.
- **PG SCRAM-SHA-256 only**: cleartext / MD5 / trust + SCRAM-SHA-256
  (without channel binding) are supported. GSS, SSPI, Kerberos,
  SCRAM-SHA-256-PLUS, and other SASL mechanisms are rejected — no
  silent fallback to a weaker auth path.
- **Driver `WithEngine` deadlock fix**: when an engine's `WorkerLoop`
  does not implement `syncRoundTripper`, drivers fall back to direct
  mode (Go netpoll) instead of deadlocking. Prevents a pool-exhaustion
  DoS on a misconfigured server.
- **H2C upgrade single-token Upgrade enforcement**: RFC 7540 §3.2
  requires `Upgrade: h2c` to be the sole token. Multi-token Upgrade
  headers (e.g. `Upgrade: websocket, h2c`) are disqualified to prevent
  ambiguity attacks against simultaneous WebSocket / H2 negotiation.

## Historical (unsupported)

Per-version security notes for the 1.3.x line and earlier are preserved in the git history of this file (`git log SECURITY.md`). Those releases no longer receive fixes — upgrade to the latest 1.4.x to remain covered.

## Reporting a Vulnerability

If you discover a security vulnerability in celeris, please report it responsibly.

**Do not open a public GitHub issue for security vulnerabilities.**

Instead, please email security@goceleris.dev with:

- A description of the vulnerability
- Steps to reproduce
- Potential impact
- Suggested fix (if any)

We will acknowledge receipt within 48 hours and aim to provide a fix within 7 days for critical issues.

## Scope

This policy covers the core `github.com/goceleris/celeris` module and all in-tree middleware, including:

- HTTP protocol parsing (H1, H2)
- I/O engines (io_uring, epoll, std)
- Request routing, pre-routing middleware (`Server.Pre()`), and context handling
- The net/http bridge adapter
- Response header sanitization (CRLF, null bytes, cookie attributes)
- Connection lifecycle management (Detach, StreamWriter, pool safety)
- Body size enforcement (MaxRequestBodySize across H1, H2, bridge)
- Callback safety (OnExpectContinue, OnConnect, OnDisconnect)
- All in-tree middleware packages (`middleware/`)
- The `middleware/store` unified `KV` substrate plus the bundled adapters
  (in-memory LRU, Redis, Postgres, memcached)
- Native drivers: `driver/postgres`, `driver/redis`, `driver/memcached`
  (wire-protocol parsers, auth handshakes, cluster failover state)
- Sub-module middleware (`middleware/compress`, `middleware/metrics`,
  `middleware/otel`, `middleware/protobuf`)

### Out of Scope

- The deprecated `github.com/goceleris/middlewares` module (archived, retracted)
- Third-party middleware not in this repository
- Application-level vulnerabilities in user code
