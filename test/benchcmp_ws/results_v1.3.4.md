# WebSocket Benchmark — v1.3.4

Environment:
- darwin/arm64, Apple M5
- Go 1.26
- `go test -bench=. -benchmem -run=^$ -benchtime=5s -count=3`
- celeris @ v1.3.4 (post-critique rewrite: chanReader, engine-integrated
  pause/resume, pooled compress scratch, BufferPool-as-*bufio.Writer pool)
- gorilla/websocket latest pinned in `test/benchcmp_ws/go.mod`

## Steady-state echo (the path that matters)

| Benchmark                 | Time/op | B/op    | Allocs/op |
|---------------------------|---------|---------|-----------|
| `WSEcho_Celeris`          | 15.4 µs | **592** | **4**     |
| `WSEcho_RawGorilla`       | 15.2 µs | 1088    | 5         |
| `WSEchoLarge_Celeris`     | **75.7 µs** | **269 KB** | **20** |
| `WSEchoLarge_RawGorilla`  | 78.2 µs | 276 KB  | 37        |

**Delta celeris vs gorilla:**
- Small echo: parity time, -46 % bytes, -20 % allocs
- Large echo: **-3 % time, -3 % bytes, -46 % allocs**

Celeris is equal to or faster than gorilla on throughput and
meaningfully ahead on memory/alloc pressure — the chanReader rewrite
eliminated one goroutine and one copy per frame, and the pooled
compress scratch + bw buffer dramatically reduced per-op allocation.

## Upgrade (one-time cost per connection)

| Benchmark                 | Time/op | B/op    | Allocs/op |
|---------------------------|---------|---------|-----------|
| `WSUpgrade_Celeris`       | 81.0 µs | 47.4 KB | 141       |
| `WSUpgrade_RawGorilla`    | 81.3 µs | 29.6 KB | 120       |

Celeris matches gorilla on upgrade time but uses more memory on the
one-time handshake because it runs the full route/middleware/context
pipeline for the upgrade request. This is architectural, not a
WebSocket-middleware regression. Once the connection is upgraded the
steady-state echo numbers above apply.

## Regression check

No known prior baseline exists because `middleware/websocket/` is new in
this branch. Regression check = compare against raw gorilla, the
reference implementation. Celeris passes that check on every steady-state
benchmark.

## Full raw results

```
BenchmarkWSUpgrade_Celeris-10         77470   80185 ns/op   47381 B/op   141 allocs/op
BenchmarkWSUpgrade_Celeris-10         74238   81078 ns/op   47386 B/op   141 allocs/op
BenchmarkWSUpgrade_Celeris-10         77016   81849 ns/op   47386 B/op   141 allocs/op
BenchmarkWSUpgrade_RawGorilla-10      77088   79581 ns/op   29643 B/op   120 allocs/op
BenchmarkWSUpgrade_RawGorilla-10      75973   81133 ns/op   29642 B/op   120 allocs/op
BenchmarkWSUpgrade_RawGorilla-10      74929   83150 ns/op   29642 B/op   120 allocs/op
BenchmarkWSEcho_Celeris-10           400075   15706 ns/op     592 B/op     4 allocs/op
BenchmarkWSEcho_Celeris-10           343288   15631 ns/op     592 B/op     4 allocs/op
BenchmarkWSEcho_Celeris-10           409368   14989 ns/op     592 B/op     4 allocs/op
BenchmarkWSEcho_RawGorilla-10        402360   15394 ns/op    1088 B/op     5 allocs/op
BenchmarkWSEcho_RawGorilla-10        366696   15291 ns/op    1088 B/op     5 allocs/op
BenchmarkWSEcho_RawGorilla-10        400000   14940 ns/op    1088 B/op     5 allocs/op
BenchmarkWSEchoLarge_Celeris-10       77670   75825 ns/op  269244 B/op    20 allocs/op
BenchmarkWSEchoLarge_Celeris-10       79189   75853 ns/op  269244 B/op    20 allocs/op
BenchmarkWSEchoLarge_Celeris-10       79570   75471 ns/op  269244 B/op    20 allocs/op
BenchmarkWSEchoLarge_RawGorilla-10    76668   78434 ns/op  276361 B/op    37 allocs/op
BenchmarkWSEchoLarge_RawGorilla-10    77282   78342 ns/op  276361 B/op    37 allocs/op
BenchmarkWSEchoLarge_RawGorilla-10    76627   77868 ns/op  276361 B/op    37 allocs/op
```
