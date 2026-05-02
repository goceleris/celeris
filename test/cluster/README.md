# Cluster stress matrix

`stress-matrix.sh` loops `mage clusterDistributedBench` across:

- **Engines**: `iouring`, `epoll`, `adaptive`
- **Protocols**: `h1`, `h2c-noupg` (prior knowledge), `h2c+upg` (RFC 9113 §3.2 upgrade), `auto+upg` (hybrid)
- **Bench targets**: `msa2-server` (Intel X710 / amd64), `msr1` (Realtek RTL8127 / arm64)
- **Async only** — sync configs are tested separately by `mage clusterBench`'s single-host pipeline.

= **24 distributed bench cells**. Each cell is a 60-second run with 256 connections (configurable).

For each cell the script captures:

- Loadgen JSON (`loadgen.json`): RPS, p50/p99/p99.99 latency, errors, throughput
- mpstat sample on the bench target (`<target>/cpu.log`)
- mpstat sample on msa2-client (`loadgen/cpu.log`)
- Server stdout/stderr (`<target>/server.log`)

…and emits one row per cell to a `summary.tsv`:

```
target  engine  protocol  async  server_name  rps  p50_us  p99_us  p99_99_us  errors  target_cpu_avg  target_cpu_peak  loadgen_cpu_avg  loadgen_cpu_peak
```

## Running

```bash
# Default: 60s × 256 conns × 24 cells (~30 min total)
bash test/cluster/stress-matrix.sh

# Quick smoke (10s × 64 conns)
STRESS_DURATION=10s STRESS_WARMUP=2s STRESS_CONNS=64 bash test/cluster/stress-matrix.sh

# Run a subset by regex on "target/engine/protocol"
STRESS_RUN_FILTER='msa2-server/iouring' bash test/cluster/stress-matrix.sh
```

| Variable             | Default   | Purpose                                         |
| -------------------- | --------- | ----------------------------------------------- |
| `STRESS_DURATION`    | `60s`     | passed to `mage clusterDistributedBench`        |
| `STRESS_WARMUP`      | `5s`      | warmup before measurement                       |
| `STRESS_CONNS`       | `256`     | loadgen `-connections`                          |
| `STRESS_RUN_FILTER`  | `''`      | regex over `<target>/<engine>/<protocol>`       |

## Pristine semantics

`sysstat` (mpstat) is the only system package the matrix needs that isn't
on a fresh cluster node. The script `apt install`s it once on entry,
sets a `trap` to `apt purge` it on exit, and the per-cell ansible plays
clean their own staging dirs and binaries between runs.

If the script is killed mid-flight, `mage clusterCleanup` restores
pristine state for the cluster-bench manifest. To clean up the
matrix-installed `sysstat` manually:

```bash
ansible cluster -i ansible/inventory.yml -m apt \
  -a "name=sysstat state=absent purge=true autoremove=true" -b
```

## Interpreting CPU columns

`target_cpu_avg/peak` and `loadgen_cpu_avg/peak` are computed from the
per-cell `cpu.log`:

- **avg**: average `%active` (`%usr + %sys + %irq + %soft`) across all
  CPUs over the entire bench window. Excludes `%idle` and `%iowait`.
- **peak**: max `%active` observed on any single CPU during the run.
  Useful for spotting single-CPU saturation that the average hides.

A clean stress-test result looks like:

- `target_cpu_avg` ≥ 80% → bench target is being driven hard
- `loadgen_cpu_avg` ≤ 60% → loadgen has headroom and isn't the bottleneck
- `errors == 0` → no connection failures or timeouts
