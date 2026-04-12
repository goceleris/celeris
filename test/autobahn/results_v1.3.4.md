# Autobahn|Testsuite Results — v1.3.4

## Summary

**All three celeris WebSocket engines pass the full Autobahn fuzzingclient
suite: 517 / 517 cases, 0 failures.**

| Engine    | Platform              | OK  | NON-STRICT | INFORMATIONAL | FAIL | UNIMPL |
|-----------|-----------------------|----:|-----------:|--------------:|-----:|-------:|
| std       | darwin/arm64 (local)  | 510 | 4          | 3             | **0** | 0      |
| epoll     | linux/arm64 (kernel 6.8, multipass VM) | 510 | 4 | 3 | **0** | 0 |
| io_uring  | linux/arm64 (kernel 6.8, tier=high)    | 510 | 4 | 3 | **0** | 0 |

The 4 NON-STRICT + 3 INFORMATIONAL cases are identical across all three
engines. They are informational only — they do not represent protocol
violations.

## What was tested

`fuzzingclient` runs the full case set (sections 1.x through 13.x of
the Autobahn protocol test corpus):

- **1.x** — Echo (text, binary, fragmented, unfragmented)
- **2.x** — Pings and pongs
- **3.x** — Reserved bits and opcodes
- **4.x** — Invalid framing
- **5.x** — Fragmentation handling
- **6.x** — UTF-8 validation
- **7.x** — Close handling
- **9.x** — Performance under large messages (up to 1 MB)
- **10.x** — Compression negotiation
- **12.x** — permessage-deflate compression, various sizes and params
- **13.x** — permessage-deflate with context takeover variants

## Bug found and fixed

The initial run (before the v1.3.4 rewrite) revealed a real bug in
`truncWriter` (`middleware/websocket/compress.go`): when the flate
writer produced multiple short trailing Writes (typical for
permessage-deflate flush), the previous hold-back window was
overwritten, corrupting the sync-marker strip. Autobahn cases
12.1.4, 12.1.6, 12.1.7, 12.1.8, 12.1.9, 12.1.10 failed with length
or content mismatches.

The fix rewrites `truncWriter.Write` to correctly reassemble the
tail-4 window when the new chunk is shorter than 4 bytes. Two new
regression tests pin the behavior:

- `TestTruncWriterSmallTailWrite` — reproduces the exact 3-byte+2-byte
  sequence that broke v1.3.3.
- `TestTruncWriterMany1ByteWrites` — hammers truncWriter with
  single-byte writes, the most extreme case.

After the fix, all 517 cases pass on all three engines.

## How to reproduce

```bash
# macOS (std engine only):
cd test/autobahn
make build
./bin/autobahn-server -engine=std -addr=:9001 &
docker run --rm --network=host \
  -v $(pwd)/fuzzingclient-std.json:/config/fuzzingclient.json:ro \
  -v $(pwd)/reports:/reports \
  --add-host=host.docker.internal:host-gateway \
  crossbario/autobahn-testsuite:latest \
  wstest -m fuzzingclient -s /config/fuzzingclient.json

# Linux (all three engines):
make autobahn
```

Reports land in `test/autobahn/reports/clients/index.html`.

## Environment

- Autobahn|Testsuite via `crossbario/autobahn-testsuite` Docker image
- Linux runs: multipass Ubuntu 24.04, kernel 6.8.0-106-generic,
  arm64, 4 vCPU, 8 GB RAM, qemu-amd64 emulation for the
  Autobahn container
- io_uring tier: `high` (multishot_accept=true, multishot_recv=true,
  provided_buffers=true, fixed_files=true, send_zc=false — kernel 6.8
  does not expose SEND_ZC support)
