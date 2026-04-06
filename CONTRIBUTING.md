# Contributing to Celeris

Thank you for your interest in contributing to celeris!

## Getting Started

1. Fork the repository
2. Clone your fork: `git clone https://github.com/<you>/celeris.git`
3. Install dependencies: `go mod download`
4. Run checks: `mage check`

## Development

### Prerequisites

- Go 1.26+
- [Mage](https://magefile.org) build tool: `go install github.com/magefile/mage@latest`
- Linux (for io_uring/epoll engine tests) or macOS (std engine only)
- [golangci-lint](https://golangci-lint.run/) v2.9+
- [h2spec](https://github.com/summerwind/h2spec) (installed automatically by `mage tools`)

### Build & Test

```bash
mage build     # Verify compilation
mage lint      # Run linters (golangci-lint)
mage test      # Run tests with race detection
mage spec      # Run h2spec + h1spec compliance tests
mage fuzz      # Run fuzz tests (30s default, set FUZZ_TIME to override)
mage check     # Run all checks: lint + test + spec + build
```

### Sub-Module Testing

The `middleware/compress`, `middleware/metrics`, and `middleware/otel` packages have their own `go.mod` files (to isolate heavy external dependencies from the core module). These are tested automatically in CI but require separate commands locally:

```bash
# Root module (includes all middleware except compress, metrics + otel)
go test -race ./...

# Sub-modules (separate go.mod)
cd middleware/compress && go test -race ./...
cd middleware/metrics && go test -race ./...
cd middleware/otel && go test -race ./...
```

### Middleware Development

All middleware lives under `middleware/`. Each package follows a consistent pattern:

- `config.go` — Config struct with defaults, validation (panics on invalid config at init)
- `<name>.go` — Middleware implementation using `New(config ...Config) celeris.HandlerFunc`
- `doc.go` — Package-level godoc with usage examples and security notes
- `<name>_test.go` — Comprehensive tests with race detection
- `bench_test.go` — Benchmarks with `b.ReportAllocs()`
- `example_test.go` — Runnable examples for godoc

When adding middleware, use `celeris.SkipHelper` for skip logic (not manual `skipMap`). Reference implementations by complexity:

- **Simple**: `middleware/recovery`, `middleware/cors` — basic middleware with no shared state
- **Stateful**: `middleware/ratelimit`, `middleware/circuitbreaker` — shared state with atomic counters, background cleanup, sliding windows
- **Response-transform**: `middleware/etag`, `middleware/compress` — BufferResponse/FlushResponse pattern
- **Request-coalescing**: `middleware/singleflight` — embedded singleflight group with response fan-out
- **Path-prefix interception**: `middleware/debug`, `middleware/pprof`, `middleware/swagger`
- **Pre-routing URL manipulation**: `middleware/redirect`, `middleware/rewrite`
- **Utility package**: `middleware/adapters` — conversion functions, not middleware constructor

### Linux Testing from macOS

The io_uring and epoll engines only compile and run on Linux. Use the Multipass VM targets to test from macOS:

```bash
mage testLinux    # Run tests with race detection in a Linux VM
mage specLinux    # Run h2spec + h1spec in a Linux VM
mage checkLinux   # Full verification suite in a Linux VM
mage benchLinux   # Run benchmarks in a Linux VM
```

The VM is created automatically on first use (Ubuntu Noble, 4 CPUs, 4 GB RAM). Use `mage vmStop` / `mage vmDelete` to manage it.

### Benchmarking

```bash
mage localBenchmark       # A/B benchmark: main vs current branch (Multipass VM)
mage localProfile         # Capture pprof profiles for main vs current branch
mage bench                # Run Go benchmarks (local, any OS)
mage middlewareBenchmark  # Compare middleware against Fiber/Echo/Chi/stdlib
mage middlewareProfile    # CPU profile each middleware individually
```

### Available Mage Targets

```bash
mage -l    # List all available targets
```

## Pull Requests

- Keep PRs focused on a single change
- Include tests for new functionality
- Run `mage check` before submitting (or `mage checkLinux` if touching engine code)
- Follow existing code style (enforced by golangci-lint)
- Write clear commit messages following the `type: description` format (e.g., `feat:`, `fix:`, `perf:`, `security:`, `test:`, `chore:`)
- Security-sensitive changes should note the CWE number in the commit message

## Code Style

- Follow standard Go conventions
- No unnecessary comments — code should be self-documenting
- Only add comments where the logic is not self-evident
- Use `mage lint` to verify (runs golangci-lint across darwin + linux)
- Hot-path code: avoid allocations, avoid `defer` for Lock/Unlock, prefer inline fast-paths with fallback to function calls
- Do not add redundant tests that only check constant values or duplicate existing test coverage

## Testing

- Unit tests go in `*_test.go` files alongside the code
- Integration tests (multi-engine, adaptive, etc.) go in `test/integration/`
- Conformance tests (HTTP/1.1 RFC 9112, HTTP/2 h2spec) go in `test/spec/`
- Middleware benchmarks comparing against other frameworks go in `test/benchcmp/`
- Use the `celeristest` package for Context/ResponseRecorder test helpers
- Run with `-race` flag (all CI runs use race detection)

## Reporting Issues

Use GitHub Issues with the provided templates for bug reports and feature requests.

For security vulnerabilities, see [SECURITY.md](SECURITY.md).
