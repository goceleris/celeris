<p align="center">
  <img src=".github/cover.png" alt="celeris — the swift, load-bearing HTTP engine for Go">
</p>

<p align="center">
  <a href="https://github.com/goceleris/celeris/actions/workflows/ci.yml?query=branch%3Amain"><img src="https://github.com/goceleris/celeris/actions/workflows/ci.yml/badge.svg?branch=main" alt="CI status on main"></a>
  <a href="https://codecov.io/gh/goceleris/celeris"><img src="https://codecov.io/gh/goceleris/celeris/graph/badge.svg" alt="Codecov test coverage"></a>
  <a href="https://codspeed.io/goceleris/celeris"><img src="https://img.shields.io/endpoint?url=https://codspeed.io/badge.json" alt="CodSpeed benchmarks"></a>
  <a href="https://scorecard.dev/viewer/?uri=github.com/goceleris/celeris"><img src="https://api.scorecard.dev/projects/github.com/goceleris/celeris/badge" alt="OpenSSF Scorecard"></a>
  <a href="https://github.com/goceleris/celeris/releases/latest"><img src="https://img.shields.io/github/v/release/goceleris/celeris" alt="Latest release"></a>
  <a href="go.mod"><img src="https://img.shields.io/github/go-mod/go-version/goceleris/celeris" alt="Go version from go.mod"></a>
</p>

<p align="center">
  <strong>celeris is a Go HTTP server built on per-core epoll or io_uring event loops, with a route-group and middleware API in the style of Gin and Echo.</strong><br>
  It serves HTTP/1.1 and cleartext HTTP/2, and ships 36 middleware packages and first-party PostgreSQL, Redis and memcached clients (the clients are in beta).
</p>

<p align="center">
  It is for Go developers who want an event-loop server behind a familiar API and run it on Linux. On macOS and Windows it runs on <code>net/http</code>, for development.
</p>

<p align="center">
  <a href="#quick-start">Quick start</a> ·
  <a href="#performance">Performance</a> ·
  <a href="#status-and-stability">Status</a> ·
  <a href="https://goceleris.dev">Documentation</a> ·
  <a href="https://pkg.go.dev/github.com/goceleris/celeris">API reference</a> ·
  <a href="https://github.com/goceleris/celeris/releases/tag/v1.6.0">Release notes</a>
</p>

## Quick start

```
go get github.com/goceleris/celeris@latest
```

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

Requires Go 1.27.4 or newer.

Routes, groups and middleware work as in Gin and Echo. Handlers run inline on the event loop, so mount a route that blocks on I/O (a database or network call) with `.Async()` to keep the loop free:

```go
s := celeris.New(celeris.Config{Addr: ":8080"})
s.Use(recovery.New(), logger.New())

api := s.Group("/api")
api.GET("/users/:id", func(c *celeris.Context) error {
	return c.JSON(200, map[string]string{"id": c.Param("id")})
})
api.GET("/report", reportHandler).Async()
```

**Engines.** On Linux, celeris runs one event loop per CPU on epoll (kernel 5.10 or later) or io_uring (5.19 or later); the default `Adaptive` engine starts on epoll and moves to io_uring under sustained load where the kernel allows it. On other systems, or when you choose `Std`, it runs on `net/http`. [Engines](https://goceleris.dev/docs/engines) has the details.

## Why celeris

- **Event loops, not a goroutine per connection.** Pinned per-core loops on Linux; `Std` on `net/http` elsewhere.
- **A familiar API.** Route groups, named parameters, middleware, error-returning handlers, and `celeris.Adapt` to reuse a `net/http` handler.
- **36 middleware packages in the repository.** Authentication, CORS, sessions, rate limiting, caching, compression, Prometheus and OpenTelemetry ([list](https://goceleris.dev/docs/middleware)).
- **Database clients.** PostgreSQL, Redis and memcached, in beta, with no CGo ([details](https://goceleris.dev/docs/data-stores)).
- **Streaming.** WebSocket with a fan-out `Hub`, Server-Sent Events with a `Broker`, `StreamWriter` and `Detach`.
- **Allocation discipline.** Request contexts are pooled, and the core request paths are held to zero allocations by tests.
- **Small dependency surface.** The root module depends directly on `golang.org/x/sys` and `golang.org/x/net` only.

## Performance

[figure: hero chart, filled from the cut-commit screen]

*Plaintext GET, 1024 connections, requests per second, best configuration per framework, linux/amd64, run of [figure: run date, filled from the cut-commit screen].*

On plaintext GET with 1024 connections on linux/amd64, celeris's best configuration ranks first of [figure: number of frameworks, filled from the cut-commit screen] frameworks: [figure: margin over the next server, filled from the cut-commit screen] ahead of the next server and [figure: ratio, filled from the cut-commit screen] times `net/http`.

| Requests per second, plaintext GET, 1024 connections | celeris, best configuration | next server overall | `net/http` |
|---|---|---|---|
| linux/amd64 | [figure: filled from the cut-commit screen] | [figure: filled from the cut-commit screen] | [figure: filled from the cut-commit screen] |
| linux/arm64 | [figure: filled from the cut-commit screen] | [figure: filled from the cut-commit screen] | [figure: filled from the cut-commit screen] |

Celeris is not ahead everywhere. Tail latency at light load on the loop engines is the known gap (see [Known limits](#known-limits)), and the full results list every row where another server is level or ahead.

The figures come from a cross-framework benchmark on dedicated x86-64 and arm64 machines, driven by one load generator ([loadgen](https://github.com/goceleris/loadgen)) from a separate host. No tail-latency ranking is claimed from these runs: their latency passes carry a known load-generator limit ([loadgen#102](https://github.com/goceleris/loadgen/issues/102)). [How the numbers are made](https://goceleris.dev/methodology/); [full results and how to read them](https://goceleris.dev/docs/results).

## Status and stability

- **Compatibility.** [GOVERNANCE.md](GOVERNANCE.md#compatibility) says: "From v1.6.0, the supported API is every exported identifier of the packages that have no `internal` element in their import path and are not marked experimental [...]. A minor or patch release [...] does not remove or change an exported identifier, a documented default or documented behaviour in a way that breaks code that uses it as documented. A change that would do so needs a new major version." The exported API is checked in CI against the golden files in [`api/`](api/). v1.5.x was not covered by this promise, and v1.6.0 removes and changes API compared with it: see [What's new](#whats-new-in-v160).
- **Database clients (beta).** The exported API of the PostgreSQL, Redis and memcached clients stays under the compatibility check, but behaviour fixes (what a value scans to, which cases return an error) may land in minor releases and are listed in the release notes. Supported types and gaps per client: [PostgreSQL](https://goceleris.dev/docs/postgres-types), [Redis](https://goceleris.dev/docs/redis-replies), [memcached](https://goceleris.dev/docs/memcached-behavior).
- **Maturity.** celeris is a young project with one maintainer ([MAINTAINERS.md](MAINTAINERS.md)). The native engines do not terminate TLS ([#446](https://github.com/goceleris/celeris/issues/446)): run celeris behind a TLS-terminating proxy, and benchmark it on your own workload.
- **Platforms.** Linux is the production platform: io_uring needs kernel 5.19 or later, epoll 5.10 or later. macOS and Windows run the `Std` engine for development and testing ([platform support](https://goceleris.dev/docs/runtime-notes#platform-support)).
- **Testing.** Every pull request runs lint (with the `api/` check), unit, conformance and driver conformance tests, and a build. [probatorium](https://github.com/goceleris/probatorium) separately runs celeris on dedicated x86-64 and arm64 machines against malformed traffic, connection churn and fault injection.

## What's new in v1.6.0

v1.6.0 removes and changes API although it is a minor release. Before you upgrade from v1.5.x, read the upgrade guide at the top of the [release notes](https://github.com/goceleris/celeris/releases/tag/v1.6.0): it lists what stops compiling and what to write instead.

**Code that needs a change**

- Go 1.27.4 or newer is required (v1.5.11 declared 1.27.0).
- The engine, protocol and driver codec packages moved under `internal/` and can no longer be imported. Choose an engine with `Config.Engine`.
- Every deprecated symbol was removed, among them `Async(true)` and `Async(false)` (now `.Async()` and `.Sync()`) and `Context.Stream` (now `StreamReader`). The release notes give each replacement.

**Same code, different behaviour**

- The default request body limit is 4 MiB, down from 100 MB. Set `Config.MaxRequestBodySize` for larger uploads (`-1` removes the limit).
- `HEAD` and `OPTIONS` are answered automatically. `Config.DisableAutoHEAD` and `Config.DisableAutoOPTIONS` turn each off.
- Middleware mounted with `Use` now runs for requests that no route matches. If you mounted `pprof`, `debug`, `metrics` or `static` that way, check its access rule: paths that answered 404 now serve.
- The static middleware no longer serves hidden files (paths with a segment that starts with a dot, such as `.env`).
- On Linux hosts whose CPUs are alike, the native engines start `GOMAXPROCS - max(1, GOMAXPROCS/8)` loops by default (14 on 16 CPUs). Set `Config.Workers` to `max(2, runtime.GOMAXPROCS(0))` for one loop per P as before.
- `SO_BUSY_POLL` is off by default; `Config.SocketBusyPoll` turns it on.
- `basicauth` no longer verifies stored bare SHA-256 digests (re-hash with `HashPasswordPBKDF2`), `compress` now prefers gzip, then zstd, then br when a client lists them with equal weight, and `swagger` serves its UI from assets embedded in the binary.
- `database/sql` through the PostgreSQL driver dials direct connections by default, and `Commit` or `Rollback` of a native `Tx` ends it.

**Fixes you can notice**

- `Shutdown` returns, and runs the `OnShutdown` hooks, only after the drain on every engine.
- HTTP/2 on the native engines: a context derived from `c.Context()` could panic the process, and a frame that arrived as an async handler returned could end it; both are fixed. A response held for a client's flow-control window is bounded, and a 64 MiB response to `x/net`'s `http2.Transport` now completes.
- epoll and io_uring send a large response whole, keep pipelined responses in order, and no longer send a body after the handler has given it back.
- The native engines' timeouts use a monotonic clock, so a wall-clock step no longer closes a connection that is making progress. io_uring checks a connection's `ReadTimeout` or `IdleTimeout` when it falls due.
- With no connection open, an event loop waits 100 ms where it waited 4 ms, so an idle server wakes far less often.
- The root module builds for Windows (the `Std` engine serves there).
- The PostgreSQL, Redis and memcached clients got many fixes, among them wrong results, calls that reported success wrongly, crashes, and hangs. v1.5.x receives no further releases: upgrade to v1.6.0. The release notes list each fix.

**New**

- `Config.GCPercent` is an opt-in garbage-collector target for large-message WebSocket p99 ([how it works](https://goceleris.dev/docs/websocket#large-messages-and-the-garbage-collector)); celeris changes no process-wide setting unless you set it.
- `celeris.Version` reads `"1.6.0"`.

## Known limits

What is still open in v1.6.0, and what a change costs. Each line names its issue.

- **Idle CPU with open connections.** On the native engines a server that holds connections open and is otherwise idle uses more CPU than a plain `net/http` or `Std` server, because each loop keeps its short wait and sweep timer ([#1478](https://github.com/goceleris/celeris/issues/1478)).
- **The first request after an idle period.** On epoll, the first request after a period with no connection open can be answered later than with v1.5.11's 4 ms wait, since an idle loop now waits 100 ms ([#1474](https://github.com/goceleris/celeris/pull/1474)).
- **io_uring idle CPU with many connections.** The deadline check costs a little more idle CPU with 10,000 open connections ([#1494](https://github.com/goceleris/celeris/issues/1494)).
- **io_uring deadline corner cases.** A few deadlines the worker cannot see are still met at the old sweep period, up to about a second after they fall due and never later than in v1.5.11; a fix is planned for v1.6.1 ([#1489](https://github.com/goceleris/celeris/issues/1489), [#1490](https://github.com/goceleris/celeris/issues/1490), [#1491](https://github.com/goceleris/celeris/issues/1491)).
- **Light-load tail latency on the loop engines.** In one WebSocket echo run (async handlers, 64 connections, about 6,000 messages a second, one arm64 container) the round-trip p99 of epoll and adaptive was about twice the `Std` engine's; it is not shown to hold for HTTP in general ([#1380](https://github.com/goceleris/celeris/issues/1380)).
- **`compress` and `Vary`.** An uncompressed response, where no encoding was negotiated, can lack `Accept-Encoding` in `Vary` when the handler set its own `Vary`; v1.5.11 does the same ([#1420](https://github.com/goceleris/celeris/issues/1420)).
- **`Config.Workers` below 2 is refused** at start on every engine, so `runtime.GOMAXPROCS(0)` fails on a one-CPU host; use `max(2, runtime.GOMAXPROCS(0))` ([#1379](https://github.com/goceleris/celeris/issues/1379)).
- **A port passed in a listener can be taken during the hand-over.** A `bind(0)` elsewhere in the process in that gap can take it, and `Listen` returns `address already in use` ([#1404](https://github.com/goceleris/celeris/issues/1404)).
- **A failed epoll start is only partly covered by tests.** The listen-registration failure and the locking of the loop's first descriptor write have no test of their own ([#1377](https://github.com/goceleris/celeris/issues/1377)).
- **A 64 MiB HTTP/2 response on a busy host.** To `x/net`'s `http2.Transport` it completes in about 0.1 s on a quiet machine on the native engines, but while the host is busy with other work an occasional transfer takes seconds, up to about 2.2 s in the measurements ([#1423](https://github.com/goceleris/celeris/issues/1423), [#809](https://github.com/goceleris/celeris/issues/809)).
- **A cancelled PostgreSQL direct-mode query returns after the cancel drain, not at its deadline,** bounded by one second plus thirty ([#1466](https://github.com/goceleris/celeris/issues/1466)).
- **An unread HTTP/2 response can now live longer.** A stream whose client never reads is reset after up to eight `WriteTimeout` windows, where v1.5.11 closed it at `ReadTimeout`; set `WriteTimeout` lower if that is too long for your clients ([#1167](https://github.com/goceleris/celeris/pull/1167)).
- **Stalls on big.LITTLE arm64 hosts.** On an arm64 host with CPUs of different capacity, the loop engines have stalled for over a second under load (io_uring at 512 connections, 2 of 8 runs, in v1.5.11 as well); the cause is not settled ([#1036](https://github.com/goceleris/celeris/issues/1036)).

Database clients (beta), planned for v1.6.1:

- An error from a write does not prove the write was not applied: do not blindly retry `Exec`, `QueryRow` with `RETURNING` or `Commit` in PostgreSQL, or `Incr` and `Decr` in memcached, after a connection error ([#1470](https://github.com/goceleris/celeris/issues/1470), [#1460](https://github.com/goceleris/celeris/issues/1460)).
- PostgreSQL does not support `LISTEN` and `NOTIFY` delivery: the driver has no way to deliver notifications ([#1480](https://github.com/goceleris/celeris/issues/1480)).
- Behind a connection pooler without prepared-statement support, set `auto_cache_statements=false` ([#1462](https://github.com/goceleris/celeris/issues/1462)).
- PostgreSQL on the event-loop path: calls made right after the server closes or resets connections can fail several times in a row with `file descriptor not registered`, for a few milliseconds ([#1465](https://github.com/goceleris/celeris/issues/1465)).
- Redis `DoSlice` on a RESP3 reply of `[member, score]` pairs from a command without `WITHSCORES` or `WITHVALUES` returns an error: use `Do` ([#1353](https://github.com/goceleris/celeris/pull/1353)).

## Graceful shutdown

Use `StartWithContext` for production; when the context is cancelled the server drains in-flight requests up to `ShutdownTimeout` (default 30s).

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

A connection that has begun a request head and not finished it (a fragment, or a bare CRLF) counts as a request in progress on epoll and io_uring. `Shutdown` waits for it until the head completes, until `ReadHeaderTimeout` (10 s by default, from the head's first byte, or for a new connection from its accept) or `WriteTimeout` cuts it, or until the context passed to `Shutdown` is done, so a client can hold a graceful shutdown for that long and the caller's bound is the context's deadline. The std engine differs: std closes a keep-alive connection in that state with the idle ones, and waits for a new connection's first head as for a silent one. The per-engine contract is in the [`Server.Shutdown`](https://pkg.go.dev/github.com/goceleris/celeris#Server.Shutdown) godoc.

## Contributing and governance

Contributions are welcome: see [CONTRIBUTING.md](CONTRIBUTING.md). Roles, how changes get merged, releases and the compatibility promise are in [GOVERNANCE.md](GOVERNANCE.md); the maintainers are listed in [MAINTAINERS.md](MAINTAINERS.md). Related projects: [loadgen](https://github.com/goceleris/loadgen) (the load generator), [probatorium](https://github.com/goceleris/probatorium) (the test suite that runs celeris on dedicated machines) and [docs](https://github.com/goceleris/docs) (the source of [goceleris.dev](https://goceleris.dev)).

## License

[Apache License 2.0](LICENSE)
