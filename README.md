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
- **Serialization** — JSON and XML response methods (`JSON`, `XML`); Protocol Buffers available via [`middleware/protobuf`](middleware/protobuf); `Bind` auto-detects request format from Content-Type
- **net/http compatibility** — wrap existing `http.Handler` via `celeris.Adapt()`
- **Streaming responses** — `Detach()` + `StreamWriter` for SSE and chunked responses on native engines
- **Connection hijacking** — `Hijack()` for WebSocket and custom protocol upgrades (H1 only)
- **Configurable body limits** — `MaxRequestBodySize` enforced across H1, H2, and the bridge
- **100-continue control** — `OnExpectContinue` callback for upload validation before body transfer
- **Accept control** — `PauseAccept()`/`ResumeAccept()` for graceful load shedding
- **Content negotiation** — `Negotiate`, `Respond`, `AcceptsEncodings`, `AcceptsLanguages`
- **Response buffering** — `BufferResponse`/`FlushResponse` for transform middleware (compress, ETag)
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

// Route groups
api := s.Group("/api")
api.GET("/items", listItems)
api.POST("/items", createItem)

// Nested groups
v2 := api.Group("/v2")
v2.GET("/items", listItemsV2)
```

## Middleware

Middleware is provided by the in-tree [`middleware/`](middleware/) packages (e.g. `middleware/logger`, `middleware/recovery`). See the [middleware package docs](middleware/) for usage, examples, and the full list of available middleware.

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

// Middleware can intercept errors from downstream handlers
func ErrorLogger() celeris.HandlerFunc {
	return func(c *celeris.Context) error {
		err := c.Next()
		if err != nil {
			slog.Error("handler error", "path", c.Path(), "error", err)
		}
		return err
	}
}
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

For CPU utilization tracking, register a monitor:

```go
mon, _ := observe.NewCPUMonitor()
server.Collector().SetCPUMonitor(mon)
defer mon.Close()
```

For Prometheus exposition and debug endpoints, use the [`middleware/metrics`](middleware/metrics) and [`middleware/debug`](middleware/debug) packages.

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

Reproduction scripts are in the [benchmarks repo](https://github.com/goceleris/benchmarks).

## API Overview

| Type | Package | Description |
|------|---------|-------------|
| `Server` | `celeris` | Top-level entry point; owns config, router, engine |
| `Config` | `celeris` | Server configuration (addr, engine, protocol, timeouts, body limits) |
| `Context` | `celeris` | Per-request context with params, headers, body, response methods |
| `HandlerFunc` | `celeris` | `func(*Context) error` — handler/middleware signature |
| `HTTPError` | `celeris` | Structured error carrying HTTP status code and message |
| `RouteGroup` | `celeris` | Group of routes sharing a prefix and middleware |
| `Route` | `celeris` | Opaque handle to a registered route |
| `RouteInfo` | `celeris` | Describes a registered route (method, path, handler count) |
| `Cookie` | `celeris` | HTTP cookie for `SetCookie`/`DeleteCookie` |
| `StreamWriter` | `celeris` | Incremental response writer for streaming/SSE |
| `EngineInfo` | `celeris` | Read-only info about the running engine (type + metrics) |
| `Collector` | `observe` | Lock-free request metrics aggregator |
| `Snapshot` | `observe` | Point-in-time copy of all collected metrics |
| `CPUMonitor` | `observe` | CPU utilization sampler (Linux `/proc/stat`, other `runtime/metrics`) |

## Requirements

- **Go 1.26+**
- **Linux** for io_uring and epoll engines
- **Any OS** for the std engine
- Dependencies: `golang.org/x/sys`, `golang.org/x/net`

## Project Structure

```
adaptive/       Adaptive meta-engine (Linux)
celeristest/    Test helpers (NewContext, ResponseRecorder)
engine/         Engine interface + implementations (iouring, epoll, std)
internal/       Shared internals (conn, cpumon, ctxkit, negotiate, platform, sockopts, timer)
observe/        Metrics collector, CPUMonitor, Snapshot
probe/          System capability detection
protocol/       Protocol parsers (h1, h2, detect)
resource/       Configuration, presets, defaults
test/           Conformance, spec compliance, integration, benchmarks
```

## Contributing

```bash
go install github.com/magefile/mage@latest  # one-time setup
mage build   # build all targets
mage test    # run tests with race detector
mage lint    # run linters
mage check   # full verification: lint + test + spec + build
```

Pull requests should target `main`.

## License

[Apache License 2.0](LICENSE)
