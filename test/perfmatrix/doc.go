// Package perfmatrix is the release-gate performance matrix for celeris.
//
// It is a self-contained Go submodule (see go.mod) so competitor
// dependencies (fiber v3, fasthttp, hertz, echo, chi, gin, iris, gorilla
// sessions, pgx, go-redis, gomemcache, ...) never leak into the main
// celeris module graph.
//
// # Layout
//
//	cmd/runner/   orchestrator binary
//	servers/      framework adapters, one sub-package per framework
//	scenarios/    benchable workload catalog
//	interleave/   run × scenario × server scheduler
//	services/     container lifecycle (pg/redis/memcached)
//	report/       csv / markdown / benchstat / pprof index writers
//	profiling/    per-cell pprof capture helpers
//
// # Running
//
// Invoke the mage targets from the repo root:
//
//	mage matrixBench          # full release-gate (10 × 10s), ~2.5 days
//	mage matrixBenchDeep      # maximum-rigor (10 × 15s, 3s warmup), ~3.5 days
//	mage matrixBenchStrict    # -race + checkptr + fail-fast (3 × 5s), ~4-8h
//	mage matrixBenchQuick     # dev-loop (3 × 5s, static only), ~1 hour
//	mage matrixBenchDrivers   # driver cells only (10 × 10s)
//	mage matrixBenchProfile   # full matrix with pprof capture per cell
//	mage matrixBenchSince     # HEAD vs PERFMATRIX_REF, fails on >2% regression
//
// matrixBenchStrict is the release-gate confidence check. The runner
// and every in-process engine / server are built with -race and
// -gcflags=-d=checkptr=2; GORACE=halt_on_error=1 + GOTRACEBACK=crash
// + runner -fail-fast abort the moment any data race, use-after-free,
// or invalid unsafe.Pointer conversion surfaces — the bug class that
// otherwise sits buried for hours of churn load before the consequence
// fires.
//
// # Cell identifiers
//
// Each cell is uniquely addressed by "<scenarioName>/<serverName>".
// Server names follow "<framework>-<protocol>[-upgrade][+async]"; see
// servers/celeris for the full celeris naming scheme.
package perfmatrix
