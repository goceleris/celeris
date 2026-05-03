# perfmatrix

Release-gate performance matrix for celeris. Benches celeris against every
competitor framework across every protocol, scenario, middleware chain and
driver-backed workload, emits aggregated CSV / Markdown / benchstat output,
and optionally captures pprof per cell.

## Status

Scaffold only (v1.4.1). Interfaces and package structure are stable; server
implementations, scenarios, and the orchestrator body are filled in by
wave-2 and wave-3 agents.

## Why a separate submodule?

The matrix depends on fiber v3, fasthttp, hertz, echo, chi, gin, iris,
gorilla/sessions, pgx v5, go-redis v9, and gomemcache. Keeping these in a
child module (`test/perfmatrix/`) prevents the competitor graph from
infecting the main celeris module — `go get github.com/goceleris/celeris`
stays lean.

A `replace github.com/goceleris/celeris => ../..` directive means the
matrix always benches **this** branch, not a published tag.

## Layout

```
cmd/runner/    orchestrator binary (flag scaffold today)
servers/       framework adapters, one sub-package per framework
scenarios/     benchable workload catalog (static / concurrency / chain / driver)
interleave/    scheduler — emits cells in (run, scenario, server) order
services/      container lifecycle (postgres / redis / memcached)
report/        aggregate + CSV / Markdown / benchstat / pprof-index writers
profiling/     per-cell pprof capture helpers
```

## Running

Invoke the mage targets from the **repo root** (not from inside this
submodule — they wrap the runner binary plus service provisioning):

| Target                  | What it does                                                     | Expected runtime |
|-------------------------|------------------------------------------------------------------|------------------|
| `matrixBench`           | full release-gate: 10 runs × 10s measurement                     | ~2.5 days on msr1 |
| `matrixBenchDeep`       | maximum-rigor: 10 runs × 15s measurement + 3s warmup             | ~3.5 days on msr1 |
| `matrixBenchQuick`      | dev-loop: 3 runs × 5s, static scenarios only                     | ~1 hour          |
| `matrixBenchDrivers`    | driver cells only (pg/redis/memcached/session), 10 runs × 10s    | ~5 hours         |
| `matrixBenchProfile`    | full matrix with pprof capture per cell                          | ~4 days on msr1  |
| `matrixBenchSince`      | HEAD vs `PERFMATRIX_REF`, fails on >2% regression                 | ~5 days on msr1  |

The runner binary (`cmd/runner`) also accepts flags directly for ad-hoc
runs. See `runner -h` or `doc.go` for the flag list.

## Output

Each run lands in `results/<timestamp>-<git-ref>/` with:

- `aggregated.csv` — one row per cell, columns: scenario, server, rps
  (median / p5 / p95), latency percentiles, errors, stddev, n.
- `report.md` — human-readable summary, grouped by scenario category.
- `benchstat.txt` — benchstat-compatible input (pipe through
  `benchstat -row scenario -col server`).
- `profiles/index.html` — pprof capture index (only when `-profile`).

## Cell identifiers

Each cell is uniquely addressed by `<scenarioName>/<serverName>`. Server
names follow `<framework>-<protocol>[-upgrade][+async]`.

Examples:

- `get-json/celeris-iouring-auto-upgrade+async`
- `post-4k/fiber-h1`
- `driver-pg-read/stdhttp-h2c`
- `chain-fullstack-get-json/celeris-epoll-h1`
