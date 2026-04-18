// bench_test.go — shared benchmark infrastructure and a TestMain gate.
//
// The per-driver benchmarks live in celeris_test.go, pgx_test.go, and
// libpq_test.go. This file only hosts helpers shared across all of them.
//
// TestMain gives us a single early exit if CELERIS_PG_DSN is unset, which
// avoids the per-benchmark skip noise.
package postgres_test

import (
	"os"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	if strings.TrimSpace(os.Getenv(envDSN)) == "" {
		// Run anyway; each benchmark will Skip individually. This keeps
		// `go test ./...` clean even when pg isn't running.
	}
	os.Exit(m.Run())
}
