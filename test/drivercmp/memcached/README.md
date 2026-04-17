# celeris-memcached driver benchmarks

Compares `github.com/goceleris/celeris/driver/memcached` against
`github.com/bradfitz/gomemcache` (the de facto Go memcached library).

This is a separate Go module so the comparison dependency (`gomemcache`)
does not pollute the main celeris module's dependency graph. It pulls
celeris in via a local `replace` directive.

## Running

Start a memcached server (see `../../conformance/memcached/docker-compose.yml`),
then:

```sh
cd test/drivercmp/memcached
go mod tidy
export CELERIS_MEMCACHED_ADDR=127.0.0.1:11211
go test -bench=. -benchmem -benchtime=3s ./...
```

For statistically meaningful comparisons, run each benchmark at least 10 times
and compare with `benchstat`:

```sh
go test -run=^$ -bench=BenchmarkGet -count=10 ./... > results.txt
benchstat -filter ".name:/(BenchmarkGet_(Celeris|GoMemcache))/" results.txt
```

## Benchmarks

Mirrored across the celeris + gomemcache variants:

- `BenchmarkGet_*` - single `get k` latency floor (populated short value).
- `BenchmarkSet_*` - single `set k v`.
- `BenchmarkGetMulti_10_*` - multi-get on 10 keys, all present.
- `BenchmarkParallel_Get_*` - `b.RunParallel` wrapping Get.
- `BenchmarkPoolAcquireRelease_Celeris` - celeris-only: Ping is the cheapest
  command exposed on both drivers, but the gomemcache version has no direct
  equivalent without a subsequent net call.

Each benchmark:

- calls `b.ReportAllocs()`,
- does a short warm-up before `b.ResetTimer()`,
- uses `context.Background()` on celeris (no per-call timeout) so context
  overhead is identical across drivers.

## Fixture invariant

Only one driver writes the read-only fixture (celeris). gomemcache picks up
the same keys opportunistically; if the keys are missing at bench start,
gomemcache falls back to a one-shot celeris seed. This keeps the measurement
focused on read-steady-state behavior instead of intermixing write costs.
