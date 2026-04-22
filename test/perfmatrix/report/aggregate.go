// Package report aggregates per-cell samples and emits the output formats
// consumed by release-gate review: CSV, Markdown, benchstat, and the
// pprof index page.
package report

import (
	"errors"
	"io"

	"github.com/goceleris/loadgen"
)

// CellResult is the per-cell collection of samples produced by the
// orchestrator: one loadgen.Result per run.
type CellResult struct {
	ScenarioName string
	ServerName   string
	Samples      []loadgen.Result
}

// Percentiles captures the latency percentile snapshot used by
// [CellAggregate].
type Percentiles struct {
	P50, P90, P99, P999, P9999 float64
	Max                        float64
}

// CellAggregate is the summary statistics for one (scenario, server)
// pair over every run.
type CellAggregate struct {
	RPSMedian     float64
	RPSP5         float64 // 5th percentile bound of the per-run RPS distribution
	RPSP95        float64 // 95th percentile bound of the per-run RPS distribution
	LatencyMedian Percentiles
	Errors        int64
	StdDev        float64
	N             int
}

// ErrNotImplemented is returned by every scaffold stub in this package.
var ErrNotImplemented = errors.New("perfmatrix/report: not yet implemented")

// Aggregate reduces per-cell samples to summary statistics keyed by a
// stable cell id ("<scenarioName>/<serverName>"). Wave-3 fills in the
// real math (median, p99, stddev, benchstat-compatible output).
func Aggregate(cells []CellResult) map[string]CellAggregate {
	_ = cells
	return map[string]CellAggregate{}
}

// WriteCSV emits an aggregated.csv report. Column order is stable so the
// file diffs cleanly across releases.
func WriteCSV(w io.Writer, agg map[string]CellAggregate) error {
	_ = w
	_ = agg
	return ErrNotImplemented
}

// WriteMarkdown emits report.md, grouped by scenario Category.
func WriteMarkdown(w io.Writer, agg map[string]CellAggregate) error {
	_ = w
	_ = agg
	return ErrNotImplemented
}

// WriteBenchstat emits a benchstat-compatible text file (one
// goos/goarch/pkg header block per scenario, then BenchmarkX-8 lines).
// Lets reviewers pipe the output through benchstat directly.
func WriteBenchstat(w io.Writer, cells []CellResult) error {
	_ = w
	_ = cells
	return ErrNotImplemented
}
