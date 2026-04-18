# celeris-postgres driver benchmarks

Compares `github.com/goceleris/celeris/driver/postgres` against:

- `github.com/jackc/pgx/v5` (both `stdlib` adapter through `database/sql` and the native `pgxpool` API)
- `github.com/lib/pq` (`database/sql` only)

This is a separate Go module so the comparison dependencies (`pgx`, `lib/pq`)
do not pollute the main celeris module's dependency graph. It pulls celeris in
via a local `replace` directive.

## Running

Start a Postgres server (see `../../conformance/postgres/docker-compose.yml`),
then:

```sh
cd test/drivercmp/postgres
go mod tidy
export CELERIS_PG_DSN='postgres://celeris:celeris@localhost:5432/celeristest?sslmode=disable'
go test -bench=. -benchmem -benchtime=3s ./...
```

For statistically meaningful comparisons, run each driver at least 10 times
and compare with `benchstat`:

```sh
go test -run=^$ -bench=BenchmarkSelectOne -count=10 ./... > results.txt
benchstat -filter ".name:/(BenchmarkSelectOne_(Celeris|Pgx))/" results.txt
```

## Exit criteria

These thresholds are *documentation*, not enforced in the benchmark code
(they need `benchstat` + ≥10 runs to be statistically meaningful):

| Metric | Threshold |
|---|---|
| `BenchmarkSelectOne` (celeris) | ≤ pgx × 1.10 |
| `BenchmarkSelect1000Rows_Binary` (celeris) | ≤ pgx × 1.05 |
| `BenchmarkSelectOne` allocs/op (celeris) | ≤ pgx + 2 |

CI integration for automatic threshold checking is a TODO; for now, record
results in `benchmark_v140.md` alongside hardware and benchstat output.

## Benchmarks

Mirrored across `celeris_test.go`, `pgx_test.go`, `libpq_test.go`:

- `BenchmarkSelectOne_*` — `SELECT 1` latency floor.
- `BenchmarkSelect1000Rows_Text_*` — 1000-row result in text format.
- `BenchmarkSelect1000Rows_Binary_*` — 1000-row result in binary format.
  (For `database/sql`-wrapped drivers, the driver decides the format; this
  bench asserts via the driver's chosen encoding, not a client-side toggle.)
- `BenchmarkInsertPrepared_*` — prepared INSERT with 3 parameters.
- `BenchmarkPoolContention_*` — `b.RunParallel` with GOMAXPROCS={1,8,64,512}.
- `BenchmarkCopyIn_1M_Rows_*` — 1M-row bulk COPY (celeris skipped until Pool
  exposes a COPY API; pgx `CopyFrom` is measured).
- `BenchmarkTransactionRoundtrip_*` — BEGIN + SELECT + COMMIT.
- `BenchmarkParallel_QueryContext_*` — `b.RunParallel` SELECT 1.

Each benchmark:

- calls `b.ReportAllocs()`,
- does a short warm-up before `b.ResetTimer()`,
- uses `context.Background()` (no per-call timeout), so the context overhead
  is identical across drivers.
