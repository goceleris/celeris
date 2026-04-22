//go:build mage

package main

import "fmt"

// MatrixBench runs the full release-gate performance matrix: all celeris
// configs × all competitors × all scenarios × 10 interleaved runs at 10s
// each. Expected runtime ~2.5 days on msr1. See test/perfmatrix/README.md.
func MatrixBench() error {
	fmt.Println("matrixBench scaffold — wave-3 agent wires this")
	return nil
}

// MatrixBenchDeep is the maximum-rigor variant: 10 runs × 15s measurement +
// 3s warmup. Captures reliable p99.99 tails. ~3.5 days on msr1. Use for
// major-release gating.
func MatrixBenchDeep() error {
	fmt.Println("matrixBench:deep scaffold — wave-3 agent wires this")
	return nil
}

// MatrixBenchQuick is the dev-loop variant: 3 runs × 5s, static scenarios
// only. ~1 hour.
func MatrixBenchQuick() error {
	fmt.Println("matrixBench:quick scaffold — wave-3 agent wires this")
	return nil
}

// MatrixBenchDrivers runs only the driver-backed cells (pg/redis/memcached/
// session), 10 runs × 10s.
func MatrixBenchDrivers() error {
	fmt.Println("matrixBench:drivers scaffold — wave-3 agent wires this")
	return nil
}

// MatrixBenchProfile runs the full matrix with pprof capture per cell.
// Adds ~12s per cell; expect ~4 days total. Cells filterable via
// PERFMATRIX_CELLS=...
func MatrixBenchProfile() error {
	fmt.Println("matrixBench:profile scaffold — wave-3 agent wires this")
	return nil
}

// MatrixBenchSince runs HEAD and the named ref, produces a diff report,
// fails on >2% regression. Ref via PERFMATRIX_REF=v1.4.0
func MatrixBenchSince() error {
	fmt.Println("matrixBench:since scaffold — wave-3 agent wires this")
	return nil
}
