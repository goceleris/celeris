# Cluster bench orchestration

The `mage cluster*` targets and the playbooks in this directory drive
the celeris perfmatrix bench against the 3-node cluster:

| Host          | Role           | NIC                                                 |
| ------------- | -------------- | --------------------------------------------------- |
| `msa2-server` | bench target   | dual-port X710 SFP+ bond (LAG1, ports 9+11)         |
| `msa2-client` | loadgen        | dual-port X710 SFP+ bond (LAG2, ports 13+15)        |
| `msr1`        | bench target   | dual-port RTL8127 10GBASE-T bond (LAG3, ports 5+7) |

All three nodes are pinned via DHCP reservation behind the QSW-M3216R-8S8T
switch and reachable via Tailscale.

## Prerequisites

On the developer Mac:

```bash
brew install ansible        # if not already installed
```

The cluster nodes need only an SSH key and passwordless sudo for the
`mini` user — both already configured. No agents, no daemons.

## Targets

| Target                            | What it does                                                                                |
| --------------------------------- | ------------------------------------------------------------------------------------------- |
| `mage clusterStatus`              | quick health check (ping + ssh + dep status on all 3 hosts)                                 |
| `mage clusterDeploy`              | cross-compile binaries + push to `/tmp/celeris-bench/`                                      |
| `mage clusterBench`               | single-host bench: runner runs server + loadgen on each bench target                        |
| `mage clusterDistributedBench`    | network-bound bench: server on bench target (msa2-server or msr1), loadgen on msa2-client   |
| `mage clusterCleanup`             | force cleanup if a bench died mid-flight                                                    |

`clusterBench` accepts these env-var knobs:

| Variable             | Default                                          | Purpose                                                |
| -------------------- | ------------------------------------------------ | ------------------------------------------------------ |
| `CLUSTER_TARGETS`    | `both`                                           | `msa2-server`, `msr1`, `both`                          |
| `CLUSTER_RUNS`       | `3`                                              | passes to runner `-runs`                               |
| `CLUSTER_DURATION`   | `5s`                                             | passes to runner `-duration`                           |
| `CLUSTER_WARMUP`     | `1s`                                             | passes to runner `-warmup`                             |
| `CLUSTER_CELLS`      | `get-simple-1024c/*`                             | passes to runner `-cells` (smoke default; format `<scenario>/<server>`)              |
| `CLUSTER_FULL_MATRIX`| `0`                                              | `1` opts into the full sweep (overrides `CELLS`)       |

## Pristine semantics

- All transient state lives under `/tmp/celeris-bench/` on each node.
  This is tmpfs on Ubuntu 26.04, so a reboot wipes it.
- Results land under `/tmp/celeris-results/<ts>-<hostname>/` and are
  fetched back to the dev machine in `results/<ts>-cluster/<hostname>/`.
- Any apt packages we install during bench prep are recorded in
  `/tmp/celeris-bench-manifest.json`. The `always:` cleanup block
  uninstalls them after the bench, leaving the host as-found.
- If you Ctrl-C mid-flight, run `mage clusterCleanup` to force the
  cleanup phase.

## Running

```bash
# Smoke (5s × 3 runs, default cells, both bench hosts in parallel)
mage clusterBench

# Just msa2-server
CLUSTER_TARGETS=msa2-server mage clusterBench

# Just msr1
CLUSTER_TARGETS=msr1 mage clusterBench

# Full release-gate matrix on both hosts (~1.9 days each — be sure!)
CLUSTER_FULL_MATRIX=1 mage clusterBench
```

### Distributed bench (server + loadgen on different hosts)

```bash
# 5s × 64 conns: server (epoll H1 async) on msa2-server, loadgen on msa2-client.
CLUSTER_DIST_TARGET=msa2-server CLUSTER_DIST_DURATION=5s CLUSTER_DIST_CONNS=64 \
  mage clusterDistributedBench

# Same against msr1 with iouring high tier + H2C upgrade
CLUSTER_DIST_TARGET=msr1 \
CLUSTER_DIST_SERVER=celeris-iouring-h2c+upg-async \
CLUSTER_DIST_DURATION=30s \
CLUSTER_DIST_CONNS=512 \
CLUSTER_DIST_H2=true \
  mage clusterDistributedBench
```

| Variable                | Default                          | Purpose                                         |
| ----------------------- | -------------------------------- | ----------------------------------------------- |
| `CLUSTER_DIST_TARGET`   | `msa2-server`                    | bench target host (msa2-server or msr1)         |
| `CLUSTER_DIST_SERVER`   | `celeris-epoll-h1-async`         | perfmatrix-registered server name               |
| `CLUSTER_DIST_PORT`     | `8080`                           | server bind port                                |
| `CLUSTER_DIST_PATH`     | `/`                              | path component of loadgen `-url`                |
| `CLUSTER_DIST_DURATION` | `10s`                            | loadgen `-duration`                             |
| `CLUSTER_DIST_WARMUP`   | `2s`                             | loadgen `-warmup`                               |
| `CLUSTER_DIST_CONNS`    | `256`                            | loadgen `-connections`                          |
| `CLUSTER_DIST_WORKERS`  | `0`                              | loadgen `-workers` (0 = library default)        |
| `CLUSTER_DIST_H2`       | `false`                          | loadgen `-h2` flag                              |

Outputs land in `results/<ts>-cluster/`:
- `<bench_target>/server.log` — celeris server stdout/stderr
- `<bench_target>/server.pid` — PID file (sanity check; will be empty after cleanup)
- `loadgen/loadgen.json` — full loadgen Result (RPS, latency percentiles, timeseries, etc.)
- `loadgen/loadgen.stderr` — loadgen progress lines

## Direct ansible invocation

The mage target is a thin wrapper around:

```bash
cd ansible
ansible-playbook cluster-bench.yml \
  --extra-vars "bench_targets_filter=both" \
  --extra-vars "runner_binary_amd64=/abs/path/runner-amd64" \
  --extra-vars "runner_binary_arm64=/abs/path/runner-arm64" \
  --extra-vars "loadgen_binary_amd64=/abs/path/loadgen-amd64" \
  --extra-vars "bench_cells=*/get-simple-1024c" \
  --extra-vars "bench_runs=3" \
  --extra-vars "bench_duration=5s" \
  --extra-vars "bench_warmup=1s" \
  --extra-vars "results_local_dir=/abs/path/results/local"
```

Use this when debugging — output is more verbose than via mage.
