// Package report aggregates per-cell samples and emits the output formats
// consumed by release-gate review: CSV, Markdown, benchstat, and the
// pprof index page.
//
// Aggregation strategy:
//   - For per-run scalars (RPS, errors, bytes/sec) we sort the per-run
//     values and take the sample median + 5th/95th percentiles as the
//     confidence bounds.
//   - For latency percentiles we take the median of each percentile ACROSS
//     runs (median-of-medians). Loadgen only exposes the per-run
//     percentile summary, not the raw histogram, so the overall P99 cannot
//     be computed exactly — median-of-P99s is the conventional workaround.
//
// All statistics are stable under permutation of the input Samples slice
// so running the same cells in a different order produces the same
// aggregate.
package report

import (
	"errors"
	"io"
	"math"
	"sort"
	"time"

	"github.com/goceleris/loadgen"
)

// CellResult is the per-cell collection of samples produced by the
// orchestrator: one [loadgen.Result] per run.
type CellResult struct {
	ScenarioName string
	ServerName   string
	ServerKind   string
	Category     string
	Samples      []loadgen.Result
}

// Percentiles captures the latency percentile snapshot used by
// [CellAggregate]. Values are expressed as time.Duration to match the
// loadgen types and to keep the markdown formatter boundary-free.
type Percentiles struct {
	P50, P90, P99, P999, P9999 time.Duration
	Max                        time.Duration
}

// CellAggregate is the summary statistics for one (scenario, server)
// pair over every run.
type CellAggregate struct {
	ScenarioName string
	ServerName   string
	ServerKind   string
	Category     string
	N            int

	RPSMedian float64
	RPSP5     float64 // 5th percentile bound of the per-run RPS distribution
	RPSP95    float64 // 95th percentile bound of the per-run RPS distribution
	RPSStdDev float64

	LatencyMedian Percentiles

	Errors      int64
	BytesMedian float64
}

// ErrNotImplemented is returned by scaffold stubs that have not yet been
// filled in. Retained for API compatibility with earlier wave stubs.
var ErrNotImplemented = errors.New("perfmatrix/report: not yet implemented")

// CellID returns the canonical key used by Aggregate output maps.
func CellID(scenarioName, serverName string) string {
	return scenarioName + "/" + serverName
}

// Aggregate reduces per-cell samples to summary statistics keyed by
// "<scenarioName>/<serverName>". See the package documentation for the
// aggregation strategy.
func Aggregate(cells []CellResult) map[string]CellAggregate {
	out := make(map[string]CellAggregate, len(cells))
	for _, cell := range cells {
		agg := CellAggregate{
			ScenarioName: cell.ScenarioName,
			ServerName:   cell.ServerName,
			ServerKind:   cell.ServerKind,
			Category:     cell.Category,
			N:            len(cell.Samples),
		}
		if len(cell.Samples) == 0 {
			out[CellID(cell.ScenarioName, cell.ServerName)] = agg
			continue
		}

		rpsVals := make([]float64, 0, len(cell.Samples))
		bytesVals := make([]float64, 0, len(cell.Samples))
		var totalErrors int64
		for _, s := range cell.Samples {
			rpsVals = append(rpsVals, s.RequestsPerSec)
			bytesVals = append(bytesVals, s.ThroughputBPS)
			totalErrors += s.Errors
		}

		agg.RPSMedian = percentile(rpsVals, 50)
		agg.RPSP5 = percentile(rpsVals, 5)
		agg.RPSP95 = percentile(rpsVals, 95)
		agg.RPSStdDev = stddev(rpsVals)
		agg.BytesMedian = percentile(bytesVals, 50)
		agg.Errors = totalErrors
		agg.LatencyMedian = medianLatency(cell.Samples)

		out[CellID(cell.ScenarioName, cell.ServerName)] = agg
	}
	return out
}

// medianLatency computes the per-percentile median across runs. Each run
// contributes one value per percentile; we sort those slices independently
// and take the median of each, yielding a "typical tail" that isn't
// pulled by a single outlier run.
func medianLatency(samples []loadgen.Result) Percentiles {
	n := len(samples)
	if n == 0 {
		return Percentiles{}
	}
	p50 := make([]int64, n)
	p90 := make([]int64, n)
	p99 := make([]int64, n)
	p999 := make([]int64, n)
	p9999 := make([]int64, n)
	maxv := make([]int64, n)
	for i, s := range samples {
		p50[i] = int64(s.Latency.P50)
		p90[i] = int64(s.Latency.P90)
		p99[i] = int64(s.Latency.P99)
		p999[i] = int64(s.Latency.P999)
		p9999[i] = int64(s.Latency.P9999)
		maxv[i] = int64(s.Latency.Max)
	}
	return Percentiles{
		P50:   time.Duration(medianInt64(p50)),
		P90:   time.Duration(medianInt64(p90)),
		P99:   time.Duration(medianInt64(p99)),
		P999:  time.Duration(medianInt64(p999)),
		P9999: time.Duration(medianInt64(p9999)),
		Max:   time.Duration(medianInt64(maxv)),
	}
}

// percentile returns the p-th percentile (0..100) of vals using linear
// interpolation. The input is not modified.
func percentile(vals []float64, p float64) float64 {
	n := len(vals)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return vals[0]
	}
	sorted := make([]float64, n)
	copy(sorted, vals)
	sort.Float64s(sorted)

	if p <= 0 {
		return sorted[0]
	}
	if p >= 100 {
		return sorted[n-1]
	}
	// Linear interpolation between the two surrounding observations.
	rank := p / 100 * float64(n-1)
	lo := int(math.Floor(rank))
	hi := int(math.Ceil(rank))
	if lo == hi {
		return sorted[lo]
	}
	frac := rank - float64(lo)
	return sorted[lo] + frac*(sorted[hi]-sorted[lo])
}

// medianInt64 returns the median of a []int64 (copy-sorted, non-destructive).
func medianInt64(vals []int64) int64 {
	n := len(vals)
	if n == 0 {
		return 0
	}
	sorted := make([]int64, n)
	copy(sorted, vals)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	if n%2 == 1 {
		return sorted[n/2]
	}
	// Even-length average (integer truncation is fine for nanosecond counts).
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

// stddev returns the sample standard deviation (divisor n-1). Returns 0
// for n < 2 so callers can safely format without guarding NaN.
func stddev(vals []float64) float64 {
	n := len(vals)
	if n < 2 {
		return 0
	}
	var mean float64
	for _, v := range vals {
		mean += v
	}
	mean /= float64(n)
	var sumSq float64
	for _, v := range vals {
		d := v - mean
		sumSq += d * d
	}
	return math.Sqrt(sumSq / float64(n-1))
}

// WriteBenchstat is retained for API compatibility with earlier wave
// scaffolds. Not implemented.
func WriteBenchstat(w io.Writer, cells []CellResult) error {
	_ = w
	_ = cells
	return ErrNotImplemented
}
