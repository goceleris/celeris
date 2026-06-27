# celeris

[![CI](https://github.com/goceleris/celeris/actions/workflows/ci.yml/badge.svg)](https://github.com/goceleris/celeris/actions/workflows/ci.yml)
[![Nightly Validation](https://github.com/goceleris/probatorium/actions/workflows/matrix-nightly-tier.yml/badge.svg?branch=main)](https://github.com/goceleris/probatorium/actions/workflows/matrix-nightly-tier.yml?query=branch%3Amain)
[![Weekend Soak](https://github.com/goceleris/probatorium/actions/workflows/matrix-weekend-tier.yml/badge.svg?branch=main)](https://github.com/goceleris/probatorium/actions/workflows/matrix-weekend-tier.yml?query=branch%3Amain)
[![Go Reference](https://pkg.go.dev/badge/github.com/goceleris/celeris.svg)](https://pkg.go.dev/github.com/goceleris/celeris)
[![Go Report Card](https://goreportcard.com/badge/github.com/goceleris/celeris)](https://goreportcard.com/report/github.com/goceleris/celeris)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

celeris is a high-throughput, load-bearing Go HTTP engine built on a protocol-aware dual-architecture (io_uring & epoll). Its standout strength is **best-in-class throughput under load** — across the [probatorium](https://github.com/goceleris/probatorium) cross-framework matrix it leads the field on driver-backed (PostgreSQL / Redis / memcached) and write-heavy workloads, sustaining the highest request rates while holding tail-latency SLOs. It provides a familiar route-group and middleware API similar to Gin and Echo, so teams can adopt it without learning a new programming model, with zero-allocation hot paths on the H1 / H2 fast paths.

## Highlights

- **io_uring and epoll at parity** — both engines deliver equivalent throughput
- **Zero hot-path allocations** on the H1 / H2 fast paths (pool-based contexts, pre-encoded HPACK responses)
- **Continuously validated** — a [probatorium](https://github.com/goceleris/probatorium) 48-cell adversarial matrix runs nightly + a deeper weekend soak; see the badges above

## Features

- **Tiered io_uring** — auto-selects the best io_uring feature set (multishot accept/recv, provided buffers, SQ poll, fixed files) for your kernel
- **Edge-triggered epoll** — per-core event loops with CPU pinning
- **Adaptive meta-engine** — dynamically switches between io_uring and epoll based on runtime telemetry
- **SIMD HTTP parser** — SSE2 (amd64) and NEON (arm64) with generic SWAR fallback
- **HTTP/2 cleartext (h2c)** — full stream multiplexing, flow control, HPACK, inline handler execution, zero-alloc HEADERS fast path
- **Auto-detect** — protocol negotiation from the first bytes on the wire
- **Error-returning handlers** — `HandlerFunc` returns `error`; structured `HTTPError` for status codes
- **Pre-routing middleware** — `Server.Pre()` runs middleware before route matching (method override, URL rewrite)
- **Serialization** — JSON and XML response methods; `Bind` auto-detects request format from Content-Type
- **net/http compatibility** — wrap existing `http.Handler` via `celeris.Adapt()`
- **Streaming responses** — `Detach()` + `StreamWriter` for SSE and chunked responses on native engines
- **Connection hijacking** — `Hijack()` for WebSocket and custom protocol upgrades (H1 only)
- **Engine-integrated WebSocket** — `UpgradeWebSocket()` hands the conn directly to the worker loop with backpressure, deflate, and a `Hub` for fan-out broadcast
- **Response buffering** — `BufferResponse`/`FlushResponse` for transform middleware (compress, ETag, cache)
- **File serving** — `File()` from OS, `FileFromFS()` from `embed.FS` / `fs.FS`
- **Content negotiation** — `Negotiate`, `Respond`, `AcceptsEncodings`, `AcceptsLanguages`
- **Configurable body limits** — `MaxRequestBodySize` enforced on H1 and H2 (the net/http bridge has a fixed 100 MB cap)
- **100-continue control** — `OnExpectContinue` callback for upload validation before body transfer
- **Accept control** — `PauseAccept()`/`ResumeAccept()` for graceful load shedding
- **Zero-downtime restart** — `InheritListener` + `StartWithListener` for socket inheritance
- **Built-in metrics** — atomic counters, CPU utilization sampling, on-by-default `Server.Collector().Snapshot()` (opt out via `Config.DisableMetrics`)
- **Per-handler async dispatch** — `Route.Async()` / `Route.Sync()` choose inline-on-worker vs. per-conn dispatch goroutine per route; H2 chooses per-stream

**TLS:** the io_uring/epoll engines speak cleartext only (HTTP/1.1 + h2c). Terminate TLS upstream (Caddy/Nginx/Envoy) or use the std engine if you need in-process HTTPS.

## Quick Start

```
go get github.com/goceleris/celeris@latest
```

Requires **Go 1.26.3+**. Linux for io_uring/epoll engines; any OS for the std engine.

## Hello World

```go
package main

import (
	"log"

	"github.com/goceleris/celeris"
)

func main() {
	s := celeris.New(celeris.Config{Addr: ":8080"})
	s.GET("/hello", func(c *celeris.Context) error {
		return c.String(200, "Hello, World!")
	})
	log.Fatal(s.Start())
}
```

## Routing

```go
s := celeris.New(celeris.Config{Addr: ":8080"})

// Static routes
s.GET("/health", healthHandler)

// Named parameters
s.GET("/users/:id", func(c *celeris.Context) error {
	id := c.Param("id")
	return c.JSON(200, map[string]string{"id": id})
})

// Catch-all wildcards
s.GET("/files/*path", staticFileHandler)

// Route groups with middleware
api := s.Group("/api")
api.Use(authMiddleware)
api.GET("/items", listItems)
api.POST("/items", createItem)

// Named routes + reverse URL generation
s.GET("/users/:id", showUser).Name("user")
url, _ := s.URL("user", "42") // "/users/42"

// Pre-routing middleware (runs before route matching)
s.Pre(methodOverride, urlRewrite)
```

## Middleware

All middleware is in-tree under [`middleware/`](middleware/):

| Package | Description |
|---------|-------------|
| [`adapters`](middleware/adapters) | Bidirectional stdlib ↔ celeris middleware/handler conversion |
| [`basicauth`](middleware/basicauth) | HTTP Basic authentication with hashed password support |
| [`bodylimit`](middleware/bodylimit) | Request body size enforcement |
| [`cache`](middleware/cache) | HTTP response cache with singleflight + Cache-Control honoring |
| [`circuitbreaker`](middleware/circuitbreaker) | Circuit breaker (3-state, sliding window error rate, 503 + Retry-After) |
| [`compress`](middleware/compress) | Response compression (zstd, brotli, gzip, deflate; separate go.mod) |
| [`cors`](middleware/cors) | Cross-Origin Resource Sharing (zero-alloc) |
| [`csrf`](middleware/csrf) | CSRF protection (double-submit cookie + origin validation) |
| [`debug`](middleware/debug) | Debug/introspection endpoints (loopback-only by default) |
| [`etag`](middleware/etag) | Automatic ETag generation and conditional 304 responses |
| [`healthcheck`](middleware/healthcheck) | Kubernetes-style liveness/readiness/startup probes |
| [`idempotency`](middleware/idempotency) | Idempotency-Key replay protection (state machine: in-flight 409 + cached replay) |
| [`jwt`](middleware/jwt) | JWT authentication (HMAC/RSA/ECDSA/EdDSA, JWKS auto-refresh) |
| [`keyauth`](middleware/keyauth) | API key authentication with constant-time comparison |
| [`logger`](middleware/logger) | Structured request logging (slog, zero-alloc FastHandler) |
| [`methodoverride`](middleware/methodoverride) | HTTP method override via header or form field |
| [`metrics`](middleware/metrics) | Prometheus metrics (separate go.mod) |
| [`otel`](middleware/otel) | OpenTelemetry tracing + metrics (separate go.mod) |
| [`overload`](middleware/overload) | 5-stage CPU + queue-depth + tail-latency-EMA overload control (503 + Retry-After) |
| [`pprof`](middleware/pprof) | Go profiling endpoints (loopback-only by default) |
| [`protobuf`](middleware/protobuf) | Protobuf serialization with content negotiation (separate go.mod) |
| [`proxy`](middleware/proxy) | Trusted proxy header extraction (X-Forwarded-For, X-Real-IP) |
| [`ratelimit`](middleware/ratelimit) | Sharded token bucket / sliding window rate limiter (Redis store adapter) |
| [`recovery`](middleware/recovery) | Panic recovery with broken pipe detection |
| [`redirect`](middleware/redirect) | URL redirect/rewrite (HTTPS, www, trailing slash) |
| [`requestid`](middleware/requestid) | Request ID generation (buffered UUID v4) |
| [`rewrite`](middleware/rewrite) | Regex-based URL rewriting with capture group support |
| [`secure`](middleware/secure) | Security headers (HSTS, CSP, COOP/CORP/COEP, OWASP defaults) |
| [`session`](middleware/session) | Cookie-based sessions on the unified [`store.KV`](middleware/store) (memory / Redis / Postgres / memcached adapters) |
| [`singleflight`](middleware/singleflight) | Request coalescing (collapse identical in-flight requests) |
| [`sse`](middleware/sse) | Server-Sent Events: heartbeat, Last-Event-ID resumption with pluggable replay store (in-memory ring buffer or KV-backed for cross-restart durability), per-client `MaxQueueDepth` + `OnSlowClient` policy (Drop/Close/Block), and a `Broker` for fan-out to N subscribers with per-subscriber bounded queues + `OnSlowSubscriber` policy (Drop/Remove/Close) |
| [`static`](middleware/static) | Static file serving with directory browse, ETag/Last-Modified caching |
| [`store`](middleware/store) | Unified in-memory `KV` (LRU); shared by session / csrf / ratelimit / cache / idempotency / jwt JWKS. Redis / Postgres / memcached adapters live under [`session/redisstore`](middleware/session/redisstore), [`session/postgresstore`](middleware/session/postgresstore), [`session/memcachedstore`](middleware/session/memcachedstore), plus [`ratelimit/redisstore`](middleware/ratelimit/redisstore) / [`ratelimit/memcachedstore`](middleware/ratelimit/memcachedstore) |
| [`swagger`](middleware/swagger) | OpenAPI spec + Swagger UI / Scalar / ReDoc (CDN-loaded) |
| [`timeout`](middleware/timeout) | Request timeout with cooperative and preemptive modes |
| [`websocket`](middleware/websocket) | RFC 6455 WebSocket: permessage-deflate, engine-integrated backpressure, plus a `Hub` for fan-out broadcast to N connections (via cached `PreparedMessage` for O(1) per-message wire-encoding cost) with `OnSlowConn` policy (Drop/Remove/Close) and per-Conn filter via `BroadcastFilter` |

```go
import (
	"github.com/goceleris/celeris/middleware/cors"
	"github.com/goceleris/celeris/middleware/logger"
	"github.com/goceleris/celeris/middleware/recovery"
)

s := celeris.New(celeris.Config{Addr: ":8080"})
s.Use(recovery.New())
s.Use(logger.New())
s.Use(cors.New())
```

For middleware with external dependencies, use separate imports:

```go
import "github.com/goceleris/celeris/middleware/metrics" // requires prometheus
import "github.com/goceleris/celeris/middleware/otel"    // requires opentelemetry
```

## Error Handling

`HandlerFunc` has the signature `func(*Context) error`. Returning a non-nil error propagates it up through the middleware chain. If no middleware handles the error, the router's safety net converts it to an HTTP response:

- `*HTTPError` — responds with `Code` and `Message` from the error.
- Any other `error` — responds with `500 Internal Server Error`.

```go
// Return a structured HTTP error
s.GET("/item/:id", func(c *celeris.Context) error {
	item, err := store.Find(c.Param("id"))
	if err != nil {
		return celeris.NewHTTPError(404, "item not found").WithError(err)
	}
	return c.JSON(200, item)
})

// Use Status() + StatusJSON() for fluent responses
s.POST("/items", func(c *celeris.Context) error {
	var item Item
	if err := c.Bind(&item); err != nil {
		return celeris.NewHTTPError(400, "invalid body").WithError(err)
	}
	created := store.Create(item)
	return c.Status(201).StatusJSON(created)
})
```

## Configuration

```go
s := celeris.New(celeris.Config{
	Addr:               ":8080",
	Protocol:           celeris.Auto,       // HTTP1, H2C, or Auto
	Engine:             celeris.Adaptive,    // IOUring, Epoll, Adaptive, or Std
	Workers:            8,
	ReadTimeout:        30 * time.Second,
	WriteTimeout:       30 * time.Second,
	IdleTimeout:        120 * time.Second,
	ShutdownTimeout:    10 * time.Second,    // max wait for in-flight requests (default 30s)
	MaxRequestBodySize: 50 << 20,            // 50 MB (default 100 MB, -1 for unlimited)
	AsyncHandlers:      false,               // server-level default (per-route .Async() overrides)
	Logger:             slog.Default(),
})
```

## Async Handlers (per-route)

Celeris runs every handler **inline on the I/O worker** by default — lowest latency, zero handoff. For handlers that block on I/O (database, RPC, file system) you can opt **per-route** into the per-connection dispatch goroutine, so the worker stays free to drive other connections:

```go
// CPU-only / cache-only — runs inline (default).
s.GET("/healthz", healthHandler)

// Blocking I/O — async, runs on per-conn goroutine.
s.GET("/db", dbHandler).Async()

// Or flip the default at the group level:
api := s.Group("/api").Async()
api.GET("/products", productHandler)        // async (inherited)
api.GET("/cached", cachedHandler).Sync()    // opt back to sync
```

Precedence is **route > group > server default** (`Config.AsyncHandlers`). Works identically across iouring, epoll, and adaptive (both sub-engines honor the per-route flag and async promotions survive sub-engine swaps): the conn is promoted **once** via the `ErrAsyncDispatch` sentinel (sticky — subsequent requests skip the inline check entirely); H2 routes the choice **per stream** between inline-on-event-loop and the shared H2 worker pool (`runtime.GOMAXPROCS*4` goroutines). The `Async`/`Sync` distinction is a no-op on the `std` engine (net/http already does goroutine-per-request).

Observe how often the inline → goroutine handoff fires via `Server.EngineInfo().Metrics.AsyncRoutes` (static count of `.Async()` routes) and `.AsyncPromotedConns` (cumulative promotions).

## net/http Compatibility

Wrap existing `net/http` handlers and middleware:

```go
// Wrap http.Handler
s.GET("/legacy", celeris.Adapt(legacyHandler))

// Wrap http.HandlerFunc
s.GET("/func", celeris.AdaptFunc(func(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("from stdlib"))
}))
```

The bridge buffers the adapted handler's response in memory, capped at a compile-time **100 MB** limit (independent of `Config.MaxRequestBodySize`). Responses exceeding this limit return an error.

## Engine Selection

| Engine | Platform | Use Case |
|--------|----------|----------|
| `IOUring` | Linux 5.10+ | Lowest latency, highest throughput |
| `Epoll` | Linux | Broad kernel support, proven stability |
| `Adaptive` | Linux | Auto-switch based on telemetry |
| `Std` | Any OS | Development, compatibility, non-Linux deploys |

Use Adaptive (the default on Linux) unless you have a specific reason to pin an engine. On non-Linux platforms, only Std is available.

## Graceful Shutdown

Use `StartWithContext` for production deployments. When the context is canceled, the server drains in-flight requests up to `ShutdownTimeout` (default 30s).

```go
ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
defer stop()

s := celeris.New(celeris.Config{
	Addr:            ":8080",
	ShutdownTimeout: 15 * time.Second,
})
s.GET("/hello", helloHandler)

if err := s.StartWithContext(ctx); err != nil {
	log.Fatal(err)
}
```

## Observability

The core provides a lightweight metrics collector accessible via `Server.Collector()`:

```go
snap := server.Collector().Snapshot()
fmt.Println(snap.RequestsTotal, snap.ErrorsTotal, snap.ActiveConns, snap.CPUUtilization)
```

For Prometheus exposition and debug endpoints, use the [`middleware/metrics`](middleware/metrics) and [`middleware/debug`](middleware/debug) packages. For OpenTelemetry, use [`middleware/otel`](middleware/otel).

## Feature Matrix

| Feature | io_uring | epoll | std |
|---------|----------|-------|-----|
| HTTP/1.1 | yes | yes | yes |
| H2C | yes | yes | yes |
| Auto-detect | yes | yes | yes |
| CPU pinning | yes | yes | no |
| Provided buffers | yes (5.19+) | no | no |
| Multishot accept | yes (5.19+) | no | no |
| Multishot recv | opt-in (5.19+, `CELERIS_IOURING_MULTISHOT_RECV=1`) | no | no |
| Provided-buffer ring size | auto-scaled (`CELERIS_IOURING_PBUF_COUNT=N` to override) | n/a | n/a |
| Zero-alloc HEADERS | yes | yes | no |
| Inline H2 handlers | yes | yes | no |
| Detach / StreamWriter | yes | yes | yes |
| Connection hijack | yes | yes | yes |

## Benchmarks

Cross-framework performance benchmarks live in [`test/perfmatrix/`](test/perfmatrix/) — a (scenario × server × protocol) matrix driven by [`goceleris/loadgen`](https://github.com/goceleris/loadgen). Run the full sweep with `mage matrixBench` or the dev-loop subset with `mage matrixBenchQuick`. Driver-isolation benchmarks remain in [`test/drivercmp/`](test/drivercmp/) and WebSocket comparisons in [`test/benchcmp_ws/`](test/benchcmp_ws/). Reproducible release-gate numbers are published by [probatorium](https://github.com/goceleris/probatorium)'s `publish-results` workflow.

> **Note:** In-tree middleware benchmarks (e.g., `middleware/compress/bench_test.go`) use `celeristest` which provides pool-based contexts with no HTTP overhead. These numbers measure pure middleware logic and should not be compared directly with `httptest`-based competitor benchmarks. Use `test/perfmatrix/` for fair cross-framework comparisons.

## Continuous Validation

Correctness is validated by [goceleris/probatorium](https://github.com/goceleris/probatorium) — an adversarial cluster matrix run on real hardware:

- **PR tier** (`matrix-pr-tier`) — fast gate that every celeris PR must pass before merge.
- **Nightly** (`matrix-nightly-tier`) — 48 cells × every engine × every protocol × refapp; ~7h, every night.
- **Weekend soak** (`matrix-weekend-tier`) — deeper run with the same matrix at higher request counts; multi-hour endurance.

Bug oracles include slowloris hang detection, malformed-request acceptance, WebSocket torture-frame acceptance, h2c churn-crash detection, and tier-3 property seeds. Badge status above.

## Project Structure

```
adaptive/       Adaptive meta-engine (Linux)
celeristest/    Test helpers (NewContext, NewContextT, ResponseRecorder, With* options)
cmd/            Celeris CLI binary (validation / diagnostics entrypoint)
driver/         First-party event-loop database drivers (postgres, redis, memcached)
engine/         Engine interface + implementations (iouring, epoll, std)
internal/       Shared internals (conn, cpumon, ctxkit, negotiate, platform, sockopts)
middleware/     In-tree middleware ecosystem (36 packages)
observe/        Collector, CPUMonitor, Snapshot
probe/          System capability detection (kernel version, io_uring feature probe)
protocol/       Protocol parsers (h1, h2, detect)
resource/       Configuration, presets, defaults
test/           Conformance, spec compliance, integration, benchmarks (perfmatrix, drivercmp, benchcmp_ws, …)
validation/     Runtime invariant assertions + validation hooks (debug builds)
```

## Requirements

- **Go 1.26.3+**
- **Linux** for io_uring and epoll engines (kernel 5.10+ for io_uring; 5.19+ for the multishot / provided-buffers tier)
- **Any OS** for the std engine
- Direct runtime dependencies: `golang.org/x/sys`, `golang.org/x/net` (only)

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for development setup, testing, and pull request guidelines.

## License

[Apache License 2.0](LICENSE)
