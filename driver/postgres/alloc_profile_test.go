//go:build !race

package postgres

import "testing"

// BenchmarkQuery1col1row exposes the existing benchQuery1col1row helper
// (used by TestAllocBudgets) as a top-level benchmark so `go test -bench
// -memprofile` can capture an allocation profile of the query hot path.
// Used during the v1.4.1 driver-alloc-reduction pass — current
// allocs/op is 16 vs an aspirational budget of 4.
func BenchmarkQuery1col1row(b *testing.B) { benchQuery1col1row(b) }
