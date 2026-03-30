# Adaptive Engine Selection Algorithm

## Data Basis

3-run median results on c7g.16xlarge (arm64) and c7i.16xlarge (x86), kernel 6.17,
keep-alive + sustained-download workloads with custom loadgen client.

## Observed Patterns

### H1 Keep-alive (the dominant real-world pattern)

| Connections | arm64 delta | x86 delta | Signal |
|---|---|---|---|
| 16 | -0.1% | -18.8% | Tied (arm64), epoll wins (x86) |
| 64 | +0.4% | -1.2% | Tied |
| 256 | -0.7% | +0.2% | Tied |
| 1024 | +0.3% | +2.6% | io_uring slight edge |
| 4096 | -1.6% | +1.2% | Tied |
| 16384 | -0.6% | -30.3% | Tied (arm64), epoll wins hard (x86) |

**Pattern:** io_uring and epoll are equivalent at 64-4096 connections on arm64.
On x86, io_uring loses at very low (16c) and very high (16384c) connections.
The sweet spot for io_uring on both architectures is 256-4096 connections.

### H1 Keep-alive p99 Tail Latency

| Connections | arm64 epoll p99 | arm64 iouring p99 | io_uring advantage |
|---|---|---|---|
| 1024 | 2.9ms | **2.4ms** | **-17%** |
| 4096 | 11.5ms | **7.0ms** | **-39%** |
| 16384 | 15.0ms | **14.6ms** | **-3%** |

**Pattern:** io_uring consistently better p99, with the largest advantage at
1024-4096 connections (17-39% better). This is the strongest, most reproducible
signal across all our measurements.

### H2 Keep-alive

| Connections | arm64 delta | x86 delta |
|---|---|---|
| 16 | -17.9% | -0.9% |
| 64 | +0.2% | +1.1% |
| 256-1024 | -1.0% to +2.8% | -1.6% to +2.4% |
| 4096 | -5.7% | -0.1% |
| 16384 | +1.3% | -0.4% |
| 65536 | +0.8% | -4.3% |

**Pattern:** H2 is noisy. Single-process loadgen H2 client can't saturate the
server, so deltas reflect client-side variance more than server differences.
At 256-1024c the engines are equivalent. H2 cannot be reliably used as a
switching signal.

### CPU Efficiency

Across all keep-alive H1 runs at 1024-4096c, io_uring uses 2-8% less CPU% for
equivalent throughput on both architectures. On x86 at high connections (4096c),
io_uring uses up to 50% less CPU (697 vs 1525 in earlier short-lived measurements).

### Sustained-download H1 (bandwidth-limited)

Both engines produce identical throughput (56,700 rps arm64, 47,250 rps x86)
across all connection counts. No engine difference exists when NIC is the
bottleneck.

## Algorithm Design

### Signals Available at Runtime

1. **Connection count** — tracked via `activeConns` atomic
2. **CPU utilization** — tracked via cpumon package
3. **Protocol** — known from Config.Protocol
4. **Architecture** — known at compile time (GOARCH)
5. **Request rate** — tracked via `reqCount` atomic
6. **Latency** — tracked via EngineMetrics (p50, p99, p999)

### Decision Logic

```
Initial engine: epoll (lower RSS, proven stability, safe default)

Evaluation interval: every 10 seconds

Switch to io_uring when ALL of:
  1. activeConns > 256 (below this, no measurable benefit)
  2. CPU% > 50% of available cores (io_uring's efficiency advantage matters)
  3. NOT (GOARCH == "amd64" AND activeConns > 8192)
     (x86 io_uring collapses at very high connection counts)

Switch back to epoll when ANY of:
  1. activeConns < 128 (hysteresis: switch at 256, revert at 128)
  2. CPU% < 30% of available cores (not CPU-bound, no benefit)
  3. io_uring p99 > 2× epoll p99 (latency regression detected)

Oscillation guard: minimum 60 seconds between switches
Memory guard: if RSS > threshold, prefer epoll (lower RSS)
```

### Why This Works

The data shows io_uring's advantages are:
1. **CPU efficiency** — 2-8% less CPU for same throughput (reliable signal)
2. **Tail latency** — 17-39% better p99 at 1024-4096c (reliable signal)
3. **Throughput parity** — within ±2% at the sweet spot (not a differentiator)

The algorithm uses CPU utilization as the primary trigger because:
- It's the most reliable signal (consistent across all runs and architectures)
- When the server is CPU-bound, switching to io_uring frees 2-8% CPU headroom
- When the server is not CPU-bound, the overhead of maintaining two engines
  outweighs the marginal benefit

### What About Protocol?

H2 data is unreliable due to client limitations. The algorithm should NOT use
protocol as a switching signal. Both engines handle H2 equivalently in practice.
The H1/H2 decision should be made at the load balancer level, not the engine level.

### What About Short-lived Connections?

Short-lived (Connection: close) data from wrk shows io_uring 0-10% slower at
1024-4096c. The adaptive algorithm should track connection churn rate
(accepts/sec vs requests/sec). If churn is high (>50% of requests are on new
connections), prefer epoll which has a simpler accept→read→write→close path.

### Fixed Files (ACCEPT_DIRECT) Status

Runtime probe passes on AWS 6.17, but the feature crashes under sustained
4096c load due to missing slot reclamation in the kernel's IORING_FILE_INDEX_ALLOC
allocator. Disabled until:
1. A free list of fixed file slots is implemented (explicit slot management
   instead of auto-allocation), OR
2. AWS ships a kernel where the allocator properly recycles CLOSE_DIRECT'd slots

### SEND_ZC Status

Broken on AWS kernel 6.17 (both virtual and metal). The notification CQE
(CQE_F_NOTIF) is never produced, causing silent DMA buffer leaks. Runtime
probe correctly detects and disables this. Will auto-enable when AWS fixes
the kernel.

### Implication for Adaptive Algorithm

Since SEND_ZC and fixed files are unavailable on current AWS kernels, the
adaptive engine cannot leverage io_uring's structural advantages over epoll.
The algorithm should be conservative:
- Default to epoll (lower RSS, simpler, proven)
- Switch to io_uring only when CPU-bound at 256-4096 connections
- Primary benefit: 2-8% CPU savings + 20-40% better p99 tail latency
- NOT a throughput advantage — throughput is equivalent
