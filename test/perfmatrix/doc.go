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
// # Cell identifiers
//
// Each cell is uniquely addressed by "<scenarioName>/<serverName>".
// Server names follow "<framework>-<protocol>[-upgrade][+async]"; see
// servers/celeris for the full celeris naming scheme.
//
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs
package perfmatrix
