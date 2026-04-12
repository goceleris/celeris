# Autobahn fuzzingclient harness

This harness runs the [Autobahn|Testsuite](https://github.com/crossbario/autobahn-testsuite)
`fuzzingclient` against the celeris WebSocket middleware on each engine,
validating RFC 6455 (WebSocket protocol) and RFC 7692 (permessage-deflate)
compliance end-to-end.

## What it tests

`fuzzingclient` runs ~500 cases across:

- **1.x** — Echo cases (text and binary, fragmented and unfragmented)
- **2.x** — Pings and pongs
- **3.x** — Reserved bits and opcodes
- **4.x** — Invalid framing
- **5.x** — Fragmentation handling
- **6.x** — UTF-8 validation
- **7.x** — Close handling
- **9.x** — Performance under large messages
- **10.x** — WebSocket compression negotiation
- **12.x / 13.x** — permessage-deflate edge cases

The acceptance criterion is **0 failures across cases 1.x–13.x** for
each celeris engine.

## Requirements

- Docker (for the Autobahn container — the project no longer ships
  native macOS binaries).
- Go 1.22+ to build the celeris server.
- On Linux, `make autobahn` runs all three engines (std/epoll/io_uring)
  in parallel. On macOS only `std` is exercised.

## Run

```bash
cd test/autobahn
make autobahn        # build server, start engines, run fuzzingclient
open reports/clients/index.html
```

To run a single engine manually:

```bash
make build
./bin/autobahn-server -engine=epoll -addr=:9002 &
docker compose run --rm fuzzingclient
```

## Files

- `server/main.go` — celeris echo server with `-engine` flag
- `fuzzingclient.json` — Autobahn case configuration (all cases, all engines)
- `docker-compose.yml` — wraps `crossbario/autobahn-testsuite`
- `Makefile` — orchestration: build, run, stop, clean
- `reports/` — generated HTML report (gitignored)

## CI integration

The recommended CI flow is:

- **Every PR (Linux runner)**: `make autobahn ENGINE=std` — fast smoke
  on the std engine only.
- **Nightly (Linux runner)**: full `make autobahn` against all three
  engines, posting the report as a build artifact.

The mage target `mage testAutobahn` is the project-level entry point.
