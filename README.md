# celeris

[![CI](https://github.com/goceleris/celeris/actions/workflows/ci.yml/badge.svg)](https://github.com/goceleris/celeris/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/goceleris/celeris.svg)](https://pkg.go.dev/github.com/goceleris/celeris)
[![Go Report Card](https://goreportcard.com/badge/github.com/goceleris/celeris)](https://goreportcard.com/report/github.com/goceleris/celeris)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

Ultra-low latency Go HTTP engine with a protocol-aware dual-architecture (io_uring & epoll) designed for high-throughput infrastructure and zero-allocation microservices. It provides a familiar route-group and middleware API similar to Gin and Echo, so teams can adopt it without learning a new programming model.

## Highlights

- **io_uring and epoll at parity** — both engines deliver equivalent throughput
- **Zero hot-path allocations** for both H1 and H2
- **Designed for throughput** — see [benchmarks](https://goceleris.dev/benchmarks) for current numbers

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
- **Response buffering** — `BufferResponse`/`FlushResponse` for transform middleware (compress, ETag, cache)
- **File serving** — `File()` from OS, `FileFromFS()` from `embed.FS` / `fs.FS`
- **Content negotiation** — `Negotiate`, `Respond`, `AcceptsEncodings`, `AcceptsLanguages`
- **Configurable body limits** — `MaxRequestBodySize` enforced across H1, H2, and the bridge
- **100-continue control** — `OnExpectContinue` callback for upload validation before body transfer
- **Accept control** — `PauseAccept()`/`ResumeAccept()` for graceful load shedding
- **Zero-downtime restart** — `InheritListener` + `StartWithListener` for socket inheritance
- **Built-in metrics** — atomic counters, CPU utilization sampling, always-on `Server.Collector().Snapshot()`

## Quick Start

```
go get github.com/goceleris/celeris@latest
```

Requires **Go 1.26+**. Linux for io_uring/epoll engines; any OS for the std engine.

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
| [`circuitbreaker`](middleware/circuitbreaker) | Circuit breaker (3-state, sliding window error rate, 503 + Retry-After) |
| [`compress`](middleware/compress) | Response compression (zstd, brotli, gzip; separate go.mod) |
| [`cors`](middleware/cors) | Cross-Origin Resource Sharing (zero-alloc) |
| [`csrf`](middleware/csrf) | CSRF protection (double-submit cookie + origin validation) |
| [`debug`](middleware/debug) | Debug/introspection endpoints (loopback-only by default) |
| [`etag`](middleware/etag) | Automatic ETag generation and conditional 304 responses |
| [`healthcheck`](middleware/healthcheck) | Kubernetes-style liveness/readiness/startup probes |
| [`jwt`](middleware/jwt) | JWT authentication (HMAC/RSA/ECDSA/EdDSA, JWKS auto-refresh) |
| [`keyauth`](middleware/keyauth) | API key authentication with constant-time comparison |
| [`logger`](middleware/logger) | Structured request logging (slog, zero-alloc FastHandler) |
| [`methodoverride`](middleware/methodoverride) | HTTP method override via header or form field |
| [`metrics`](middleware/metrics) | Prometheus metrics (separate go.mod) |
| [`otel`](middleware/otel) | OpenTelemetry tracing + metrics (separate go.mod) |
| [`pprof`](middleware/pprof) | Go profiling endpoints (loopback-only by default) |
| [`proxy`](middleware/proxy) | Trusted proxy header extraction (X-Forwarded-For, X-Real-IP) |
| [`ratelimit`](middleware/ratelimit) | Sharded token bucket / sliding window rate limiter |
| [`recovery`](middleware/recovery) | Panic recovery with broken pipe detection |
| [`redirect`](middleware/redirect) | URL redirect/rewrite (HTTPS, www, trailing slash) |
| [`requestid`](middleware/requestid) | Request ID generation (buffered UUID v4) |
| [`rewrite`](middleware/rewrite) | Regex-based URL rewriting with capture group support |
| [`secure`](middleware/secure) | Security headers (HSTS, CSP, COOP/CORP/COEP, OWASP defaults) |
| [`session`](middleware/session) | Cookie-based sessions with pluggable store |
| [`singleflight`](middleware/singleflight) | Request coalescing (collapse identical in-flight requests) |
| [`static`](middleware/static) | Static file serving with directory browse, ETag/Last-Modified caching |
| [`swagger`](middleware/swagger) | OpenAPI spec + Swagger UI/Scalar (CDN-loaded) |
| [`timeout`](middleware/timeout) | Request timeout with cooperative and preemptive modes |

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
	Logger:             slog.Default(),
})
```

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

The bridge buffers the adapted handler's response in memory, capped at `MaxRequestBodySize` (default 100 MB). Responses exceeding this limit return an error.

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
| Multishot recv | yes (6.0+) | no | no |
| Zero-alloc HEADERS | yes | yes | no |
| Inline H2 handlers | yes | yes | no |
| Detach / StreamWriter | yes | yes | yes |
| Connection hijack | yes | yes | yes |

## Benchmarks

For current benchmark results and methodology, see [goceleris.dev/benchmarks](https://goceleris.dev/benchmarks).

Middleware comparison benchmarks are in [`test/benchcmp/`](test/benchcmp/) (Celeris vs Fiber v3 vs Echo v4 vs Chi v5 vs stdlib). Run with `mage middlewareBenchmark`.

> **Note:** In-tree middleware benchmarks (e.g., `middleware/compress/bench_test.go`) use `celeristest` which provides pool-based contexts with no HTTP overhead. These numbers measure pure middleware logic and should not be compared directly with `httptest`-based competitor benchmarks. Use `test/benchcmp/` for fair cross-framework comparisons.

## Project Structure

```
adaptive/       Adaptive meta-engine (Linux)
celeristest/    Test helpers (NewContext, ResponseRecorder)
engine/         Engine interface + implementations (iouring, epoll, std)
internal/       Shared internals (conn, cpumon, ctxkit, negotiate, platform, sockopts)
middleware/     In-tree middleware ecosystem (29 packages)
observe/        Metrics collector, CPUMonitor, Snapshot
probe/          System capability detection
protocol/       Protocol parsers (h1, h2, detect)
resource/       Configuration, presets, defaults
test/           Conformance, spec compliance, integration, benchmarks
```

## Requirements

- **Go 1.26+**
- **Linux** for io_uring and epoll engines
- **Any OS** for the std engine
- Dependencies: `golang.org/x/sys`, `golang.org/x/net`

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for development setup, testing, and pull request guidelines.

## License

[Apache License 2.0](LICENSE)
