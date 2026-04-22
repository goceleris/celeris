package report

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/loadgen"
)

// TestAggregateMedianAndCI feeds a known distribution and asserts the
// median + 5th/95th percentiles come back at the expected values.
func TestAggregateMedianAndCI(t *testing.T) {
	// 11 samples so P50, P5 and P95 land on data points (no interpolation
	// ambiguity). Sorted RPS: 100..200 step 10. Median = 150. P5 ≈ 105,
	// P95 ≈ 195.
	samples := []float64{150, 140, 160, 130, 170, 120, 180, 110, 190, 100, 200}
	cell := CellResult{
		ScenarioName: "get-json",
		ServerName:   "celeris-std-h1",
		ServerKind:   "celeris",
		Category:     "static",
		Samples:      makeSamples(samples, 0),
	}
	agg := Aggregate([]CellResult{cell})
	if len(agg) != 1 {
		t.Fatalf("Aggregate: want 1 entry, got %d", len(agg))
	}
	c := agg[CellID("get-json", "celeris-std-h1")]
	if c.N != 11 {
		t.Errorf("N: want 11, got %d", c.N)
	}
	if math.Abs(c.RPSMedian-150) > 0.01 {
		t.Errorf("RPSMedian: want 150, got %.2f", c.RPSMedian)
	}
	// Linear interpolation over 11 values at p=5 → rank 0.5 → between
	// 100 and 110 → 105.
	if math.Abs(c.RPSP5-105) > 0.5 {
		t.Errorf("RPSP5: want ~105, got %.2f", c.RPSP5)
	}
	// p=95 → rank 9.5 → between 190 and 200 → 195.
	if math.Abs(c.RPSP95-195) > 0.5 {
		t.Errorf("RPSP95: want ~195, got %.2f", c.RPSP95)
	}
	// Standard deviation of {100..200 step 10} = sqrt(sum((x-150)^2)/10)
	// = sqrt(11000/10) ≈ 33.17.
	if math.Abs(c.RPSStdDev-33.17) > 0.5 {
		t.Errorf("RPSStdDev: want ~33.17, got %.2f", c.RPSStdDev)
	}
}

// TestAggregateLatencyMedianOfMedians checks that per-run P99s come
// through as a median-of-P99s rather than max or mean.
func TestAggregateLatencyMedianOfMedians(t *testing.T) {
	// Three runs with P99 = 1ms, 2ms, 3ms. Median = 2ms.
	samples := []loadgen.Result{
		{RequestsPerSec: 100, Latency: loadgen.Percentiles{
			P50: 100 * time.Microsecond, P99: 1 * time.Millisecond, P999: 2 * time.Millisecond, P9999: 3 * time.Millisecond,
		}},
		{RequestsPerSec: 100, Latency: loadgen.Percentiles{
			P50: 100 * time.Microsecond, P99: 2 * time.Millisecond, P999: 4 * time.Millisecond, P9999: 6 * time.Millisecond,
		}},
		{RequestsPerSec: 100, Latency: loadgen.Percentiles{
			P50: 100 * time.Microsecond, P99: 3 * time.Millisecond, P999: 6 * time.Millisecond, P9999: 9 * time.Millisecond,
		}},
	}
	agg := Aggregate([]CellResult{{ScenarioName: "sc", ServerName: "sv", Samples: samples}})
	c := agg[CellID("sc", "sv")]
	if c.LatencyMedian.P99 != 2*time.Millisecond {
		t.Errorf("P99 median: want 2ms, got %s", c.LatencyMedian.P99)
	}
	if c.LatencyMedian.P999 != 4*time.Millisecond {
		t.Errorf("P999 median: want 4ms, got %s", c.LatencyMedian.P999)
	}
	if c.LatencyMedian.P9999 != 6*time.Millisecond {
		t.Errorf("P9999 median: want 6ms, got %s", c.LatencyMedian.P9999)
	}
}

// TestAggregateErrorsSummed verifies errors are summed across runs.
func TestAggregateErrorsSummed(t *testing.T) {
	samples := []loadgen.Result{
		{RequestsPerSec: 10, Errors: 1},
		{RequestsPerSec: 10, Errors: 2},
		{RequestsPerSec: 10, Errors: 3},
	}
	agg := Aggregate([]CellResult{{ScenarioName: "s", ServerName: "v", Samples: samples}})
	c := agg[CellID("s", "v")]
	if c.Errors != 6 {
		t.Errorf("Errors: want 6, got %d", c.Errors)
	}
}

// TestAggregateEmpty ensures an empty samples slice produces a zero
// aggregate rather than a NaN one.
func TestAggregateEmpty(t *testing.T) {
	cell := CellResult{ScenarioName: "a", ServerName: "b"}
	agg := Aggregate([]CellResult{cell})
	c := agg[CellID("a", "b")]
	if c.N != 0 || c.RPSMedian != 0 || c.RPSStdDev != 0 {
		t.Errorf("empty cell: want zeroed aggregate, got %+v", c)
	}
}

// TestWriteCSVRoundTrip renders a small map and checks the output shape.
func TestWriteCSVRoundTrip(t *testing.T) {
	samples := makeSamples([]float64{100, 200, 300}, 50*time.Microsecond)
	cell := CellResult{
		ScenarioName: "get-json",
		ServerName:   "celeris-std-h1",
		ServerKind:   "celeris",
		Category:     "static",
		Samples:      samples,
	}
	agg := Aggregate([]CellResult{cell})

	var buf bytes.Buffer
	if err := WriteCSV(&buf, agg); err != nil {
		t.Fatalf("WriteCSV: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "scenario\tserver\t") {
		t.Errorf("WriteCSV: missing header; got:\n%s", out)
	}
	if !strings.Contains(out, "get-json\tceleris-std-h1") {
		t.Errorf("WriteCSV: missing cell row; got:\n%s", out)
	}
	// Tab-separated output must not contain double tabs (i.e. no empty
	// fields, every column populated).
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.Contains(line, "\t\t") {
			t.Errorf("WriteCSV: empty field in line %q", line)
		}
	}
}

// TestWriteMarkdownRoundTrip makes sure the generator produces stable
// sections + table rows for a small map.
func TestWriteMarkdownRoundTrip(t *testing.T) {
	samples := makeSamples([]float64{100, 200, 300}, 50*time.Microsecond)
	a := CellResult{
		ScenarioName: "get-json", ServerName: "celeris-std-h1",
		ServerKind: "celeris", Category: "static", Samples: samples,
	}
	b := CellResult{
		ScenarioName: "get-json", ServerName: "fiber-h1",
		ServerKind: "fiber", Category: "static", Samples: makeSamples([]float64{90, 180, 270}, 60*time.Microsecond),
	}
	agg := Aggregate([]CellResult{a, b})
	var buf bytes.Buffer
	meta := Meta{
		GitRef:     "testref",
		StartedAt:  time.Unix(1700000000, 0).UTC(),
		FinishedAt: time.Unix(1700001000, 0).UTC(),
		Host:       "unit-test",
		Runs:       3,
		Duration:   10 * time.Second,
		TotalCells: 2,
	}
	if err := WriteMarkdown(&buf, agg, meta); err != nil {
		t.Fatalf("WriteMarkdown: %v", err)
	}
	md := buf.String()
	for _, want := range []string{
		"# Celeris perfmatrix report — testref",
		"## Summary",
		"HTTP/1 — static scenarios",
		"get-json",
		"celeris-std-h1",
		"fiber-h1",
		"Tail latency",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("WriteMarkdown: missing %q in output:\n%s", want, md)
		}
	}
	// The celeris row should be bolded as the winner (200 rps median vs
	// 180 rps competitor).
	if !strings.Contains(md, "**200.0 rps**") && !strings.Contains(md, "**200 rps**") {
		t.Errorf("WriteMarkdown: winner not bolded in output:\n%s", md)
	}
}

// TestWritePprofIndex exercises the directory-walk path with a fake
// layout of empty .pprof files.
func TestWritePprofIndex(t *testing.T) {
	dir := t.TempDir()
	// Layout: run0/get-json/celeris-std-h1.cpu.pprof + .heap.pprof.
	paths := []string{
		"run0/get-json/celeris-std-h1.cpu.pprof",
		"run0/get-json/celeris-std-h1.heap.pprof",
		"run0/get-json/fiber-h1.cpu.pprof",
	}
	for _, p := range paths {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte("fake"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	var buf bytes.Buffer
	if err := WritePprofIndex(&buf, dir); err != nil {
		t.Fatalf("WritePprofIndex: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"<title>perfmatrix pprof index</title>",
		"run0",
		"get-json",
		"celeris-std-h1",
		"fiber-h1",
		"go tool pprof",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("WritePprofIndex: missing %q in output:\n%s", want, out)
		}
	}
}

// makeSamples synthesises loadgen.Result samples from a set of RPS
// values and a constant latency. Useful for quickly building test
// fixtures.
func makeSamples(rps []float64, latency time.Duration) []loadgen.Result {
	out := make([]loadgen.Result, len(rps))
	for i, r := range rps {
		out[i] = loadgen.Result{
			RequestsPerSec: r,
			ThroughputBPS:  r * 1024,
			Duration:       10 * time.Second,
			Latency: loadgen.Percentiles{
				P50:   latency,
				P90:   latency * 2,
				P99:   latency * 4,
				P999:  latency * 8,
				P9999: latency * 16,
				Max:   latency * 32,
			},
		}
	}
	return out
}
