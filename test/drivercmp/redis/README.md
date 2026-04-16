# celeris-redis driver benchmarks

Compares `github.com/goceleris/celeris/driver/redis` against
`github.com/redis/go-redis/v9`.

This is a separate Go module so the comparison dependency (`go-redis`) does
not pollute the main celeris module's dependency graph. It pulls celeris in
via a local `replace` directive.

## Running

Start a Redis server (see `../../conformance/redis/docker-compose.yml`), then:

```sh
cd test/drivercmp/redis
go mod tidy
export CELERIS_REDIS_ADDR=127.0.0.1:6379
# Optional: export CELERIS_REDIS_PASSWORD=celeris  (if the server requires AUTH)
go test -bench=. -benchmem -benchtime=3s ./...
```

For statistically meaningful comparisons, run each benchmark at least 10 times
and compare with `benchstat`:

```sh
go test -run=^$ -bench=BenchmarkGet -count=10 ./... > results.txt
benchstat -filter ".name:/(BenchmarkGet_(Celeris|GoRedis))/" results.txt
```

## Exit criteria

These thresholds are *documentation*, not enforced in the benchmark code
(they need `benchstat` + >=10 runs to be statistically meaningful):

| Metric | Threshold |
|---|---|
| `BenchmarkGet` (celeris ns/op) | <= go-redis x 1.10 |
| `BenchmarkPipeline1000` (celeris ns/op) | <= go-redis x 0.90 |
| `BenchmarkGet` / `BenchmarkSet` allocs/op (celeris) | 0 (target matches go-redis's UnsafeMode) |

The single-Get threshold gives celeris a 10% budget over go-redis's tuned
single-RTT path. The Pipeline1000 threshold asks celeris to *beat* go-redis
by 10% — celeris uses a single event-loop writer per FD with no per-request
goroutine handoff, while go-redis spawns a reader goroutine per conn.

CI integration for automatic threshold checking is a TODO; for now, record
results in `benchmark_v140.md` alongside hardware and benchstat output.

## Benchmarks

Mirrored across `celeris_test.go` and `goredis_test.go`:

- `BenchmarkGet_*` - single `GET key` latency floor (populated short value).
- `BenchmarkSet_*` - single `SET key value`.
- `BenchmarkMGet10_*` - `MGET` 10 keys.
- `BenchmarkPipeline{10,100,1000,10000}_*` - N commands in one Exec.
- `BenchmarkPubSub1to1Latency_*` - Publish + receive round-trip on a single
  subscriber. The celeris variant is skipped pending a typed `Publish` helper
  on `*Client` (see follow-up issue in #137).
- `BenchmarkPoolAcquireRelease_*` - PING-equivalent: acquire + release with
  no user work.
- `BenchmarkParallel_Get_*` - `b.RunParallel` wrapping Get.

Each benchmark:

- calls `b.ReportAllocs()`,
- does a short warm-up before `b.ResetTimer()`,
- uses `context.Background()` (no per-call timeout) so context overhead is
  identical across drivers.
